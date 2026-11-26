package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeTautulli is a minimal Tautulli: api/v2, /settings' CSRF token and
// /configUpdate, with a write counter.
type fakeTautulli struct {
	mu       sync.Mutex
	apiKey   string
	pmsIP    string
	pmsPort  int
	pmsID    string
	pmsToken string
	// tokenMode: "" returns the stored value, "hide" null (a do-not-download key),
	// "mask" a placeholder.
	tokenMode    string
	apiEnabled   bool
	writes       int
	configUpdate int
	lastForm     url.Values
}

func newFakeTautulli(t *testing.T) (*fakeTautulli, *httptest.Server) {
	t.Helper()
	f := &fakeTautulli{apiKey: "test-key", apiEnabled: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTautulli) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v2":
		f.serveAPI(w, r)
	case r.URL.Path == "/settings":
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta name="csrf-token" content="test-csrf-token"></head></html>`))
	case r.URL.Path == "/configUpdate" && r.Method == http.MethodPost:
		if r.Header.Get("X-CSRF-Token") != "test-csrf-token" {
			http.Error(w, "missing csrf token", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		f.lastForm = r.Form
		f.configUpdate++
		f.writes++
		f.pmsIP = r.Form.Get("pms_ip")
		var err error
		f.pmsPort, err = atoiSafe(r.Form.Get("pms_port"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.pmsID = r.Form.Get("pms_identifier")
		f.pmsToken = r.Form.Get("pms_token")
		f.apiEnabled = r.Form.Get("api_enabled") == "1"
		writeTautulliJSON(w, map[string]any{"result": "success", "message": "Settings saved."})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func atoiSafe(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, &HTTPError{Status: http.StatusBadRequest, Body: "not a number: " + s}
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func (f *fakeTautulli) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("apikey") != f.apiKey {
		writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "error", "message": "Invalid apikey"}})
		return
	}
	if !f.apiEnabled {
		writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "error", "message": "API not enabled"}})
		return
	}
	switch r.URL.Query().Get("cmd") {
	case "status":
		writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{}}})
	case "get_settings":
		apiEnabled := 0
		if f.apiEnabled {
			apiEnabled = 1
		}
		var token any = f.pmsToken
		switch f.tokenMode {
		case "hide":
			token = nil
		case "mask":
			token = "********"
		}
		writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
			"General": map[string]any{"api_enabled": apiEnabled},
			"PMS": map[string]any{
				"pms_ip":         f.pmsIP,
				"pms_port":       f.pmsPort,
				"pms_identifier": f.pmsID,
				"pms_token":      token,
			},
		}}})
	case "get_server_identity":
		writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
			"machine_identifier": f.pmsID,
			"version":            "1.43.5",
		}}})
	default:
		http.Error(w, "unexpected cmd "+r.URL.Query().Get("cmd"), http.StatusNotFound)
	}
}

func writeTautulliJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testPlexToken stands in for the Secret's plex-token.
const testPlexToken = "plex-token-abc123"

func TestTautulliWaitReady(t *testing.T) {
	_, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	if err := tt.WaitReady(context.Background(), 2_000_000_000); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestTautulliWaitReadyFailsOnBadKey(t *testing.T) {
	_, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "wrong-key")
	if err := tt.Ping(context.Background()); err == nil {
		t.Fatal("ping succeeded with the wrong api key")
	}
}

// The chart seals http_root to /tautulli, so every request lands under it and the
// port's root redirects.
func TestTautulliAsksUnderThePrefixItIsServedAt(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/tautulli/api/v2" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{"result": "success", "message": "Ok"},
		})
	}))
	defer srv.Close()

	tt := NewTautulli(srv.URL+"/tautulli", "test-key")
	if err := tt.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/tautulli/api/v2" {
		t.Fatalf("the client asked %v, want one request to /tautulli/api/v2", paths)
	}
}

func TestEnsurePlexConnectionCreatesThenIsIdempotent(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, "")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if change != Created {
		t.Fatalf("first ensure change = %v, want Created", change)
	}
	if f.configUpdate != 1 {
		t.Fatalf("configUpdate called %d times, want 1", f.configUpdate)
	}
	// An absent checkbox is "unchecked": omitting api_enabled would disable the
	// API on the call that configures it.
	if f.lastForm.Get("api_enabled") != "1" {
		t.Fatalf("api_enabled not resent: form = %v", f.lastForm)
	}
	if !f.apiEnabled {
		t.Fatal("api_enabled was turned off by the write")
	}

	change, err = tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, "")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if change != Unchanged {
		t.Fatalf("second ensure change = %v, want Unchanged", change)
	}
	if f.configUpdate != 1 {
		t.Fatalf("second ensure wrote again: configUpdate = %d, want 1", f.configUpdate)
	}
}

func TestEnsurePlexConnectionRepairsDrift(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatal(err)
	}
	// The user (or a reinstalled Plex) changed identity.
	f.pmsID = "stale"

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, "")
	if err != nil {
		t.Fatalf("ensure after drift: %v", err)
	}
	if change != Updated {
		t.Fatalf("change = %v, want Updated", change)
	}
	if f.pmsID != "machine-123" {
		t.Fatalf("identifier not repaired: %q", f.pmsID)
	}
}

func TestEnsurePlexConnectionFailsWhenIdentityDoesNotMatch(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	// A save that reaches a different Plex must fail the identity proof.
	origHandler := f
	_ = origHandler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configUpdate" {
			f.mu.Lock()
			f.configUpdate++
			f.mu.Unlock()
			writeTautulliJSON(w, map[string]any{"result": "success", "message": "Settings saved."})
			return
		}
		if r.URL.Path == "/settings" {
			_, _ = w.Write([]byte(`<meta name="csrf-token" content="test-csrf-token">`))
			return
		}
		if r.URL.Path == "/api/v2" && r.URL.Query().Get("cmd") == "get_settings" {
			writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
				"General": map[string]any{"api_enabled": 1},
				"PMS":     map[string]any{"pms_ip": "", "pms_port": 0, "pms_identifier": ""},
			}}})
			return
		}
		if r.URL.Path == "/api/v2" && r.URL.Query().Get("cmd") == "get_server_identity" {
			writeTautulliJSON(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
				"machine_identifier": "some-other-server",
			}}})
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	_, err := tt.EnsurePlexConnection(context.Background(), "plex", 32400, "machine-123", testPlexToken, "")
	if err == nil {
		t.Fatal("ensure succeeded although get_server_identity proved a different server")
	}
}

func TestEnsurePlexConnectionSendsPlexToken(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")

	if _, err := tt.EnsurePlexConnection(context.Background(), "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := f.lastForm.Get("pms_token"); got != testPlexToken {
		t.Fatalf("pms_token sent as %q, want %q", got, testPlexToken)
	}
	if f.pmsToken != testPlexToken {
		t.Fatalf("stored pms_token = %q, want %q", f.pmsToken, testPlexToken)
	}
}

// A rotated token is drift even when host, port and identifier match.
func TestEnsurePlexConnectionRewritesRotatedToken(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatal(err)
	}
	before := f.configUpdate

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, "")
	if err != nil {
		t.Fatalf("unchanged token: %v", err)
	}
	if change != Unchanged || f.configUpdate != before {
		t.Fatalf("same token: change=%v writes=%d, want Unchanged and no write", change, f.configUpdate-before)
	}

	rotated := testPlexToken + "-rotated"
	change, err = tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", rotated, "")
	if err != nil {
		t.Fatalf("rotated token: %v", err)
	}
	if change != Updated {
		t.Fatalf("rotated token: change = %v, want Updated", change)
	}
	if f.pmsToken != rotated {
		t.Fatalf("rotated token not stored: %q", f.pmsToken)
	}
}

// With pms_token hidden, the fingerprint is the only evidence the link is current: a
// match is a no-op.
func TestEnsurePlexConnectionWithheldTokenSameFingerprintStaysUnchanged(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatal(err)
	}
	f.tokenMode = "hide"
	before := f.configUpdate

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, PlexTokenFingerprint(testPlexToken))
	if err != nil {
		t.Fatalf("withheld token: %v", err)
	}
	if change != Unchanged {
		t.Fatalf("withheld token: change = %v, want Unchanged", change)
	}
	if f.configUpdate != before {
		t.Fatalf("withheld token: rewrote settings (%d writes)", f.configUpdate-before)
	}
}

// A fingerprint for another token means the Secret rotated: rewrite with the current
// one.
func TestEnsurePlexConnectionWithheldTokenRewritesRotatedToken(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	oldToken := testPlexToken
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", oldToken, ""); err != nil {
		t.Fatal(err)
	}
	f.tokenMode = "hide"
	before := f.configUpdate

	rotated := testPlexToken + "-rotated"
	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", rotated, PlexTokenFingerprint(oldToken))
	if err != nil {
		t.Fatalf("rotated token: %v", err)
	}
	if change != Updated {
		t.Fatalf("rotated token: change = %v, want Updated", change)
	}
	if f.configUpdate != before+1 {
		t.Fatalf("rotated token: configUpdate called %d time(s), want 1", f.configUpdate-before)
	}
	if got := f.lastForm.Get("pms_token"); got != rotated {
		t.Fatalf("pms_token sent as %q, want the rotated token", got)
	}
	if f.pmsToken != rotated {
		t.Fatalf("rotated token not stored: %q", f.pmsToken)
	}
}

// No fingerprint with a hidden token is "unknown": rewrite once, then it is a
// no-op.
func TestEnsurePlexConnectionWithheldTokenWithoutFingerprintRewritesOnce(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatal(err)
	}
	f.tokenMode = "hide"

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, "")
	if err != nil {
		t.Fatalf("unknown token: %v", err)
	}
	if change != Updated {
		t.Fatalf("unknown token: change = %v, want Updated (rewrite once)", change)
	}
	after := f.configUpdate
	change, err = tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, PlexTokenFingerprint(testPlexToken))
	if err != nil {
		t.Fatalf("known token: %v", err)
	}
	if change != Unchanged || f.configUpdate != after {
		t.Fatalf("known token: change=%v writes=%d, want Unchanged and no write", change, f.configUpdate-after)
	}
}

// A masked placeholder is never compared or written back: a write still carries the
// Secret's token.
func TestEnsurePlexConnectionNeverWritesMaskedToken(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	ctx := context.Background()
	if _, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, ""); err != nil {
		t.Fatal(err)
	}
	f.tokenMode = "mask"
	f.pmsIP = "elsewhere"

	change, err := tt.EnsurePlexConnection(ctx, "plex", 32400, "machine-123", testPlexToken, PlexTokenFingerprint(testPlexToken))
	if err != nil {
		t.Fatalf("masked live token: %v", err)
	}
	if change != Updated {
		t.Fatalf("masked live token with host drift: change = %v, want Updated", change)
	}
	if got := f.lastForm.Get("pms_token"); got != testPlexToken {
		t.Fatalf("pms_token sent as %q, want the Secret's token", got)
	}
	if strings.Contains(f.lastForm.Encode(), "********") {
		t.Fatalf("masked placeholder written back: %v", f.lastForm)
	}
}

// Without a token the client refuses: the alternative is the network-trust hole.
func TestEnsurePlexConnectionRequiresToken(t *testing.T) {
	f, srv := newFakeTautulli(t)
	tt := NewTautulli(srv.URL, "test-key")
	if _, err := tt.EnsurePlexConnection(context.Background(), "plex", 32400, "machine-123", "  ", ""); err == nil {
		t.Fatal("ensure succeeded with an empty Plex token")
	}
	if f.configUpdate != 0 {
		t.Fatalf("configUpdate called %d times without a token", f.configUpdate)
	}
}
