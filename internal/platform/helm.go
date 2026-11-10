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

// Helm is the Releases implementation: the Helm SDK over client-go, the same
// client `box apply` uses, for the platform's own releases. Charts are fetched
// from their published homes at run time at the pinned version; nothing is
// embedded but the manifests box itself owns. Both chart fetches -- the chart
// repository and the OCI registry -- carry fetch.Transport, so an upstream 5xx
// is retried instead of failing the install.
type Helm struct {
	Kubeconfig string
	// Timeout bounds one install or upgrade, including its wait. Zero means
	// five minutes.
	Timeout time.Duration
	// Retry is the policy for the chart fetches. Zero means box's policy:
	// four attempts, 1s/2s/4s. Tests replace the sleeper so backoff costs no
	// time.
	Retry fetch.Policy

	configs map[string]*action.Configuration
}

// NewHelm builds the client. It does not contact the cluster: Helm's
// configuration is lazy, so constructing it is not a read or a write.
func NewHelm(kubeconfig string) *Helm {
	return &Helm{Kubeconfig: kubeconfig, configs: map[string]*action.Configuration{}}
}

// configuration returns the Helm configuration for one namespace. Helm stores
// one release record per namespace, so a platform of two releases in two
// namespaces has two configurations over the same cluster.
func (h *Helm) configuration(namespace string) (*action.Configuration, error) {
	if cfg, ok := h.configs[namespace]; ok {
		return cfg, nil
	}
	flags := kube.Flags(h.Kubeconfig, namespace)

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, namespace, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
	// Same rule as `box apply`: the reporter is the only thing that speaks.
	// Helm's slog output and hook stdout are dropped, never mixed into box's
	// transcript (AGENTS.md, the client-go/klog noise rule).
	cfg.SetLogger(slog.DiscardHandler)
	cfg.HookOutputFunc = func(string, string, string) io.Writer { return io.Discard }
	h.configs[namespace] = cfg
	return cfg, nil
}

// Version returns the chart version of a deployed release and whether box's
// ownership label is on it. A release that exists in another state (failed,
// pending) is an error, not something to install over: Helm would refuse the
// name, and silently removing it is not box's call.
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

// ownerLabels is what makes a release box's. It is stored on the Helm release
// record itself (Helm v4's release labels), not in chart values, so it is read
// back without rendering anything and is never mistaken for a chart setting.
// It tells a release box installed from a same-named one it did not: a
// release without it is the operator's, and box never upgrades or modifies it.
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

// chart fetches the pinned chart and loads it. The tarball lives in a
// temporary directory for the length of the install; box fetches at run time
// and never ships a chart of its own.
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
		// The credentials file is inside the private home and does not exist,
		// and the authorizer carries no credential, so registry credentials --
		// Helm's registry/config.json or docker's config.json -- are never read
		// or sent. box only pulls public charts; a private registry is out of
		// scope (docs/box.md), and box says so when one asks for credentials.
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

// findChartInRepo is Helm's repo.FindChartInRepoURL, with the one thing box
// cannot have: the copy in Helm's repo package writes its scratch index to
// helmpath.CachePath("repository") -- root's ~/.cache/helm -- which it creates
// and writes into before deleting the files again. That path comes from the
// process environment (HELM_CACHE_HOME/XDG_CACHE_HOME/HOME) at call time, so no
// settings value can redirect it. repo.ChartRepository.CachePath is a field,
// though: the same index download, parsing and chart lookup happen here, and
// every byte Helm writes lands in the private home the caller removes.
//
// Which repository, and which chart in it, is still Helm's decision: only the
// cache location moves.
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

// chartClient is the client the OCI chart fetch goes through. Helm's registry
// client takes one (registry.ClientOptHTTPClient), so the retrying transport is
// installed here rather than around the pull.
func (h *Helm) chartClient() *http.Client {
	return &http.Client{Transport: fetch.Transport{Policy: h.Retry}}
}

func isOCI(ref string) bool {
	return len(ref) > 6 && ref[:6] == "oci://"
}

// interface check: Helm is what cmd/box opens for the platform.
var _ Releases = (*Helm)(nil)
