package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/fetch"
)

// instant is box's retry policy with the waiting removed: the tests assert
// that a fetch was retried, not how long the backoff took.
func instant() fetch.Policy {
	return fetch.Policy{Sleep: func(context.Context, time.Duration) error { return nil }}
}

// hits counts requests and answers each one from the given statuses, in order.
type hits struct {
	statuses []int
	count    atomic.Int32
}

func (h *hits) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(h.count.Add(1)) - 1
		if n >= len(h.statuses) {
			n = len(h.statuses) - 1
		}
		if h.statuses[n] != http.StatusOK {
			http.Error(w, "upstream said no", h.statuses[n])
			return
		}
		_, _ = io.WriteString(w, "k0s binary")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadDefaultsToTheRetryingTransport(t *testing.T) {
	// The production path is the zero value: no injected client, no injected
	// policy. Its client must still be the one that retries, since that is
	// the fetch a first install actually makes.
	transport, ok := HTTPDownloader{}.client().Transport.(fetch.Transport)
	if !ok {
		t.Fatalf("the default client's transport is %T, want fetch.Transport", HTTPDownloader{}.client().Transport)
	}
	if transport.Policy.Attempts != 0 || transport.Policy.Sleep != nil {
		t.Errorf("policy = %+v, want the zero policy that means box's defaults", transport.Policy)
	}
}

func TestHTTPDownloaderRetriesATransientUpstreamFailure(t *testing.T) {
	h := &hits{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	srv := h.serve(t)

	d := HTTPDownloader{Retry: instant()}
	body, err := d.Fetch(context.Background(), srv.URL+"/k0s")
	if err != nil {
		t.Fatalf("a 502 on the first try must not fail the download: %v", err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if string(got) != "k0s binary" {
		t.Errorf("body = %q, want the asset", got)
	}
	if n := h.count.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestHTTPDownloaderDoesNotRetryA404(t *testing.T) {
	h := &hits{statuses: []int{http.StatusNotFound}}
	srv := h.serve(t)

	_, err := HTTPDownloader{Retry: instant()}.Fetch(context.Background(), srv.URL+"/k0s")
	if err == nil {
		t.Fatal("a 404 must fail the download")
	}
	if !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("error = %q, want it to name the status", err)
	}
	if n := h.count.Load(); n != 1 {
		t.Errorf("requests = %d, want 1: a missing asset is not retried", n)
	}
}

func TestHTTPDownloaderNamesTheURLEOnceWhenItGivesUp(t *testing.T) {
	h := &hits{statuses: []int{http.StatusGatewayTimeout}}
	srv := h.serve(t)
	url := srv.URL + "/k0s"

	_, err := HTTPDownloader{Retry: instant()}.Fetch(context.Background(), url)
	if err == nil {
		t.Fatal("four 504s must fail the download")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fetching "+url+": 504 Gateway Timeout after 4 attempts") {
		t.Errorf("error = %q, want box's own sentence", msg)
	}
	if n := strings.Count(msg, url); n != 1 {
		t.Errorf("the URL appears %d times in %q, want once", n, msg)
	}
	if n := h.count.Load(); n != 4 {
		t.Errorf("requests = %d, want 4", n)
	}
}

func TestAPIReadinessSendsBearerToken(t *testing.T) {
	const token = "readytoken"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("%s Authorization = %q, want %q", r.URL.Path, got, "Bearer "+token)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/readyz":
			_, _ = io.WriteString(w, "ok")
		case "/api/v1/nodes":
			_, _ = io.WriteString(w, `{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: %s
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx
current-context: ctx
users:
- name: u
  user:
    token: %s
`, srv.URL, token)
	path := filepath.Join(t.TempDir(), "admin.conf")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	if err := (APIReadiness{ConfigPath: path}).Ready(context.Background()); err != nil {
		t.Fatalf("Ready with a token kubeconfig: %v", err)
	}
}
