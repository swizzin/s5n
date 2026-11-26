package release

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// helmWithReleases is a client over the real accessor and storage, in memory.
func helmWithReleases(t *testing.T) *Helm {
	t.Helper()
	cs := k8sfake.NewClientset()
	cfg := new(action.Configuration)
	cfg.Releases = storage.Init(driver.NewSecrets(cs.CoreV1().Secrets("testbox")))
	cfg.SetLogger(slog.DiscardHandler)
	return &Helm{Namespace: "testbox", Release: ReleaseName, cfg: cfg, cs: cs}
}

// storedRelease writes one record at a revision with a status.
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

func TestHelmStateRefusesAFailedRelease(t *testing.T) {
	h := helmWithReleases(t)
	storedRelease(t, h, 2, common.StatusFailed, "0.4.0")

	_, err := h.State(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not deployed") {
		t.Fatalf("State = %v, want a refusal naming the failed release", err)
	}
}

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

func TestNewHelmWithTimeoutBoundsEveryRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := `apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: not-a-real-token
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"status's bounded read", 10 * time.Second, 10 * time.Second},
		{"a run with a wait of its own", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := NewHelmWithTimeout(path, "testbox", ReleaseName, c.timeout)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := h.cfg.RESTClientGetter.ToRESTConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Timeout != c.want {
				t.Fatalf("request timeout = %v, want %v", cfg.Timeout, c.want)
			}
		})
	}
}
