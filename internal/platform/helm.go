package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	helmrelease "helm.sh/helm/v4/pkg/release"
	repo "helm.sh/helm/v4/pkg/repo/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

// Helm is the Releases implementation: the Helm SDK over client-go, the same
// client `box apply` uses, for the platform's own releases. Charts are fetched
// from their published homes at run time at the pinned version; nothing is
// embedded but the manifests box itself owns.
type Helm struct {
	Kubeconfig string
	// Timeout bounds one install or upgrade, including its wait. Zero means
	// five minutes.
	Timeout time.Duration

	configs  map[string]*action.Configuration
	settings *cli.EnvSettings
	getters  getter.Providers
	registry *registry.Client
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
	path := h.Kubeconfig
	ns := namespace
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &path
	flags.Namespace = &ns

	rest.SetDefaultWarningHandler(rest.NoWarnings{})

	cfg := new(action.Configuration)
	if err := cfg.Init(flags, ns, "secret"); err != nil {
		return nil, fmt.Errorf("release client for %s: %w", namespace, err)
	}
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
	owned := acc.Labels()[ReleaseOwnerLabel] == ReleaseOwner
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
		up.WaitStrategy = kube.StatusWatcherStrategy
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
	in.WaitStrategy = kube.StatusWatcherStrategy
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
func ownerLabels() map[string]string {
	return map[string]string{ReleaseOwnerLabel: ReleaseOwner}
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
	if h.settings == nil {
		h.settings = cli.New()
	}
	if h.getters == nil {
		h.getters = getter.All(h.settings)
	}
	ref := spec.Ref
	if isOCI(ref) {
		if h.registry == nil {
			rc, err := registry.NewClient()
			if err != nil {
				return "", fmt.Errorf("chart %s: %w", spec.Chart, err)
			}
			h.registry = rc
		}
	} else {
		url, err := repo.FindChartInRepoURL(ref, spec.Chart, h.getters, repo.WithChartVersion(spec.Version))
		if err != nil {
			return "", fmt.Errorf("chart %s %s from %s: %w", spec.Chart, spec.Version, ref, err)
		}
		ref = url
	}
	cd := downloader.ChartDownloader{
		Out:              io.Discard,
		Getters:          h.getters,
		Options:          []getter.Option{getter.WithRegistryClient(h.registry)},
		RegistryClient:   h.registry,
		RepositoryConfig: h.settings.RepositoryConfig,
		RepositoryCache:  h.settings.RepositoryCache,
		ContentCache:     h.settings.ContentCache,
	}
	path, _, err := cd.DownloadTo(ref, spec.Version, dir)
	if err != nil {
		return "", fmt.Errorf("chart %s %s: %w", spec.Chart, spec.Version, err)
	}
	return path, nil
}

func isOCI(ref string) bool {
	return len(ref) > 6 && ref[:6] == "oci://"
}

// interface check: Helm is what cmd/box opens for the platform.
var _ Releases = (*Helm)(nil)
