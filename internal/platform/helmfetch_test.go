package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/getter"

	"github.com/swizzin/s5n/internal/fetch"
)

// noWait is box's retry policy with the waiting removed: these tests assert
// that the fetch was retried, not how long the backoff took.
func noWait() fetch.Policy {
	return fetch.Policy{Sleep: func(context.Context, time.Duration) error { return nil }}
}

// chartRepo serves a one-chart repository: index.yaml and the chart's tarball.
// Either path can be made to fail on its first request, which is the shape of
// the upstream 5xx that aborted a real bootstrap.
type chartRepo struct {
	name, version string
	tarball       []byte
	indexFailures int
	chartFailures int
	indexHits     atomic.Int32
	chartHits     atomic.Int32
	url           string
}

func (r *chartRepo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/index.yaml"):
		if int(r.indexHits.Add(1)) <= r.indexFailures {
			http.Error(w, "upstream took too long", http.StatusGatewayTimeout)
			return
		}
		sum := sha256.Sum256(r.tarball)
		fmt.Fprintf(w, `apiVersion: v1
entries:
  %s:
    - apiVersion: v2
      name: %s
      version: %s
      digest: sha256:%s
      urls:
        - %s/%s-%s.tgz
`, r.name, r.name, r.version, hex.EncodeToString(sum[:]), r.url, r.name, r.version)
	case strings.HasSuffix(req.URL.Path, "/"+r.name+"-"+r.version+".tgz"):
		if int(r.chartHits.Add(1)) <= r.chartFailures {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(r.tarball)
	default:
		http.NotFound(w, req)
	}
}

// newChartRepo starts the server and returns a Helm pointed at it.
func newChartRepo(t *testing.T, indexFailures, chartFailures int) (*chartRepo, *Helm, Release) {
	t.Helper()
	const name, version = "metallb", "0.16.1"
	tarball, err := chartutil.Save(&chartv2.Chart{Metadata: &chartv2.Metadata{
		APIVersion: "v2",
		Name:       name,
		Version:    version,
	}}, t.TempDir())
	if err != nil {
		t.Fatalf("build a chart to serve: %v", err)
	}
	data, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("read the chart: %v", err)
	}
	repo := &chartRepo{name: name, version: version, tarball: data,
		indexFailures: indexFailures, chartFailures: chartFailures}

	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	repo.url = srv.URL

	// box builds its own Helm home per fetch; see TestDownloadIgnoresTheOperatorsHelmHome.
	h := &Helm{Retry: noWait()}
	return repo, h, Release{Chart: name, Version: version, Ref: srv.URL}
}

func TestDownloadRetriesATransientChartRepoFailure(t *testing.T) {
	repo, h, spec := newChartRepo(t, 1, 1)

	dir := t.TempDir()
	path, err := h.download(spec, dir)
	if err != nil {
		t.Fatalf("a 5xx on the first try of each fetch must not fail the download: %v", err)
	}
	if got := repo.indexHits.Load(); got != 2 {
		t.Errorf("index requests = %d, want 2: the index fetch is retried", got)
	}
	if got := repo.chartHits.Load(); got != 2 {
		t.Errorf("chart requests = %d, want 2: the chart fetch is retried", got)
	}
	loaded, err := loader.Load(path)
	if err != nil {
		t.Fatalf("load what was downloaded: %v", err)
	}
	chrt, ok := loaded.(*chartv2.Chart)
	if !ok {
		t.Fatalf("loaded %T, want a chart", loaded)
	}
	if chrt.Metadata == nil || chrt.Metadata.Name != spec.Chart {
		t.Errorf("chart = %v, want %s", chrt.Metadata, spec.Chart)
	}
}

func TestDownloadDoesNotRetryAMissingChart(t *testing.T) {
	_, h, spec := newChartRepo(t, 0, 0)
	spec.Version = "9.9.9"

	_, err := h.download(spec, t.TempDir())
	if err == nil {
		t.Fatal("a chart version the index does not list must fail the download")
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error = %q, want it to name the version", err)
	}
}

func TestDownloadNamesTheURLEOnceWhenTheUpstreamKeepsFailing(t *testing.T) {
	repo, h, spec := newChartRepo(t, 99, 0)

	_, err := h.download(spec, t.TempDir())
	if err == nil {
		t.Fatal("four 504s on the index must fail the download")
	}
	want := "fetching " + repo.url + "/index.yaml: 504 Gateway Timeout after 4 attempts"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if got := repo.indexHits.Load(); got != 4 {
		t.Errorf("index requests = %d, want 4", got)
	}
}

// TestGetterStatusReadsTheStatusHelmReturns runs Helm's own HTTP getter
// against a 504. internal/platform reads the status back out of the error
// Helm builds, because the getter API exposes no other way to tell a retryable
// 503 from a final 404; if a Helm upgrade rewords that error, this fails
// instead of the retries silently stopping.
func TestGetterStatusReadsTheStatusHelmReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream took too long", http.StatusGatewayTimeout)
	}))
	defer srv.Close()

	url := srv.URL + "/metallb-0.16.1.tgz"
	g, err := getter.NewHTTPGetter(getter.WithTimeout(5 * time.Second))
	if err != nil {
		t.Fatalf("build Helm's getter: %v", err)
	}
	if _, err := g.Get(url); err == nil {
		t.Fatal("a 504 must fail the getter")
	} else if code, status, ok := getterStatus(err, url); !ok {
		t.Fatalf("Helm's getter error no longer names the status (%q); getterStatus needs updating: %v", status, err)
	} else if code != http.StatusGatewayTimeout || status != "504 Gateway Timeout" {
		t.Errorf("status = %q (%d), want 504 Gateway Timeout", status, code)
	}
}

// TestChartClientRetriesServerErrors proves the *http.Client box hands Helm's
// OCI registry client (registry.ClientOptHTTPClient) really carries the
// retrying transport: the registry client is not introspectable from outside,
// so the client it is built from is exercised here.
func TestChartClientRetriesServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	client := (&Helm{Retry: noWait()}).chartClient()
	resp, err := client.Get(srv.URL + "/v2/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("four 502s must fail the request")
	}
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("error = %q, want the retry count in it", err)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("requests = %d, want 4", got)
	}
}

// interface check: the wrapped getter is a getter.
var _ getter.Getter = retryingGetter{}

// poisonHelmHome writes the operator Helm state box must never read, and points
// the process environment at it. repositories.yaml is the failure that stopped a
// real bootstrap: an entry whose index was never cached (what `helm repo add`
// without `helm repo update` leaves behind) makes Helm's scanReposForURL fail
// every chart URL download with "no cached repo found ... try 'helm repo
// update'". The registry config is not valid JSON, so a Helm registry client
// built from it fails at construction; and a plugin getter sits in the plugins
// directory, because a getter plugin is code Helm would run in box's process.
func poisonHelmHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "config", "repositories.yaml"), `apiVersion: ""
generated: "2020-01-01T00:00:00Z"
repositories:
  - name: stale
    url: https://charts.example.com
`)
	writeFile(t, filepath.Join(root, "config", "registry", "config.json"), `{"auths": `)
	writeFile(t, filepath.Join(root, "plugins", "box-getter", "plugin.yaml"), `name: box-getter
version: 0.1.0
downloaders:
  - command: "box-getter"
    protocols:
      - "https"
`)
	for key, path := range map[string]string{
		"HOME":                   root,
		"XDG_CONFIG_HOME":        filepath.Join(root, "config"),
		"XDG_CACHE_HOME":         filepath.Join(root, "cache"),
		"XDG_DATA_HOME":          filepath.Join(root, "data"),
		"HELM_CONFIG_HOME":       filepath.Join(root, "config"),
		"HELM_CACHE_HOME":        filepath.Join(root, "cache"),
		"HELM_DATA_HOME":         filepath.Join(root, "data"),
		"HELM_REPOSITORY_CONFIG": filepath.Join(root, "config", "repositories.yaml"),
		"HELM_REPOSITORY_CACHE":  filepath.Join(root, "cache", "repository"),
		"HELM_REGISTRY_CONFIG":   filepath.Join(root, "config", "registry", "config.json"),
		"HELM_CONTENT_CACHE":     filepath.Join(root, "cache", "content"),
		"HELM_PLUGINS":           filepath.Join(root, "plugins"),
	} {
		t.Setenv(key, path)
	}
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot renders every entry under root: path, mode, size, mtime and the
// content hash of a file. A directory's mtime changes when Helm creates and
// deletes its scratch index inside it, so this catches a write that is cleaned
// up again -- which is exactly what FindChartInRepoURL does to ~/.cache/helm.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hash := ""
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			hash = hex.EncodeToString(sum[:])
		}
		fmt.Fprintf(&b, "%s mode=%s size=%d mtime=%s sha=%s\n",
			rel, info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), hash)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return b.String()
}

// TestDownloadIgnoresTheOperatorsHelmHome is the regression: with root's Helm
// environment poisoned -- a repository entry and no cached index, credentials
// Helm cannot parse, a getter plugin -- the chart still downloads, and nothing
// box did is visible in that tree. Before box built its own Helm home, the
// stale entry failed this download with "no cached repo found".
func TestDownloadIgnoresTheOperatorsHelmHome(t *testing.T) {
	poison := poisonHelmHome(t)
	before := snapshot(t, poison)

	_, h, spec := newChartRepo(t, 0, 0)
	path, err := h.download(spec, t.TempDir())
	if err != nil {
		t.Fatalf("the operator's Helm home must not decide what box downloads: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the downloaded chart is not where download said it is: %v", err)
	}

	if after := snapshot(t, poison); after != before {
		t.Fatalf("box wrote into the operator's Helm home:\n--- before\n%s--- after\n%s", before, after)
	}
}

// TestDownloadLeavesNoTemporaryHelmHome: the private home exists for the length
// of one fetch and no longer. TMPDIR is a sandbox here, so a leftover directory
// (Helm's or box's) is visible; both the success and the failure path are
// checked, because a failed download is exactly when a defer is easy to miss.
func TestDownloadLeavesNoTemporaryHelmHome(t *testing.T) {
	_, h, spec := newChartRepo(t, 0, 0)
	root := t.TempDir()
	sandbox := filepath.Join(root, "tmp")
	dest := filepath.Join(root, "dest")
	for _, dir := range []string{sandbox, dest} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", sandbox)

	if _, err := h.download(spec, dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	if left := readDir(t, sandbox); len(left) != 0 {
		t.Fatalf("a successful download left %v behind in its temporary directory", left)
	}

	spec.Version = "9.9.9"
	if _, err := h.download(spec, dest); err == nil {
		t.Fatal("a chart version the index does not list must fail the download")
	}
	if left := readDir(t, sandbox); len(left) != 0 {
		t.Fatalf("a failed download left %v behind in its temporary directory", left)
	}
}

func readDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestHelmHomeIsPrivate pins the shape of the home itself: 0700, every path
// Helm consults inside it, and no plugins directory to scan, so an operator's
// getter plugin cannot be loaded.
func TestHelmHomeIsPrivate(t *testing.T) {
	env, err := newHelmEnv()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 0700", perm)
	}
	for name, path := range map[string]string{
		"RepositoryConfig": env.settings.RepositoryConfig,
		"RepositoryCache":  env.settings.RepositoryCache,
		"ContentCache":     env.settings.ContentCache,
		"RegistryConfig":   env.settings.RegistryConfig,
		"PluginsDirectory": env.settings.PluginsDirectory,
	} {
		if rel, err := filepath.Rel(env.dir, path); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s = %s, want it under %s", name, path, env.dir)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists (%v), want an empty private home", path, err)
		}
	}
	dir := env.dir
	if err := env.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the private home still exists after Close: %v", err)
	}
}
