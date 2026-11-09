package bootstrap

import "testing"

func TestDetectDistribution(t *testing.T) {
	cases := []struct {
		name             string
		version          string
		hasKubeadmConfig bool
		labels           map[string]string
		want             Distribution
	}{
		{"k3s suffix", "v1.37.2+k3s1", false, nil, DistK3s},
		{"k0s suffix", "v1.37.2+k0s.1", false, nil, DistK0s},
		{"eks suffix", "v1.30.5-eks-123abc", false, nil, DistEKS},
		{"gke suffix", "v1.29.2-gke.100", false, nil, DistGKE},
		{"kubeadm configmap", "v1.30.0", true, nil, DistKubeadm},
		{"eks node label", "v1.30.0", false, map[string]string{"eks.amazonaws.com/nodegroup": "ng"}, DistEKS},
		{"gke node label", "v1.30.0", false, map[string]string{"cloud.google.com/gke-nodepool": "pool"}, DistGKE},
		{"vanilla", "v1.30.0", false, map[string]string{"kubernetes.io/os": "linux"}, DistVanilla},
		{"no evidence", "v1.30.0", false, nil, DistVanilla},
		// suffix is authoritative over any other evidence
		{"k3s suffix beats kubeadm config", "v1.37.2+k3s1", true, nil, DistK3s},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectDistribution(tc.version, Cluster{
				HasKubeadmConfig: tc.hasKubeadmConfig,
				NodeLabels:       tc.labels,
			})
			if got != tc.want {
				t.Errorf("DetectDistribution(%q, …) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}
