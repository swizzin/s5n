package discover

// distribution_test.go proves the one fact a teardown needs: which distribution
// installed the cluster, read from the marker its kubelet's version carries, and
// "" when nothing proves it. Every version below is a shape the distributions
// write, not a cluster this test ran against.

import "testing"

func TestDistributionIsTheNodesMarker(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
		want  string
	}{
		{
			name:  "k0s",
			nodes: []Node{{Name: "box", KubeletVersion: "v1.34.1+k0s.0"}},
			want:  K0s,
		},
		{
			name:  "k3s",
			nodes: []Node{{Name: "box", KubeletVersion: "v1.34.1+k3s1"}},
			want:  K3s,
		},
		{
			// Every node has to agree: a teardown for one distribution printed
			// for a cluster that is half another is a command that half works.
			name: "every node agrees",
			nodes: []Node{
				{Name: "a", KubeletVersion: "v1.34.1+k0s.0"},
				{Name: "b", KubeletVersion: "v1.34.1+k0s.1"},
			},
			want: K0s,
		},
		{
			name: "the nodes disagree",
			nodes: []Node{
				{Name: "a", KubeletVersion: "v1.34.1+k0s.0"},
				{Name: "b", KubeletVersion: "v1.34.1+k3s1"},
			},
			want: "",
		},
		{
			// Upstream Kubernetes: no marker at all, which is what a kind node
			// and a hand-installed cluster both report.
			name:  "no marker is no answer",
			nodes: []Node{{Name: "box", KubeletVersion: "v1.34.1"}},
			want:  "",
		},
		{
			// A marker box has no teardown for is not k3s because it starts
			// with a k.
			name:  "a marker box does not know",
			nodes: []Node{{Name: "box", KubeletVersion: "v1.34.1+rke2r1"}},
			want:  "",
		},
		{
			name:  "no nodes at all",
			nodes: nil,
			want:  "",
		},
		{
			// A saved manifest that left the node's status out says nothing,
			// which is a plan rebuilt from manifests rather than a source that
			// was never asked.
			name:  "a node with no version",
			nodes: []Node{{Name: "box"}},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Distribution(tt.nodes); got != tt.want {
				t.Errorf("Distribution = %q, want %q", got, tt.want)
			}
		})
	}
}
