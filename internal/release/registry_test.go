package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/pin"
)

const registryDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// registryServer is a registry that answers one tag, on loopback and in the
// clear, which is the only plain HTTP box speaks.
func registryServer(t *testing.T, status func(ref string) int, digest string) (*httptest.Server, *int32) {
	var asks int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&asks, 1)
		if r.Method != http.MethodHead {
			t.Errorf("%s %s: box asks with HEAD", r.Method, r.URL.Path)
		}
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		code := http.StatusOK
		if status != nil {
			code = status(ref)
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &asks
}

func loopbackRepository(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://") + "/example/qbittorrent"
}

func TestRegistryResolvesATagToItsManifestDigest(t *testing.T) {
	srv, asks := registryServer(t, nil, registryDigest)
	digest, err := Registry{Timeout: 5 * time.Second}.Resolve(context.Background(), loopbackRepository(srv), "5.3-lt2.1")
	if err != nil {
		t.Fatal(err)
	}
	if digest != registryDigest {
		t.Fatalf("digest = %q, want %q", digest, registryDigest)
	}
	if atomic.LoadInt32(asks) != 1 {
		t.Fatalf("asked %d times, want one HEAD", *asks)
	}
}

func TestRegistrySaysNotFoundForATagThatIsNotThere(t *testing.T) {
	srv, _ := registryServer(t, func(string) int { return http.StatusNotFound }, registryDigest)
	_, err := Registry{Timeout: 5 * time.Second}.Resolve(context.Background(), loopbackRepository(srv), "5.2-lt2.1")
	if !errors.Is(err, pin.ErrNotFound) {
		t.Fatalf("Resolve = %v, want pin.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "example/qbittorrent") || !strings.Contains(err.Error(), "5.2-lt2.1") {
		t.Fatalf("the error names neither the repository nor the tag: %v", err)
	}
}

func TestRegistryRetriesAServerError(t *testing.T) {
	var calls int32
	srv, _ := registryServer(t, func(string) int {
		if atomic.AddInt32(&calls, 1) == 1 {
			return http.StatusBadGateway
		}
		return http.StatusOK
	}, registryDigest)
	var waits []time.Duration
	digest, err := Registry{
		Timeout: 5 * time.Second,
		Retry: fetch.Policy{
			Sleep: func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			},
		},
	}.Resolve(context.Background(), loopbackRepository(srv), "5.3-lt2.1")
	if err != nil {
		t.Fatal(err)
	}
	if digest != registryDigest {
		t.Fatalf("digest = %q, want the answer after the retry", digest)
	}
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want one retry", waits)
	}
}

func TestRegistryStopsAtItsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	started := time.Now()
	_, err := Registry{Timeout: 100 * time.Millisecond}.Resolve(context.Background(), loopbackRepository(srv), "5.3-lt2.1")
	if err == nil {
		t.Fatal("a registry that never answers resolved")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Resolve took %s, want its own budget", elapsed)
	}
	if !strings.Contains(err.Error(), "example/qbittorrent") {
		t.Fatalf("the error does not name the repository: %v", err)
	}
}

func TestPlainRegistryIsOnlyLoopback(t *testing.T) {
	for _, c := range []struct {
		repository string
		plain      bool
	}{
		{"ghcr.io/swizzin/qbittorrent", false},
		{"127.0.0.1:5000/example/qbittorrent", true},
		{"localhost:5000/example/qbittorrent", true},
		{"[::1]:5000/example/qbittorrent", true},
		{"example.com:5000/example/qbittorrent", false},
	} {
		if got := plainRegistry(c.repository); got != c.plain {
			t.Errorf("plainRegistry(%q) = %v, want %v", c.repository, got, c.plain)
		}
	}
}

func TestRegistrySaysNotFoundForGhcrForbidden(t *testing.T) {
	// ghcr answers 403 for a package that is missing or private, which is the
	// same answer as 404.
	srv, _ := registryServer(t, func(string) int { return http.StatusForbidden }, registryDigest)
	_, err := Registry{Timeout: 5 * time.Second}.Resolve(context.Background(), loopbackRepository(srv), "5.2-lt2.1")
	if !errors.Is(err, pin.ErrNotFound) {
		t.Fatalf("Resolve = %v, want pin.ErrNotFound", err)
	}
}
