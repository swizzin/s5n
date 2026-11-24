package release

import (
	helmkube "helm.sh/helm/v4/pkg/kube"

	"github.com/swizzin/s5n/internal/kube"
)

// Helm names the field manager of everything it applies after the running
// binary's file name unless told otherwise, so a box run as box-linux-amd64 and
// a later one run as box would be two owners of the same fields, and server-side
// apply refuses the second. Every Helm write box makes is box's, under the one
// name its own applies use.
func init() {
	helmkube.ManagedFieldsManager = kube.FieldManager
}
