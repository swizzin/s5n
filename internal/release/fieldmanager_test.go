package release

import (
	"testing"

	helmkube "helm.sh/helm/v4/pkg/kube"

	"github.com/swizzin/s5n/internal/kube"
)

// TestHelmAppliesUnderBoxsOwnFieldManager: the manager does not follow the
// binary's file name, so a box installed by the downloaded asset and updated by
// the installed one stays one owner.
func TestHelmAppliesUnderBoxsOwnFieldManager(t *testing.T) {
	if helmkube.ManagedFieldsManager != kube.FieldManager {
		t.Fatalf("Helm's field manager = %q, want %q", helmkube.ManagedFieldsManager, kube.FieldManager)
	}
}
