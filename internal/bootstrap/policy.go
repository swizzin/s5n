package bootstrap

import (
	"fmt"
	"net/url"
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
// release's pin for this binary's architecture, plus the affirmative install
// choice that the installer requires. The checksum is the one the same pins
// carry beside the version, so the plan and the file cannot name different
// releases. A host whose release pins no sum for this architecture still gets a
// plan naming the version; the installer refuses it there, on the missing sum,
// before anything is downloaded.
func k0sPlan(pins release.Versions) InstallPlan {
	plan := InstallPlan{Version: pins.K0s, Arch: runtime.GOARCH, CleanHost: true}
	if rel, err := k0sReleaseFor(pins, runtime.GOARCH); err == nil {
		plan.SHA256 = rel.SHA256
	}
	return plan
}

// installRefusal is the whole "may box touch this host" question, answered
// from the plan and from read-only host evidence. Every refusal happens before
// the download, so a run that stops here has written nothing and fetched
// nothing.
//
// The order is the order of the mistakes: no privilege is box being run wrong;
// no clean-host choice is box being asked to install by a path that did not
// decide to; k0s already on the host is the one case where installing would
// destroy something box did not create.
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
	return ""
}
