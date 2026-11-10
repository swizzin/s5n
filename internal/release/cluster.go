package release

import "github.com/swizzin/s5n/internal/kube"

// ResolveKubeconfig picks the cluster `box apply` writes to. The rule lives in
// internal/kube (flag, then the first KUBECONFIG entry, then the recorded
// cluster) because it is not apply's rule: bootstrap decided the cluster, and
// root's `box adduser` will resolve one the same way. This is apply's name for
// it.
func ResolveKubeconfig(flag, env string, records kube.Records) (string, error) {
	return kube.ResolveKubeconfig(flag, env, records)
}
