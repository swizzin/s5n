package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"runtime"

	"github.com/swizzin/s5n/internal/release"
)

// This file is box's k0s policy: the conditions under which box may touch a
// host at all, and the release it installs there, with the checksum of that
// release's asset. Neither is compiled in: the version is the k0s pin of the
// chart release box pulled, and its checksums sit beside it in that release's
// charts/s5n/platform.yaml, so a k0s bump is a chart release and box is not
// rebuilt for it.
//
// It is separate from the sequence in installer.go because every value here is
// a decision that is expensive to get wrong. The binary box writes runs as root
// and registers a system service, so the checksum cannot be the one served
// beside the archive: that only proves both left the same place, not that
// either is what the release says it is. The pinned sums come from each
// release's published sha256sums.txt and are checked against the downloaded
// assets before they land (hack/k0s-sums --check runs in CI), so moving them is
// a deliberate edit that CI refuses to let go wrong.

const (
	// K0sBinaryPath is where box puts the single k0s binary, and therefore
	// where the systemd unit k0s writes points.
	K0sBinaryPath = "/usr/local/bin/k0s"
	// K0sStateDir is k0s's state: its PKI, the admin kubeconfig, and once
	// started, containerd's and the kubelet's data. Its presence is how box
	// tells "k0s is already here" from "clean host".
	K0sStateDir = "/var/lib/k0s"
	// K0sConfigPath is the k0s config box writes on the install path: k0s's
	// own default path, which k0s reads even when nobody passes --config, so
	// the file box writes is the only place the API address is pinned. box
	// refuses to overwrite one it did not write.
	K0sConfigPath = "/etc/k0s/k0s.yaml"
	// k0sReleaseBase is where the k0s project publishes release assets.
	k0sReleaseBase = "https://github.com/k0sproject/k0s/releases/download"
)

// k0sRelease is one pinned build: the release, the architecture it was built
// for, and the checksum box requires of it.
type k0sRelease struct {
	Version string
	Arch    string
	SHA256  string
}

// k0sReleaseFor is one release, for one architecture, taken from the pins of
// the release in hand: the version and the sum of its asset are one pin, in one
// file, so they cannot describe different releases. An architecture the file
// pins no sum for is refused by name; installing it unchecked is the one thing
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

// URL is the release asset. The tag carries a '+', which a path segment may
// hold literally (only a query string reads it as a space), so the tag goes in
// as published.
func (r k0sRelease) URL() string {
	return k0sReleaseBase + "/" + url.PathEscape(r.Version) + "/k0s-" + r.Version + "-" + r.Arch
}

// k0sPlan is what box records as the install plan on the clean-host branch: the
// release's pin for this binary's architecture, the affirmative install choice
// that the installer requires, and the node identity the installer pins. The
// checksum is the one the same pins carry beside the version, so the plan and
// the file cannot name different releases. A host whose release pins no sum for
// this architecture still gets a plan naming the version; the installer refuses
// it there, on the missing sum, before anything is downloaded.
//
// The node IP is a value the decision resolved from the host, not something the
// plan guesses: the installer refuses a plan that carries none, so box cannot
// install a node whose address nothing pinned.
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

// k0sNodeConfig is the minimal k0s config box installs with. It exists for one
// field: spec.api.address.
//
// Left unset, k0s fills the API address with iface.FirstPublicAddress(), the
// first non-loopback IPv4 in the kernel's interface order -- whatever
// net.Interfaces() lists first, which on a host with a tunnel up can be the
// tunnel (k0s pkg/apis/k0s/v1beta1/api.go setDefaults, and
// internal/pkg/iface/iface.go). k0s writes that address into the admin
// kubeconfig box records, and binds the API server to it, so a cluster whose
// API address sits on a tunnel is only reachable while the tunnel is. There is
// no `k0s install` flag for it, so the config file is the mechanism; k0s
// merges this document into its defaults, so nothing else has to be written
// down.
func k0sNodeConfig(nodeIP string) string {
	return fmt.Sprintf(`# Written by box when it installed k0s. The API address is pinned to the
# uplink so it is not whatever address the kernel lists first; the node's
# own address is pinned with --kubelet-extra-args=--node-ip.
apiVersion: k0s.k0sproject.io/v1beta1
kind: ClusterConfig
metadata:
  name: k0s
spec:
  api:
    address: %s
`, nodeIP)
}

// installRefusal is the whole "may box touch this host" question, answered
// from the plan and from read-only host evidence. Every refusal happens before
// the download, so a run that stops here has written nothing and fetched
// nothing.
//
// The order is the order of the mistakes: no privilege is box being run wrong;
// no clean-host choice is box being asked to install by a path that did not
// decide to; k0s already on the host is the one case where installing would
// destroy something box did not create; and an unpinned node, or one whose
// address sits on a tunnel, is box being asked to install a cluster every pod
// would talk through badly.
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

// k0sConfigConflict refuses a k0s config at the install path that box did not
// write. k0s reads /etc/k0s/k0s.yaml by default, so a file there is somebody's
// decision about this host: box can neither install around it (k0s would merge
// it) nor overwrite it. A file whose bytes are exactly what box writes is box's
// own, left by a run that failed before it could start k0s, and it is reused.
func k0sConfigConflict(path, want string) string {
	got, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if string(got) == want {
		return ""
	}
	return fmt.Sprintf("%s already exists and box did not write it: box installs k0s with a config that pins the API address, and it will not overwrite a k0s config it did not write. Move that file aside, or remove it and run bootstrap again.", path)
}
