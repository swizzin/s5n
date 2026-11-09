package bootstrap

import (
	"fmt"
	"net/url"
	"runtime"
)

// This file is box's k0s policy: the one release box installs, the checksum of
// that release's binary for each architecture, and the conditions under which
// box may touch a host at all.
//
// It is separate from the sequence in installer.go because every value here is
// a decision that is expensive to get wrong. The binary box writes runs as root
// and registers a system service, so the checksum cannot be the one served
// beside the archive: that only proves both left the same place, not that
// either is what box intended to run. The pins below come from the release's
// published sha256sums.txt and were confirmed by hashing the downloaded assets;
// changing them is a deliberate edit, not something a run can drift into.

// K0sVersion is the k0s release box installs on a clean host. It is the newest
// stable release, not the version in an old test fixture: k8s 1.31 is out of
// support. The chart's kubeVersion is a floor (>=1.30), so a newer cluster
// needs no chart change.
const K0sVersion = "v1.37.2+k0s.1"

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

// k0sSHA256 is the sha256 of the release asset, per GOARCH, as published in
// that release's sha256sums.txt. Only architectures with a verified pin may be
// installed: an architecture missing here is refused, never downloaded and
// trusted unchecked.
var k0sSHA256 = map[string]string{
	"amd64": "4770cf89ff45e43292819f6e3a02a25a453bc750c777f6f7ad2de335f3f41df5",
	"arm64": "d04426041ef9ac002b957fbf43fc36dd736398147db7589fad53687753bedd52",
}

// k0sRelease is one pinned build: the release, the architecture it was built
// for, and the checksum box requires of it.
type k0sRelease struct {
	Version string
	Arch    string
	SHA256  string
}

func k0sReleaseFor(arch string) (k0sRelease, error) {
	sum, ok := k0sSHA256[arch]
	if !ok {
		return k0sRelease{}, fmt.Errorf("k0s %s is not published for %q: box installs amd64 or arm64", K0sVersion, arch)
	}
	return k0sRelease{Version: K0sVersion, Arch: arch, SHA256: sum}, nil
}

// URL is the release asset. The tag carries a '+', which a path segment may
// hold literally (only a query string reads it as a space), so the tag goes in
// as published.
func (r k0sRelease) URL() string {
	return k0sReleaseBase + "/" + url.PathEscape(r.Version) + "/k0s-" + r.Version + "-" + r.Arch
}

// k0sPlan is what box records as the install plan on the clean-host branch: the
// pin for this binary's architecture, plus the affirmative install choice that
// the installer requires. A host with no verified pin still gets a plan naming
// the version; the installer refuses it on the architecture.
func k0sPlan() InstallPlan {
	plan := InstallPlan{Version: K0sVersion, Arch: runtime.GOARCH, CleanHost: true}
	if rel, err := k0sReleaseFor(runtime.GOARCH); err == nil {
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
