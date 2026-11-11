package bootstrap

import (
	"fmt"
	"net/url"
	"runtime"

	"github.com/swizzin/s5n/internal/release"
)

// This file is box's k0s policy: the checksums of the k0s release box installs,
// for each architecture, and the conditions under which box may touch a host at
// all. The release itself is not here: it is the k0s pin of the chart release
// box pulled (charts/s5n/platform.yaml), so a k0s bump is a chart release and
// box is not rebuilt for it.
//
// It is separate from the sequence in installer.go because every value here is
// a decision that is expensive to get wrong. The binary box writes runs as root
// and registers a system service, so the checksum cannot be the one served
// beside the archive: that only proves both left the same place, not that
// either is what box intended to run. The sums below come from each release's
// published sha256sums.txt and were confirmed by hashing the downloaded assets;
// changing them is a deliberate edit, not something a run can drift into.

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

// k0sSHA256 is the sha256 of each k0s release box may install, per GOARCH, as
// published in that release's sha256sums.txt.
//
// Which of these is installed is not decided here: bootstrap installs the k0s
// the chart release pins, so a bump is one line of the chart. The sums stay
// compiled in, because they are what makes the download of a root-running
// binary trustworthy: box refuses a release it carries no sum for, and an
// architecture it carries no sum for, rather than downloading something
// unchecked. A k0s bump therefore lands as the chart line plus this map, each
// line copied from that release's own sha256sums.txt after hashing the asset.
var k0sSHA256 = map[string]map[string]string{
	"v1.37.2+k0s.1": {
		"amd64": "4770cf89ff45e43292819f6e3a02a25a453bc750c777f6f7ad2de335f3f41df5",
		"arm64": "d04426041ef9ac002b957fbf43fc36dd736398147db7589fad53687753bedd52",
	},
}

// k0sRelease is one pinned build: the release, the architecture it was built
// for, and the checksum box requires of it.
type k0sRelease struct {
	Version string
	Arch    string
	SHA256  string
}

// k0sReleaseFor is one release, for one architecture. A version box carries no
// checksum for is refused by name: installing it unchecked is the one thing
// this file exists to prevent.
func k0sReleaseFor(version, arch string) (k0sRelease, error) {
	if version == "" {
		return k0sRelease{}, fmt.Errorf("the chart release pins no k0s version: box installs the k0s its release names in charts/s5n/platform.yaml")
	}
	sums, ok := k0sSHA256[version]
	if !ok {
		return k0sRelease{}, fmt.Errorf("k0s %s is not a release this box carries a checksum for: box verifies the k0s it installs, so a k0s pinned ahead of box is one a box update has to bring", version)
	}
	sum, ok := sums[arch]
	if !ok {
		return k0sRelease{}, fmt.Errorf("k0s %s is not published for %q: box installs amd64 or arm64", version, arch)
	}
	return k0sRelease{Version: version, Arch: arch, SHA256: sum}, nil
}

// URL is the release asset. The tag carries a '+', which a path segment may
// hold literally (only a query string reads it as a space), so the tag goes in
// as published.
func (r k0sRelease) URL() string {
	return k0sReleaseBase + "/" + url.PathEscape(r.Version) + "/k0s-" + r.Version + "-" + r.Arch
}

// k0sPlan is what box records as the install plan on the clean-host branch: the
// release's pin for this binary's architecture, plus the affirmative install
// choice that the installer requires. A host with no verified sum still gets a
// plan naming the release's version; the installer refuses it there, on the
// architecture or on the missing checksum.
func k0sPlan(pins release.Versions) InstallPlan {
	plan := InstallPlan{Version: pins.K0s, Arch: runtime.GOARCH, CleanHost: true}
	if rel, err := k0sReleaseFor(pins.K0s, runtime.GOARCH); err == nil {
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
