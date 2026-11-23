package bootstrap

// inspect_test.go covers the gate chain asked as a question: the same refusals
// bootstrap stops a fresh install with, over a cluster a migration found already
// running.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExistingClusterRefusalIsBootstrapsOwnGates(t *testing.T) {
	tests := []struct {
		name  string
		probe *fakeProber
		want  string
	}{
		{
			name:  "a single-node cluster box supports",
			probe: &fakeProber{version: Version{GitVersion: "v1.34.1"}, cluster: Cluster{Nodes: 1}},
		},
		{
			name:  "a k0s node",
			probe: &fakeProber{version: Version{GitVersion: "v1.34.1+k0s.0"}, cluster: Cluster{Nodes: 1}},
		},
		{
			name:  "multi-node",
			probe: &fakeProber{version: Version{GitVersion: "v1.34.1"}, cluster: Cluster{Nodes: 3}},
			want:  "3 nodes: box runs one seedbox on one node",
		},
		{
			name:  "a managed control plane",
			probe: &fakeProber{version: Version{GitVersion: "v1.30.5-eks-123abc"}, cluster: Cluster{Nodes: 2}},
			want:  "managed clusters are not supported in v1",
		},
		{
			name:  "no nodes",
			probe: &fakeProber{version: Version{GitVersion: "v1.34.1"}, cluster: Cluster{Nodes: 0}},
			want:  "the cluster lists no nodes",
		},
		{
			name:  "an api that does not answer",
			probe: &fakeProber{versionErr: errors.New("connection refused")},
			want:  "did not answer /version",
		},
		{
			name:  "an identity that cannot manage it",
			probe: &fakeProber{version: Version{GitVersion: "v1.34.1"}, adminErr: errors.New("namespaces is forbidden")},
			want:  "this identity cannot manage it",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := existingClusterRefusal(context.Background(), "/etc/s5n/source.kubeconfig", []byte("kc"), newFakeProber(tt.probe))
			if tt.want == "" {
				if got != "" {
					t.Fatalf("refusal = %q, want the cluster accepted", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("refusal = %q, want it to say %q", got, tt.want)
			}
			// The sentence names the file that answered: a refusal about a
			// cluster is only useful with the kubeconfig it reached.
			if !strings.Contains(got, "/etc/s5n/source.kubeconfig") {
				t.Errorf("refusal does not name the kubeconfig: %q", got)
			}
			// And it is read-only: no capability probe runs behind a gate that
			// already refused.
			if tt.probe.calls["platform"] != 0 {
				t.Errorf("the platform layer was probed behind a refusal")
			}
		})
	}
}
