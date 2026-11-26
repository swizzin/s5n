package bootstrap

import "strings"

// DetectDistribution names the distribution from the server version and cluster
// evidence: the version suffix when present, then a kubeadm-config, then node labels.
// It chooses presets and lets box refuse a managed cluster by name; the chart itself
// is distribution-agnostic.
func DetectDistribution(version string, cluster Cluster) Distribution {
	switch {
	case strings.Contains(version, "+k3s"):
		return DistK3s
	case strings.Contains(version, "+k0s"):
		return DistK0s
	case strings.Contains(version, "-eks-"):
		return DistEKS
	case strings.Contains(version, "-gke."):
		return DistGKE
	case cluster.HasKubeadmConfig:
		return DistKubeadm
	case hasLabel(cluster.NodeLabels, "eks.amazonaws.com/nodegroup"):
		return DistEKS
	case hasLabel(cluster.NodeLabels, "cloud.google.com/gke-nodepool"):
		return DistGKE
	default:
		return DistVanilla
	}
}

func hasLabel(labels map[string]string, key string) bool {
	if labels == nil {
		return false
	}
	_, ok := labels[key]
	return ok
}
