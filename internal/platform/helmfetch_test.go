package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"helm.sh/helm/v4/pkg/cli"
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

	settings := cli.New()
	settings.RepositoryCache = t.TempDir()
	settings.ContentCache = t.TempDir()
	// A box run inherits the operator's Helm repository config, and any entry
	// in it without a cached index fails a direct-URL download inside Helm's
	// scanReposForURL. The test fetches a direct URL, so it gets an empty one.
	settings.RepositoryConfig = filepath.Join(t.TempDir(), "repositories.yaml")
	h := &Helm{Retry: noWait(), settings: settings}
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
