package discover

// distribution.go says which distribution installed the source cluster, from the
// one place a cluster states it: the marker every distribution writes into its
// kubelet's version. k0s appends `+k0s.0`, k3s appends `+k3s1`, and upstream
// Kubernetes appends nothing.
//
// It exists because a teardown has to name the thing that is actually there.
// `box migrate cutover --replace-cluster` prints the reset for the operator to
// double-check, and a k0s recipe run on a k3s host removes nothing while looking
// like it removed something.

import "strings"

// The distributions whose marker box knows, and whose teardown it can name.
const (
	K0s = "k0s"
	K3s = "k3s"
)

// Distribution is the distribution that installed the source cluster, or "" when
// box cannot tell: the nodes disagree, no node carries a marker, or the marker is
// one box has no teardown for. A caller prints its own wording for "" rather than
// picking a distribution to guess at.
func Distribution(nodes []Node) string {
	dist := ""
	for _, node := range nodes {
		got := distributionOf(node.KubeletVersion)
		if got == "" || (dist != "" && got != dist) {
			return ""
		}
		dist = got
	}
	return dist
}

// distributionOf is the one node's marker: the build metadata a distribution adds
// to the kubelet's version, matched on its name so `+k0s.0` and `+k3s1` are both
// read and `+rke2r1` is not read as k3s.
func distributionOf(kubeletVersion string) string {
	i := strings.IndexByte(kubeletVersion, '+')
	if i < 0 {
		return ""
	}
	marker := kubeletVersion[i+1:]
	for _, dist := range []string{K0s, K3s} {
		if strings.HasPrefix(marker, dist) {
			return dist
		}
	}
	return ""
}
