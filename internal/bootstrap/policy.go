package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// This file is box's k0s policy: when box may touch a host, and the release it installs
// with that asset's checksum. Both are the chart release's pins, so a k0s bump is a
// chart release.
//
// The binary box writes runs as root and registers a service, so the checksum cannot be
// the one served beside the archive. The pinned sums come from each release's
// sha256sums.txt and are checked before they land (hack/k0s-sums --check, in CI).

const (
	// K0sBinaryPath is where box puts the k0s binary, and where its unit points.
	K0sBinaryPath = "/usr/local/bin/k0s"
	// K0sStateDir is k0s's state: PKI, admin kubeconfig, containerd's and the
	// kubelet's data. Its presence tells "k0s is already here" from "clean host".
	K0sStateDir = "/var/lib/k0s"
	// K0sConfigPath is the k0s config box writes: k0s's default path, which it reads
	// without --config, so this file is the only place the API address is pinned. box
	// refuses to overwrite one it did not write.
	K0sConfigPath = "/etc/k0s/k0s.yaml"
	// k0sPodCIDR and k0sServiceCIDR are the cluster's networks, written into that
	// config so the installation and the firewall rule read one copy. They are k0s's
	// own defaults (pkg/apis/k0s/v1beta1/network.go, DefaultNetwork).
	k0sPodCIDR     = "10.244.0.0/16"
	k0sServiceCIDR = "10.96.0.0/12"
	// k0sReleaseBase is where the k0s project publishes release assets.
	k0sReleaseBase = "https://github.com/k0sproject/k0s/releases/download"
)

// k0sRelease is one pinned build: release, architecture, required checksum.
type k0sRelease struct {
	Version string
	Arch    string
	SHA256  string
}

// k0sReleaseFor is one release for one architecture, from the release in hand: the
// version and the sum are one pin, so they cannot describe different releases. An
// architecture with no pinned sum is refused by name; installing unchecked is what
// this file exists to prevent.
func k0sReleaseFor(pins release.Versions, arch string) (k0sRelease, error) {
	if pins.K0s == "" {
		return k0sRelease{}, fmt.Errorf("the chart release pins no k0s version: box installs the k0s its release names in charts/s5n/platform.yaml")
	}
	sum, ok := pins.K0sSHA256[arch]
	if !ok || sum == "" {
		return k0sRelease{}, fmt.Errorf("k0s %s has no checksum for %q: box installs amd64 or arm64, and verifies the one it installs against the sum the chart pins beside the version", pins.K0s, arch)
	}
	return k0sRelease{Version: pins.K0s, Arch: arch, SHA256: sum}, nil
}

// URL is the release asset. The tag's '+' may sit literally in a path segment, so
// the tag goes in as published.
func (r k0sRelease) URL() string {
	return k0sReleaseBase + "/" + url.PathEscape(r.Version) + "/k0s-" + r.Version + "-" + r.Arch
}

// k0sPlan is the clean-host install plan: the pin for this architecture, the affirmative
// choice, the node identity. A host whose release pins no sum still gets a plan; the
// installer refuses it on the missing sum before downloading.
func k0sPlan(pins release.Versions, up Uplink) InstallPlan {
	plan := InstallPlan{
		Version:    pins.K0s,
		Arch:       runtime.GOARCH,
		CleanHost:  true,
		NodeIP:     up.Address,
		Uplink:     up,
		ConfigPath: K0sConfigPath,
	}
	if rel, err := k0sReleaseFor(pins, runtime.GOARCH); err == nil {
		plan.SHA256 = rel.SHA256
	}
	if up.Address != "" {
		plan.ConfigText = k0sNodeConfig(up.Address)
	}
	return plan
}

// k0sNodeConfig is the minimal config box installs with, for two values k0s would choose.
//
// spec.api.address: unset, k0s fills it with iface.FirstPublicAddress(), the first
// non-loopback IPv4 in interface order, which can be a tunnel (pkg/apis/k0s/v1beta1/api.go
// setDefaults, internal/pkg/iface/iface.go) -- and that address goes into the admin
// kubeconfig and binds the API server. No `k0s install` flag exists, so this is the
// mechanism.
//
// spec.network: written here so the firewall rule trusts what was installed, with no
// second copy to disagree; k0sNetworks reads it back.
func k0sNodeConfig(nodeIP string) string {
	return fmt.Sprintf(`# Written by box when it installed k0s. The API address is pinned to the
# uplink so it is not whatever address the kernel lists first; the node's
# own address is pinned with --kubelet-extra-args=--node-ip. The two networks
# are here so the host's firewall rule trusts what the cluster uses.
apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
metadata:
  name: k0s
spec:
  api:
    address: %s
  network:
    podCIDR: %s
    serviceCIDR: %s
`, nodeIP, k0sPodCIDR, k0sServiceCIDR)
}

// k0sNetworks reads the two networks back out of the config box wrote: a reader for
// the one document this package generates, not a YAML parser. It refuses a file
// without both, because the file at that path is box's own (k0sConfigConflict) and
// one without them is a firewall box cannot settle.
func k0sNetworks(config []byte) (pod, service string, err error) {
	var keys []string
	var indents []int
	for _, line := range strings.Split(string(config), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " \t"))
		for len(indents) > 0 && indents[len(indents)-1] >= n {
			indents = indents[:len(indents)-1]
			keys = keys[:len(keys)-1]
		}
		key, value, ok := splitKV(trimmed)
		if !ok {
			return "", "", fmt.Errorf("the k0s config has a line that is not a key: %q", trimmed)
		}
		switch strings.Join(append(append([]string{}, keys...), key), ".") {
		case "spec.network.podCIDR":
			pod = unquote(value)
		case "spec.network.serviceCIDR":
			service = unquote(value)
		}
		if value == "" {
			keys = append(keys, key)
			indents = append(indents, n)
		}
	}
	if pod == "" || service == "" {
		return "", "", fmt.Errorf("the k0s config carries no podCIDR and serviceCIDR under spec.network, so box does not know which networks to trust")
	}
	return pod, service, nil
}

// installRefusal is "may box touch this host", before the download, so a run that stops
// here wrote and fetched nothing.
//
// The order is the order of the mistakes: no privilege, no clean-host choice, k0s
// already present (the one case where installing destroys), an unpinned node or one on
// a tunnel.
func installRefusal(plan InstallPlan, root bool, findings []Finding) string {
	if !root {
		return "box installs k0s as root: run it with sudo (or as root)"
	}
	if !plan.CleanHost {
		return "box did not choose to install a cluster on this host: refusing to touch it"
	}
	if len(findings) > 0 {
		return fmt.Sprintf("k0s is already on this host (%s): box never installs over a k0s it did not create. Use the cluster that is there, or remove k0s first with `k0s stop; k0s reset`", formatFindings(findings))
	}
	if plan.NodeIP == "" {
		return "box installs k0s with the node IP pinned to this host's uplink, and this plan pins none: a node that registers on a tunnel gives every pod that tunnel's MTU"
	}
	if plan.ConfigText == "" {
		return "box installs k0s with a config that pins the API address, and this plan carries none: box writes the config it installs with, so it never has to guess one"
	}
	if plan.Uplink.Tunnel {
		return tunnelRefusal(plan.Uplink)
	}
	return ""
}

// k0sConfigConflict refuses a config at the install path that box did not write: k0s
// reads /etc/k0s/k0s.yaml by default, so a file there is somebody's decision, and box
// can neither install around it (k0s merges it) nor overwrite it. Exactly box's own
// bytes are reused, left by a run that failed before starting k0s.
func k0sConfigConflict(path, want string) string {
	got, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if string(got) == want {
		return ""
	}
	// A refusal is the fact, the one command that fixes it, and what happens next
	// (rule 11); the why is the log's.
	return fmt.Sprintf("%s exists\n$ sudo mv %s %s.before-box\nthen run this again", path, path, path)
}
