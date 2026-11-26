package release

import (
	helmkube "helm.sh/helm/v4/pkg/kube"

	"github.com/swizzin/s5n/internal/kube"
)

// Helm names its field manager after the binary's file name, so box-linux-amd64
// and box would be two owners and the second apply would be refused. Every Helm
// write is under box's one name.
func init() {
	helmkube.ManagedFieldsManager = kube.FieldManager
}
