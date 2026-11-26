package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	helmkube "helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	helmrelease "helm.sh/helm/v4/pkg/release"
	repo "helm.sh/helm/v4/pkg/repo/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/kube"
)

// Helm is the Releases implementation: the Helm SDK over client-go, for the platform's
// own releases. Charts are fetched at the pinned version at run time; only box's own
// manifests are embedded. Both chart fetches carry fetch.Transport, so an upstream 5xx
// is retried.
type Helm struct {
	Kubeconfig string
	// Timeout bounds one install or upgrade, its wait included; zero means five
	// minutes.
	Timeout time.Duration
	// Retry is the chart fetches' policy; zero is box's (four attempts, 1s/2s/4s).
	Retry fetch.Policy

	configs map[string]*action.Configuration
}

// NewHelm builds the client without contacting the cluster: Helm's configuration is
// lazy.
func NewHelm(kubeconfig string) *Helm {
	return &Helm{Kubeconfig: kubeconfig, configs: map[string]*action.Configuration{}}
}

// configuration returns the Helm configuration for one namespace: Helm stores one
// release record per namespace.
func (h *Helm) configuration(namespace string) (*action.Configuration, error) {
	if cfg, ok := h.configs[namespace]; ok {
		return cfg, nil
	}
	flags := kube.Flags(h.Kubeconfig, namespace)

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	// Same rule as `box apply`: Helm's slog output and hook stdout are dropped, never
	// mixed into box's transcript.
	cfg.SetLogger(slog.DiscardHandler)
	cfg.HookOutputFunc = func(string, string, string) io.Writer { return io.Discard }
	h.configs[namespace] = cfg
	return cfg, nil
}

// Version returns a deployed release's chart version and whether box's label is on
// it. A release in another state (failed, pending) is an error, not something to
// install over: removing it silently is not box's call.
func (h *Helm) Version(_ context.Context, name, namespace string) (string, bool, bool, error) {
	cfg, err := h.configuration(namespace)
	if err != nil {
		return "", false, false, err
	}
	rel, err := cfg.Releases.Deployed(name)
	if err != nil {
		if errors.Is(err, driver.ErrNoDeployedReleases) || errors.Is(err, driver.ErrReleaseNotFound) {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf("read release %s in %s: %w", name, namespace, err)
	}
	acc, err := helmrelease.NewAccessor(rel)
	if err != nil {
		return "", false, false, fmt.Errorf("read release %s in %s: %w", name, namespace, err)
	}
	owned := acc.Labels()[kube.ManagedByLabel] == kube.ManagedBy
	switch c := acc.Chart().(type) {
	case *chartv2.Chart:
		if c.Metadata != nil {
			return c.Metadata.Version, owned, true, nil
		}
	}
	return "", owned, false, fmt.Errorf("release %s in %s: the stored chart has no version", name, namespace)
}

// Install installs or upgrades one release and waits for its workloads.
func (h *Helm) Install(ctx context.Context, spec Release) error {
	cfg, err := h.configuration(spec.Namespace)
	if err != nil {
		return err
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	chrt, err := h.chart(spec)
	if err != nil {
		return err
	}

	if h.exists(cfg, spec.Name) {
		up := action.NewUpgrade(cfg)
		up.Namespace = spec.Namespace
		up.WaitStrategy = helmkube.StatusWatcherStrategy
		up.WaitForJobs = true
		up.Timeout = timeout
		up.Description = "box bootstrap"
		up.Labels = ownerLabels()
		if _, err := up.RunWithContext(ctx, spec.Name, chrt, valuesOf(spec)); err != nil {
			return fmt.Errorf("upgrade %s in %s: %w", spec.Name, spec.Namespace, err)
		}
		return nil
	}
	in := action.NewInstall(cfg)
	in.ReleaseName = spec.Name
	in.Namespace = spec.Namespace
	in.CreateNamespace = true
	in.WaitStrategy = helmkube.StatusWatcherStrategy
	in.WaitForJobs = true
	in.Timeout = timeout
	in.Description = "box bootstrap install"
	in.Labels = ownerLabels()
	if _, err := in.RunWithContext(ctx, chrt, valuesOf(spec)); err != nil {
		return fmt.Errorf("install %s in %s: %w", spec.Name, spec.Namespace, err)
	}
	return nil
}

// ownerLabels is what makes a release box's, stored on the Helm release record (v4's
// release labels) so it is read without rendering and never taken for a chart setting.
// A release without it is the operator's, and box never upgrades or modifies it.
func ownerLabels() map[string]string {
	return map[string]string{kube.ManagedByLabel: kube.ManagedBy}
}

func (h *Helm) exists(cfg *action.Configuration, name string) bool {
	_, err := cfg.Releases.Deployed(name)
	return err == nil
}

func valuesOf(spec Release) map[string]any {
	if spec.Values == nil {
		return map[string]any{}
	}
	return spec.Values
}

// chart fetches the pinned chart into a temporary directory for the install; box never
// ships a chart of its own.
func (h *Helm) chart(spec Release) (*chartv2.Chart, error) {
	dir, err := os.MkdirTemp("", "box-chart-")
	if err != nil {
		return nil, fmt.Errorf("chart %s: %w", spec.Chart, err)
	}
	defer os.RemoveAll(dir)

	path, err := h.download(spec, dir)
	if err != nil {
		return nil, err
	}
	loaded, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("chart %s %s: %w", spec.Chart, spec.Version, err)
	}
	chrt, ok := loaded.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("chart %s %s: unexpected chart type %T", spec.Chart, spec.Version, loaded)
	}
	return chrt, nil
}

func (h *Helm) download(spec Release, dir string) (string, error) {
	env, err := newHelmEnv()
	if err != nil {
		return "", fmt.Errorf("chart %s: %w", spec.Chart, err)
	}
	defer env.Close()

	getters := chartGetters(getter.All(env.settings), h.Retry)
	httpClient := h.chartClient()
	var reg *registry.Client
	ref := spec.Ref
	if isOCI(ref) {
		// The credentials file is inside the private home and does not exist, and the
		// authorizer carries none, so registry credentials are never read or sent: box
		// pulls public charts only (docs/box.md).
		reg, err = registry.NewClient(
			registry.ClientOptHTTPClient(httpClient),
			registry.ClientOptCredentialsFile(env.settings.RegistryConfig),
			registry.ClientOptAuthorizer(orasauth.Client{Client: httpClient}),
		)
		if err != nil {
			return "", fmt.Errorf("chart %s: %w", spec.Chart, err)
		}
	} else {
		url, err := findChartInRepo(ref, spec.Chart, spec.Version, getters, env.settings.RepositoryCache)
		if err != nil {
			return "", fmt.Errorf("chart %s %s from %s: %w", spec.Chart, spec.Version, ref, err)
		}
		ref = url
	}
	cd := downloader.ChartDownloader{
		Out:              io.Discard,
		Getters:          getters,
		Options:          []getter.Option{getter.WithRegistryClient(reg)},
		RegistryClient:   reg,
		RepositoryConfig: env.settings.RepositoryConfig,
		RepositoryCache:  env.settings.RepositoryCache,
		ContentCache:     env.settings.ContentCache,
	}
	path, _, err := cd.DownloadTo(ref, spec.Version, dir)
	if err != nil {
		return "", fmt.Errorf("chart %s %s: %w", spec.Chart, spec.Version, err)
	}
	return path, nil
}

// findChartInRepo is Helm's repo.FindChartInRepoURL with one change: Helm's copy
// writes its scratch index to helmpath.CachePath("repository"), from the process
// environment, which no settings value can redirect. ChartRepository.CachePath is a
// field, so the same download, parsing and lookup happen here with every byte landing
// in the private home the caller removes. Which repository and chart stay Helm's
// decision.
func findChartInRepo(ref, chart, version string, getters getter.Providers, cache string) (string, error) {
	entry := &repo.Entry{Name: "box", URL: ref}
	r, err := repo.NewChartRepository(entry, getters)
	if err != nil {
		return "", err
	}
	r.CachePath = cache
	indexPath, err := r.DownloadIndexFile()
	if err != nil {
		return "", fmt.Errorf("looks like %q is not a valid chart repository or cannot be reached: %w", ref, err)
	}
	index, err := repo.LoadIndexFile(indexPath)
	if err != nil {
		return "", err
	}
	errMsg := fmt.Sprintf("chart %q", chart)
	if version != "" {
		errMsg = fmt.Sprintf("%s version %q", errMsg, version)
	}
	cv, err := index.Get(chart, version)
	if err != nil {
		return "", repo.ChartNotFoundError{Chart: errMsg, RepoURL: ref}
	}
	if len(cv.URLs) == 0 {
		return "", fmt.Errorf("%s has no downloadable URLs", errMsg)
	}
	return repo.ResolveReferenceURL(ref, cv.URLs[0])
}

// chartClient is the OCI fetch's client: the registry client takes one, so the
// retrying transport is installed here rather than around the pull.
func (h *Helm) chartClient() *http.Client {
	return &http.Client{Transport: fetch.Transport{Policy: h.Retry}}
}

func isOCI(ref string) bool {
	return len(ref) > 6 && ref[:6] == "oci://"
}

// interface check: Helm is what cmd/box opens for the platform.
var _ Releases = (*Helm)(nil)
