package release

import "github.com/swizzin/s5n/internal/kube"

// ResolveKubeconfig is apply's name for internal/kube's rule (flag, first
// KUBECONFIG entry, then the record); bootstrap and adduser resolve the same.
func ResolveKubeconfig(flag, env string, records kube.Records) (string, error) {
	return kube.ResolveKubeconfig(flag, env, records)
}
