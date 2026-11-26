package release

import (
	"testing"

	helmkube "helm.sh/helm/v4/pkg/kube"

	"github.com/swizzin/s5n/internal/kube"
)

func TestHelmAppliesUnderBoxsOwnFieldManager(t *testing.T) {
	if helmkube.ManagedFieldsManager != kube.FieldManager {
		t.Fatalf("Helm's field manager = %q, want %q", helmkube.ManagedFieldsManager, kube.FieldManager)
	}
}
