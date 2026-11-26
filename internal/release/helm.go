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

	"github.com/swizzin/s5n/internal/kube"
	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	helmkube "helm.sh/helm/v4/pkg/kube"
	helmrelease "helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/release/common"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Helm is the release client: the Helm SDK over client-go, namespace-scoped,
// because one release is one namespace is one seedbox.
type Helm struct {
	Kubeconfig string
	Namespace  string
	Release    string
	// Timeout bounds install, upgrade and their wait. Zero means 15 minutes,
	// which is what hack/e2e-kind.sh gives the wiring Job.
	Timeout time.Duration
	// HooksOnly waits for hooks, not resources: `box update`'s health gate is
	// the only one that can tell a crashloop from a slow migration. Helm's wait
	// would spend the whole Timeout on a broken image before a rollback.
	HooksOnly bool
	// SkipHooks installs without the chart's hooks; `box update` sets it for its
	// first apply so its health gate judges the apps before the wiring Job.
	SkipHooks bool

	cfg *action.Configuration
	cs  kubernetes.Interface
}

// waitStrategy is how this handle's install and upgrade wait: for hooks only,
// or for the release and its hooks.
func (h *Helm) waitStrategy() helmkube.WaitStrategy {
	if h.HooksOnly {
		return helmkube.HookOnlyStrategy
	}
	return helmkube.StatusWatcherStrategy
}

// NewHelm builds the client for one namespace. It does not contact the cluster:
// the Helm configuration is lazy, so constructing it is not a read or a write.
func NewHelm(kubeconfig, namespace, releaseName string) (*Helm, error) {
	return NewHelmWithTimeout(kubeconfig, namespace, releaseName, 0)
}

// NewHelmWithTimeout bounds every client-go request: a context deadline bounds
// nothing inside Helm, whose release storage calls with context.Background()
// (see kube.WithRequestTimeout).
func NewHelmWithTimeout(kubeconfig, namespace, releaseName string, timeout time.Duration) (*Helm, error) {
	if namespace == "" {
		return nil, errors.New("no namespace: the grant names the release namespace")
	}
	if releaseName == "" {
		releaseName = ReleaseName
	}
	// Helm builds its clients from this getter, so kube.Flags carries box's
	// warning policy: client-go deprecations would print through klog.
	flags := kube.Flags(kubeconfig, namespace)
	if timeout > 0 {
		flags = kube.WithRequestTimeout(flags, timeout)
	}

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	// Helm's slog output and hook stdout would interrupt box's transcript; drop
	// them, but keep its errors.
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

// Deployed returns the installed values -- for a box release the whole
// effective document -- so a re-apply compares like with like.
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
	// ChartVersion is what an update compares against; Revision is what a failed
	// step returns to.
	ChartVersion string
	Revision     int
	Status       string
}

// State reads the chart version, revision and status: what is running, which an
// update's target and rollback both start from.
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
		// Without it an update cannot say what it moves from, and guessing would
		// step a current box.
		return ReleaseState{}, fmt.Errorf("release %s in %s: Helm does not record which chart version it was deployed with", h.Release, h.Namespace)
	}
	return state, nil
}

// Deployed reports Helm's deployed status; a hook can fail a release after its
// upgrade.
func (s ReleaseState) Deployed() bool {
	return s.Status == string(common.StatusDeployed)
}

// deployedRelease is the deployed release, nil when none. A release in another
// state is an error: box will not unload it behind the operator.
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

// Upgrade installs or upgrades and waits. The install path is Helm's Install
// action, so an existing release cannot be adopted by accident. SkipHooks
// leaves hooks for a later apply, whose gate must move the release first.
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
		up.WaitStrategy = h.waitStrategy()
		up.WaitForJobs = true
		up.DisableHooks = h.SkipHooks
		up.Timeout = timeout
		up.Description = "box apply"
		rel, err = up.RunWithContext(ctx, h.Release, chrt, values)
	} else {
		in := action.NewInstall(h.cfg)
		in.ReleaseName = h.Release
		in.Namespace = h.Namespace
		in.WaitStrategy = h.waitStrategy()
		in.WaitForJobs = true
		in.DisableHooks = h.SkipHooks
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

// Rollback returns to the recorded revision and waits. It takes the step's own
// revision, never "one back", which could land between a retried apply's two.
// Helm v4.3.1's rollback takes no context, so box refuses a cancelled run and
// bounds it with Timeout.
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

// timeout is a Helm action's budget; zero is 15 minutes.
func (h *Helm) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 15 * time.Minute
}

// Receipt reads `<release>-wiring`'s status.json.
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
		Status          string    `json:"status"`
		StartedAt       time.Time `json:"startedAt"`
		DurationSeconds float64   `json:"durationSeconds"`
		Error           string    `json:"error"`
		Steps           []struct {
			Name   string `json:"name"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return Receipt{}, fmt.Errorf("wiring receipt: %w", err)
	}
	out := Receipt{
		Status:    doc.Status,
		StartedAt: doc.StartedAt,
		Duration:  time.Duration(doc.DurationSeconds * float64(time.Second)),
		Error:     doc.Error,
	}
	for _, s := range doc.Steps {
		out.Steps = append(out.Steps, Step{Name: s.Name, Result: s.Result, Detail: s.Detail})
	}
	return out, nil
}

// JobLog reads the wiring Job's log, followed or once. The Job is a hook, not a
// release resource, so its newest pod is found by the controller's label; no pod
// yet is an error the wait retries.
func (h *Helm) JobLog(ctx context.Context, follow bool) (io.ReadCloser, error) {
	job := h.Release + "-wiring"
	pods, err := h.cs.CoreV1().Pods(h.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil {
		return nil, fmt.Errorf("read the %s pods: %w", job, err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		// A finished pod is kept; one being deleted belongs to the next run.
		if p.DeletionTimestamp != nil {
			continue
		}
		if pod == nil || p.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = p
		}
	}
	if pod == nil {
		return nil, fmt.Errorf("no %s pod yet", job)
	}
	stream, err := h.cs.CoreV1().Pods(h.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: wiringName,
		Follow:    follow,
	}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the log of %s: %w", pod.Name, err)
	}
	return stream, nil
}

// configOf reads a release's values; Helm v4 exposes them only on v1's type.
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

// interface checks: Helm is what Applier opens from cmd/box, and the wait reads
// the wiring Job's log through it.
var (
	_ ReleaseClient = (*Helm)(nil)
	_ JobLogs       = (*Helm)(nil)
)
