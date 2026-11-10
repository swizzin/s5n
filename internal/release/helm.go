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
	"helm.sh/helm/v4/pkg/kube"
	helmrelease "helm.sh/helm/v4/pkg/release"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	kubeconfigPath := kubeconfig
	ns := namespace
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kubeconfigPath
	flags.Namespace = &ns

	// client-go's default warning handler prints deprecation notices through
	// klog, which would put a line like "I1107 ... Endpoints is deprecated" in
	// the middle of box's own output. box is the whole interface (docs/box.md),
	// so Kubernetes noise is dropped here rather than explained away. Helm's own
	// warnings go through slog to stderr and are unaffected.
	rest.SetDefaultWarningHandler(rest.NoWarnings{})

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, ns, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	// Helm's own slog output and hook stdout would land in the middle of
	// box's transcript. box is the whole interface (docs/box.md), and the
	// reporter is the only thing that speaks: Helm's noise is dropped, not
	// explained away. Its errors still come back through the API calls.
	cfg.SetLogger(slog.DiscardHandler)
	cfg.HookOutputFunc = func(string, string, string) io.Writer { return io.Discard }
	rest, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(rest)
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
	rel, err := h.cfg.Releases.Deployed(h.Release)
	if err == nil {
		config, err := configOf(rel)
		if err != nil {
			return nil, false, err
		}
		if config == nil {
			config = map[string]any{}
		}
		return config, true, nil
	}
	if !errors.Is(err, driver.ErrNoDeployedReleases) && !errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, false, fmt.Errorf("read release %s in %s: %w", h.Release, h.Namespace, err)
	}

	// Nothing deployed. A release of that name in any other state (failed,
	// pending) is not a clean slate: Helm would refuse the install with
	// "cannot re-use a name that is still in use", and uninstalling a failed
	// release behind the operator's back is not box's call. Say so instead.
	hist, herr := h.cfg.Releases.History(h.Release)
	if herr == nil && len(hist) > 0 {
		return nil, false, fmt.Errorf("a release named %s exists in %s but is not deployed (status %q); fix or remove it with helm, then apply again",
			h.Release, h.Namespace, lastStatus(hist))
	}
	return nil, false, nil
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
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	_, exists, err := h.Deployed(ctx)
	if err != nil {
		return 0, err
	}

	var rel helmrelease.Releaser
	if exists {
		up := action.NewUpgrade(h.cfg)
		up.Namespace = h.Namespace
		up.WaitStrategy = kube.StatusWatcherStrategy
		up.WaitForJobs = true
		up.Timeout = timeout
		up.Description = "box apply"
		rel, err = up.RunWithContext(ctx, h.Release, chrt, values)
	} else {
		in := action.NewInstall(h.cfg)
		in.ReleaseName = h.Release
		in.Namespace = h.Namespace
		in.WaitStrategy = kube.StatusWatcherStrategy
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
