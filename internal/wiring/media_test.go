package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
)

// fakeMediaStack is Plex + Tautulli + Seerr, standing in for
// hack/e2e-kind.sh's live versions in the same spirit as fakeStack in
// wiring_test.go: just enough of each API for the wiring Job's steps to be
// provable without a cluster.
type fakeMediaStack struct {
	mu      sync.Mutex
	claimed bool
	machine string

	apiKey     string
	pmsIP      string
	pmsPort    int
	pmsID      string
	apiEnabled bool
	writes     int

	seerrUp bool
}

func (f *fakeMediaStack) plexHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/identity" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		claimedAttr := "0"
		if f.claimed {
			claimedAttr = "1"
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer claimed="` + claimedAttr + `" machineIdentifier="` + f.machine + `" version="1.43.5"></MediaContainer>`))
	}
}

func (f *fakeMediaStack) tautulliHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v2":
			f.tautulliAPI(w, r)
		case r.URL.Path == "/settings":
			_, _ = w.Write([]byte(`<meta name="csrf-token" content="test-csrf-token">`))
		case r.URL.Path == "/configUpdate" && r.Method == http.MethodPost:
			if r.Header.Get("X-CSRF-Token") != "test-csrf-token" {
				http.Error(w, "missing csrf token", http.StatusForbidden)
				return
			}
			_ = r.ParseForm()
			f.writes++
			f.pmsIP = r.Form.Get("pms_ip")
			f.pmsPort, _ = strconv.Atoi(r.Form.Get("pms_port"))
			f.pmsID = r.Form.Get("pms_identifier")
			f.apiEnabled = r.Form.Get("api_enabled") == "1"
			writeJSONAny(w, map[string]any{"result": "success", "message": "Settings saved."})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *fakeMediaStack) tautulliAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("apikey") != f.apiKey {
		writeJSONAny(w, map[string]any{"response": map[string]any{"result": "error", "message": "Invalid apikey"}})
		return
	}
	switch r.URL.Query().Get("cmd") {
	case "status":
		writeJSONAny(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{}}})
	case "get_settings":
		apiEnabled := 0
		if f.apiEnabled {
			apiEnabled = 1
		}
		writeJSONAny(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
			"General": map[string]any{"api_enabled": apiEnabled},
			"PMS":     map[string]any{"pms_ip": f.pmsIP, "pms_port": f.pmsPort, "pms_identifier": f.pmsID},
		}}})
	case "get_server_identity":
		writeJSONAny(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{"machine_identifier": f.pmsID}}})
	default:
		http.Error(w, "unexpected cmd "+r.URL.Query().Get("cmd"), http.StatusNotFound)
	}
}

func (f *fakeMediaStack) seerrHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/api/v1/settings/public" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		if !f.seerrUp {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		writeJSONAny(w, map[string]any{"initialized": false})
	}
}

func writeJSONAny(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newMediaHarness builds a Runner wired to fake Plex, Tautulli and Seerr
// servers only (no qBittorrent/Arr/Prowlarr), so the media steps can be
// asserted in isolation from the rest of internal/wiring's coverage.
func newMediaHarness(t *testing.T, claimed bool) (*Runner, *fakeMediaStack) {
	t.Helper()
	f := &fakeMediaStack{claimed: claimed, machine: "machine-123", apiKey: "tautulli-key", seerrUp: true}
	plex := httptest.NewServer(f.plexHandler())
	tautulli := httptest.NewServer(f.tautulliHandler())
	seerr := httptest.NewServer(f.seerrHandler())
	t.Cleanup(func() { plex.Close(); tautulli.Close(); seerr.Close() })

	dir := t.TempDir()
	apiKeyFile := dir + "/tautulli.key"
	if err := os.WriteFile(apiKeyFile, []byte(f.apiKey), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		TimeoutSeconds: 10,
		Plex:           &PlexConfig{URL: plex.URL},
		Tautulli:       &TautulliConfig{URL: tautulli.URL, APIKeyFile: apiKeyFile},
		Seerr:          &SeerrConfig{URL: seerr.URL},
	}
	runner := &Runner{Config: cfg}
	return runner, f
}

func step(res *Result, name string) *Step {
	for i := range res.Steps {
		if res.Steps[i].Name == name {
			return &res.Steps[i]
		}
	}
	return nil
}

func TestWiringClaimedPlexWiresTautulliButSeerrStaysPending(t *testing.T) {
	r, f := newMediaHarness(t, true)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "plex/ready"); s == nil || s.Result != "ok" {
		t.Fatalf("plex/ready = %+v", s)
	}
	if s := step(res, "plex/claim"); s == nil || s.Result != "ok" {
		t.Fatalf("plex/claim = %+v, want ok (claimed)", s)
	}
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("tautulli/plex = %+v, want ok/created", s)
	}
	if f.pmsID != "machine-123" {
		t.Fatalf("tautulli PMS identifier = %q", f.pmsID)
	}
	if s := step(res, "seerr/ready"); s == nil || s.Result != "ok" {
		t.Fatalf("seerr/ready = %+v", s)
	}
	if s := step(res, "seerr/plex"); s == nil || s.Result != "pending" {
		t.Fatalf("seerr/plex = %+v, want pending even though Plex is claimed", s)
	}
}

func TestWiringUnclaimedPlexLeavesClaimPendingButStillWiresTautulli(t *testing.T) {
	r, f := newMediaHarness(t, false)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "plex/claim"); s == nil || s.Result != "pending" {
		t.Fatalf("plex/claim = %+v, want pending (no claim token)", s)
	}
	// Tautulli needs no Plex.tv token at all (PLEX_NO_AUTH_NETWORKS on the
	// chart side covers it), so it still wires regardless of claim state.
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "ok" {
		t.Fatalf("tautulli/plex = %+v, want ok even though Plex is unclaimed", s)
	}
	if f.pmsID != "machine-123" {
		t.Fatalf("tautulli PMS identifier = %q", f.pmsID)
	}
	if s := step(res, "seerr/plex"); s == nil || s.Result != "pending" {
		t.Fatalf("seerr/plex = %+v, want pending", s)
	}
}

func TestWiringMediaSecondRunIsANoOp(t *testing.T) {
	r, f := newMediaHarness(t, true)
	ctx := context.Background()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	writesAfterFirst := f.writes

	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(res))
	}
	if got := f.writes; got != writesAfterFirst {
		t.Fatalf("second run wrote to Tautulli %d more time(s)", got-writesAfterFirst)
	}
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "ok" || s.Detail != "unchanged" {
		t.Fatalf("tautulli/plex on second run = %+v, want unchanged", s)
	}
}

func TestWiringTautulliPendingWhenPlexNotDeployed(t *testing.T) {
	r, _ := newMediaHarness(t, true)
	r.Config.Plex = nil
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "pending" {
		t.Fatalf("tautulli/plex = %+v, want pending when plex is not configured", s)
	}
}

// Sanity check that the config validator used by url.Parse-adjacent code
// does not silently accept an empty Tautulli API key file path.
func TestTautulliConfigValidation(t *testing.T) {
	cfg := Config{
		Plex:     &PlexConfig{URL: "http://plex:32400"},
		Tautulli: &TautulliConfig{URL: "http://tautulli:8181", APIKeyFile: ""},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("empty tautulli apiKeyFile accepted")
	}
}

func TestTautulliConfigValidationRequiresPlex(t *testing.T) {
	cfg := Config{
		Tautulli: &TautulliConfig{URL: "http://tautulli:8181", APIKeyFile: "/k"},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("tautulli without plex accepted")
	}
}
