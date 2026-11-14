package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	helmkube "helm.sh/helm/v4/pkg/kube"
	helmrelease "helm.sh/helm/v4/pkg/release"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
)

// Helm is the release client: the Helm SDK over client-go, which the maintainer
// decided is box's cluster client. It is namespace-scoped, because one release
// is one namespace is one seedbox.
type Helm struct {
	Kubeconfig string
	Namespace  string
	Release    string
	// Timeout bounds install, upgrade and their wait. Zero means 15 minutes,
	// which is what hack/e2e-kind.sh gives the wiring Job.
	Timeout time.Duration

	cfg *action.Configuration
	cs  kubernetes.Interface
}

// NewHelm builds the client for one namespace. It does not contact the cluster:
// the Helm configuration is lazy, so constructing it is not a read or a write.
func NewHelm(kubeconfig, namespace, releaseName string) (*Helm, error) {
	if namespace == "" {
		return nil, errors.New("no namespace: the grant names the release namespace")
	}
	if releaseName == "" {
		releaseName = ReleaseName
	}
	// Helm builds its own clients from this getter, so kube.Flags is what
	// carries box's warning policy into them: client-go's deprecation notices
	// would otherwise print through klog in the middle of box's output
	// (docs/box.md: box is the whole interface). Helm's own warnings go
	// through slog and are silenced below.
	flags := kube.Flags(kubeconfig, namespace)

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	// Helm's own slog output and hook stdout would land in the middle of
	// box's transcript. box is the whole interface (docs/box.md), and the
	// reporter is the only thing that speaks: Helm's noise is dropped, not
	// explained away. Its errors still come back through the API calls.
	cfg.SetLogger(slog.DiscardHandler)
	cfg.HookOutputFunc = func(string, string, string) io.Writer { return io.Discard }
	restConfig, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	return &Helm{Kubeconfig: kubeconfig, Namespace: namespace, Release: releaseName, cfg: cfg, cs: cs}, nil
}

// Deployed returns the values the release was installed with. Helm stores the
// values box handed it, which for a box-applied release is the whole effective
// document (chart defaults, the user's values, then the grant), so the diff of
// a re-apply compares like with like.
func (h *Helm) Deployed(_ context.Context) (map[string]any, bool, error) {
	rel, err := h.deployedRelease()
	if err != nil || rel == nil {
		return nil, false, err
	}
	config, err := configOf(rel)
	if err != nil {
		return nil, false, err
	}
	if config == nil {
		config = map[string]any{}
	}
	return config, true, nil
}

// ReleaseState is the deployed release as `box update` reads it: the chart
// version it is running, the revision a failed step rolls back to, and Helm's
// own status. Exists is false when no release of box's name is deployed.
type ReleaseState struct {
	Exists bool
	// ChartVersion is the version of the chart the release was deployed with.
	// It is what an update compares a newer release against, and the revision
	// is what a failed step returns to.
	ChartVersion string
	Revision     int
	Status       string
}

// State reads the deployed release's chart version, revision and status
// without its values. Deployed answers "what did the operator ask for"; this
// answers "what is running", which is the question an update's target and its
// rollback both start from.
func (h *Helm) State(_ context.Context) (ReleaseState, error) {
	rel, err := h.deployedRelease()
	if err != nil {
		return ReleaseState{}, err
	}
	if rel == nil {
		return ReleaseState{}, nil
	}
	acc, err := helmrelease.NewAccessor(rel)
	if err != nil {
		return ReleaseState{}, fmt.Errorf("read release %s in %s: %w", h.Release, h.Namespace, err)
	}
	state := ReleaseState{Exists: true, Revision: acc.Version(), Status: acc.Status()}
	switch ch := acc.Chart().(type) {
	case *chartv2.Chart:
		if ch.Metadata != nil {
			state.ChartVersion = ch.Metadata.Version
		}
	case chartv2.Chart:
		if ch.Metadata != nil {
			state.ChartVersion = ch.Metadata.Version
		}
	}
	if state.ChartVersion == "" {
		// Without it an update cannot say what it is moving from, and
		// guessing "the newest" would step a box that is already current.
		return ReleaseState{}, fmt.Errorf("release %s in %s: Helm does not record which chart version it was deployed with", h.Release, h.Namespace)
	}
	return state, nil
}

// deployedRelease is the release Helm has deployed, or nil when there is none.
// A release of that name in any other state (failed, pending) is an error: it
// is not a clean slate, and unloading it behind the operator's back is not
// box's call.
func (h *Helm) deployedRelease() (helmrelease.Releaser, error) {
	rel, err := h.cfg.Releases.Deployed(h.Release)
	if err == nil {
		return rel, nil
	}
	if !errors.Is(err, driver.ErrNoDeployedReleases) && !errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, fmt.Errorf("read release %s in %s: %w", h.Release, h.Namespace, err)
	}
	hist, herr := h.cfg.Releases.History(h.Release)
	if herr == nil && len(hist) > 0 {
		return nil, fmt.Errorf("a release named %s exists in %s but is not deployed (status %q); fix or remove it with helm, then apply again",
			h.Release, h.Namespace, lastStatus(hist))
	}
	return nil, nil
}

// Upgrade installs or upgrades the release and waits for it. The install path
// is Helm's Install action rather than Upgrade with Install=true, so the choice
// is visible in the code and an unexpected existing release cannot be adopted
// by accident.
func (h *Helm) Upgrade(ctx context.Context, chart Chart, values map[string]any) (int, error) {
	chrt, err := chart.open()
	if err != nil {
		return 0, fmt.Errorf("load chart %s: %w", chart.Ref, err)
	}
	timeout := h.timeout()
	_, exists, err := h.Deployed(ctx)
	if err != nil {
		return 0, err
	}

	var rel helmrelease.Releaser
	if exists {
		up := action.NewUpgrade(h.cfg)
		up.Namespace = h.Namespace
		up.WaitStrategy = helmkube.StatusWatcherStrategy
		up.WaitForJobs = true
		up.Timeout = timeout
		up.Description = "box apply"
		rel, err = up.RunWithContext(ctx, h.Release, chrt, values)
	} else {
		in := action.NewInstall(h.cfg)
		in.ReleaseName = h.Release
		in.Namespace = h.Namespace
		in.WaitStrategy = helmkube.StatusWatcherStrategy
		in.WaitForJobs = true
		in.Timeout = timeout
		in.Description = "box apply install"
		rel, err = in.RunWithContext(ctx, chrt, values)
	}
	if err != nil {
		return 0, fmt.Errorf("apply %s in %s: %w", h.Release, h.Namespace, err)
	}
	acc, err := helmrelease.NewAccessor(rel)
	if err != nil {
		return 0, fmt.Errorf("apply %s in %s: %w", h.Release, h.Namespace, err)
	}
	return acc.Version(), nil
}

// Rollback returns the release to an earlier revision and waits for it.
//
// It is what a failed update step does, so it takes the revision the step
// recorded, never "one back": an update can make more than one revision (the
// per-host and login steps are object writes, not revisions, but a retried
// apply is), and "one back" could land between them.
//
// Helm v4.3.1's rollback takes no context, so box refuses to start one on a
// cancelled run and bounds it with the same Timeout as an upgrade rather than
// pretending the context reaches it.
func (h *Helm) Rollback(ctx context.Context, revision int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if revision <= 0 {
		return fmt.Errorf("roll back %s in %s: %d is not a revision box recorded", h.Release, h.Namespace, revision)
	}
	rb := action.NewRollback(h.cfg)
	rb.Version = revision
	rb.WaitStrategy = helmkube.StatusWatcherStrategy
	rb.WaitForJobs = true
	rb.Timeout = h.timeout()
	rb.Description = "box update rollback"
	if err := rb.Run(h.Release); err != nil {
		return fmt.Errorf("roll back %s in %s to revision %d: %w", h.Release, h.Namespace, revision, err)
	}
	return nil
}

// timeout is how long a Helm action may take. Zero is 15 minutes, which is
// what hack/e2e-kind.sh gives the wiring Job.
func (h *Helm) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 15 * time.Minute
}

// Receipt reads the `<release>-wiring` ConfigMap's status.json, which
// internal/wiring publishes on every run.
func (h *Helm) Receipt(ctx context.Context) (Receipt, error) {
	cm, err := h.cs.CoreV1().ConfigMaps(h.Namespace).Get(ctx, h.Release+"-wiring", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Receipt{}, ErrNoReceipt
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("read wiring receipt: %w", err)
	}
	raw, ok := cm.Data["status.json"]
	if !ok || strings.TrimSpace(raw) == "" {
		return Receipt{}, ErrNoReceipt
	}
	var doc struct {
		Status    string    `json:"status"`
		StartedAt time.Time `json:"startedAt"`
		Error     string    `json:"error"`
		Steps     []struct {
			Name   string `json:"name"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return Receipt{}, fmt.Errorf("wiring receipt: %w", err)
	}
	out := Receipt{Status: doc.Status, StartedAt: doc.StartedAt, Error: doc.Error}
	for _, s := range doc.Steps {
		out.Steps = append(out.Steps, Step{Name: s.Name, Result: s.Result, Detail: s.Detail})
	}
	return out, nil
}

// configOf returns a release's own values. Helm v4 exposes the user-supplied
// values only on the concrete v1 release type; the accessor interface is
// deliberately smaller.
func configOf(rel helmrelease.Releaser) (map[string]any, error) {
	switch r := rel.(type) {
	case *v1release.Release:
		return r.Config, nil
	case v1release.Release:
		return r.Config, nil
	default:
		return nil, fmt.Errorf("unexpected release type %T", rel)
	}
}

func lastStatus(hist []helmrelease.Releaser) string {
	last := ""
	best := -1
	for _, r := range hist {
		acc, err := helmrelease.NewAccessor(r)
		if err != nil {
			continue
		}
		if acc.Version() > best {
			best, last = acc.Version(), acc.Status()
		}
	}
	if last == "" {
		return "unknown"
	}
	return last
}

// interface check: Helm is what Applier opens from cmd/box.
var _ ReleaseClient = (*Helm)(nil)
