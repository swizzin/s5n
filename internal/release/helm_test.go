package release

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// helmWithReleases is a Helm client over an in-memory release store: the real
// accessor and the real storage code, with no API server.
func helmWithReleases(t *testing.T) *Helm {
	t.Helper()
	cs := k8sfake.NewClientset()
	cfg := new(action.Configuration)
	cfg.Releases = storage.Init(driver.NewSecrets(cs.CoreV1().Secrets("testbox")))
	cfg.SetLogger(slog.DiscardHandler)
	return &Helm{Namespace: "testbox", Release: ReleaseName, cfg: cfg, cs: cs}
}

// storedRelease writes one release record at a revision with a status.
func storedRelease(t *testing.T, h *Helm, revision int, status common.Status, chartVersion string) {
	t.Helper()
	rel := &v1release.Release{
		Name:      h.Release,
		Namespace: h.Namespace,
		Version:   revision,
		Info:      &v1release.Info{Status: status},
		Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{
			Name:       "s5n",
			Version:    chartVersion,
			APIVersion: "v2",
		}},
	}
	if err := h.cfg.Releases.Create(rel); err != nil {
		t.Fatalf("store release: %v", err)
	}
}

// TestHelmStateNamesTheInstalledChart: an update moves from the chart version
// the release was deployed with, and rolls back to the revision recorded before
// its step. Both come from Helm's own record, never from the newest published
// release or from box's idea of what it installed.
func TestHelmStateNamesTheInstalledChart(t *testing.T) {
	h := helmWithReleases(t)

	state, err := h.State(context.Background())
	if err != nil {
		t.Fatalf("State on an empty store: %v", err)
	}
	if state.Exists {
		t.Fatalf("State = %+v, want no deployed release", state)
	}

	storedRelease(t, h, 3, common.StatusDeployed, "0.4.0")
	state, err = h.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Exists || state.ChartVersion != "0.4.0" || state.Revision != 3 || state.Status != string(common.StatusDeployed) {
		t.Fatalf("State = %+v, want 0.4.0 at revision 3, deployed", state)
	}
}

// TestHelmStateRefusesAFailedRelease: a release left failed is not one an
// update may step, the same way apply refuses to install over it.
func TestHelmStateRefusesAFailedRelease(t *testing.T) {
	h := helmWithReleases(t)
	storedRelease(t, h, 2, common.StatusFailed, "0.4.0")

	_, err := h.State(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not deployed") {
		t.Fatalf("State = %v, want a refusal naming the failed release", err)
	}
}

// TestHelmRollbackRefusesARevisionItCannotTrust: a zero revision is Helm's
// "one back", which is not what update recorded; a cancelled run must not
// start a rollback it cannot stop.
func TestHelmRollbackRefusesARevisionItCannotTrust(t *testing.T) {
	h := helmWithReleases(t)
	if err := h.Rollback(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "not a revision") {
		t.Fatalf("Rollback(revision 0) = %v, want a refusal", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Rollback(ctx, 2); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Rollback on a cancelled run = %v, want the cancellation", err)
	}
}
