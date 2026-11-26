package platform

import (
	helmkube "helm.sh/helm/v4/pkg/kube"

	"github.com/swizzin/s5n/internal/kube"
)

// Helm names the field manager of what it applies after the running binary unless told
// otherwise, so box run as box-linux-amd64 and later as box would be two owners of one
// field and server-side apply would refuse the second. Every Helm write is box's, under
// the name its own applies use.
func init() {
	helmkube.ManagedFieldsManager = kube.FieldManager
}
