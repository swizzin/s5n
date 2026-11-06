package bootstrap

import "strings"

// DetectDistribution names the Kubernetes distribution from the server version
// and cluster evidence. The version suffix is authoritative when present; then
// a kubeadm-config ConfigMap; then node labels. The answer only chooses
// presets — the chart itself is distribution-agnostic.
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
