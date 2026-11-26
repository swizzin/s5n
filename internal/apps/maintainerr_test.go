package apps

// The fake of Maintainerr's settings API at v3.33.0, including its rule: a PATCH that
// moves the server is refused until a token is stored.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testMaintainerrToken = "synthetic-plex-token-42"

// fakeMaintainerr is one settings row and the routes that write it.
type fakeMaintainerr struct {
	mu sync.Mutex
	// settings is the masked row GET /api/settings answers.
	settings MaintainerrSettings
	// storedToken is the token the app holds, which the mask is made from.
	storedToken string
	// tokenSaves and patches count writes, so a second run can be proved silent.
	tokenSaves, patches int
	// testFails makes the connection test answer NOK.
	testAnswer string
	testFails  bool
	// failPatch makes every PATCH answer 500.
	failPatch bool
}

func (f *fakeMaintainerr) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/maintainerr")
		switch {
		case path == "/api/health/ready" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"status": "ok", "database": "ok"})
		case path == "/api/settings" && r.Method == http.MethodGet:
			f.getSettings(w)
		case path == "/api/settings" && r.Method == http.MethodPatch:
			f.patchSettings(w, r)
		case path == "/api/settings/plex/token" && r.Method == http.MethodPost:
			f.saveToken(w, r)
		case path == "/api/settings/test/plex" && r.Method == http.MethodGet:
			f.testPlex(w)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// getSettings answers the row with the token masked the way the app masks it.
func (f *fakeMaintainerr) getSettings(w http.ResponseWriter) {
	out := f.settings
	out.PlexAuthToken = nil
	if f.storedToken != "" {
		masked := "****"
		if len(f.storedToken) > 6 {
			masked = f.storedToken[:3] + "..." + f.storedToken[len(f.storedToken)-3:]
		}
		out.PlexAuthToken = &masked
	}
	writeJSON(w, out)
}

// saveToken is the app's own token endpoint: an empty one is refused.
func (f *fakeMaintainerr) saveToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"plex_auth_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, map[string]any{"status": "NOK", "code": 0, "message": "Plex auth token is required"})
		return
	}
	f.storedToken = body.Token
	f.tokenSaves++
	writeJSON(w, map[string]any{"status": "OK", "code": 1, "message": "Success"})
}

// patchSettings merges a body, refusing a server change until a token is stored.
func (f *fakeMaintainerr) patchSettings(w http.ResponseWriter, r *http.Request) {
	if f.failPatch {
		http.Error(w, "cannot save", http.StatusInternalServerError)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, ok := body["plex_hostname"]; ok && f.storedToken == "" {
		writeJSON(w, map[string]any{"status": "NOK", "code": 0,
			"message": "Authenticate with Plex before saving Plex server settings."})
		return
	}
	f.apply(body)
	f.patches++
	writeJSON(w, map[string]any{"status": "OK", "code": 1, "message": "Success"})
}

// testPlex is the app's own connection test.
func (f *fakeMaintainerr) testPlex(w http.ResponseWriter) {
	if f.storedToken == "" {
		writeJSON(w, map[string]any{"status": "NOK", "code": 0,
			"message": "Authenticate with Plex before testing the connection."})
		return
	}
	if f.testFails {
		writeJSON(w, map[string]any{"status": "NOK", "code": 0,
			"message": "Failed to connect to Plex. Verify host and credentials."})
		return
	}
	answer := f.testAnswer
	if answer == "" {
		answer = "1.43.5.11079"
	}
	writeJSON(w, map[string]any{"status": "OK", "code": 1, "message": answer})
}

// apply merges a PATCH body over the stored row, as the app does.
func (f *fakeMaintainerr) apply(body map[string]any) {
	if s, ok := body["plex_hostname"].(string); ok {
		f.settings.PlexHostname = s
	}
	if n, ok := body["plex_port"].(float64); ok {
		f.settings.PlexPort = int(n)
	}
	if n, ok := body["plex_ssl"].(float64); ok {
		f.settings.PlexSSL = int(n)
	}
	if n, ok := body["plex_manual_mode"].(float64); ok {
		f.settings.PlexManualMode = int(n)
	}
	if s, ok := body["plex_name"].(string); ok {
		f.settings.PlexName = s
	}
	if s, ok := body["media_server_type"].(string); ok {
		f.settings.MediaServerType = s
	}
}

func newMaintainerrHarness(t *testing.T) (*Maintainerr, *fakeMaintainerr) {
	t.Helper()
	f := &fakeMaintainerr{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return NewMaintainerr(srv.URL, "/maintainerr"), f
}

func maintainerrSpec(token, fingerprint string) PlexConnection {
	return PlexConnection{Host: "plex", Port: 32400, Token: token, KnownFingerprint: fingerprint}
}

// A fresh Maintainerr gets the token first, then the connection, then the app's test.
func TestMaintainerrConfiguresAFreshPlexConnection(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	change, err := m.EnsurePlexConnection(context.Background(), maintainerrSpec(testMaintainerrToken, ""))
	if err != nil {
		t.Fatalf("EnsurePlexConnection: %v", err)
	}
	if change != Created {
		t.Fatalf("change = %v, want Created", change)
	}
	if f.tokenSaves != 1 || f.patches != 1 {
		t.Fatalf("the app saw %d token saves and %d patches, want 1 and 1", f.tokenSaves, f.patches)
	}
	if f.settings.PlexHostname != "plex" || f.settings.PlexPort != 32400 || f.settings.PlexSSL != 0 {
		t.Fatalf("the connection is %+v", f.settings)
	}
	// Manual mode keeps the in-cluster address instead of a plex.direct name.
	if f.settings.PlexManualMode != 1 {
		t.Fatalf("plex_manual_mode = %d, want 1", f.settings.PlexManualMode)
	}
	if f.settings.MediaServerType != "plex" {
		t.Fatalf("media_server_type = %q, want plex", f.settings.MediaServerType)
	}
	if f.settings.PlexName != maintainerrPlexName {
		t.Fatalf("plex_name = %q, want %q", f.settings.PlexName, maintainerrPlexName)
	}
}

// The same token and connection is a no-op: the fingerprint decides through the
// mask.
func TestMaintainerrPlexConnectionIsIdempotent(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	ctx := context.Background()
	if _, err := m.EnsurePlexConnection(ctx, maintainerrSpec(testMaintainerrToken, "")); err != nil {
		t.Fatal(err)
	}
	saves, patches := f.tokenSaves, f.patches
	change, err := m.EnsurePlexConnection(ctx,
		maintainerrSpec(testMaintainerrToken, PlexTokenFingerprint(testMaintainerrToken)))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if change != Unchanged {
		t.Fatalf("change = %v, want Unchanged", change)
	}
	if f.tokenSaves != saves || f.patches != patches {
		t.Fatalf("a second run wrote: %d token saves (%d), %d patches (%d)",
			f.tokenSaves, saves, f.patches, patches)
	}
}

// A rotated token is written again: the fingerprint is the difference.
func TestMaintainerrNoticesARotatedPlexToken(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	ctx := context.Background()
	if _, err := m.EnsurePlexConnection(ctx, maintainerrSpec(testMaintainerrToken, "")); err != nil {
		t.Fatal(err)
	}
	const rotated = testMaintainerrToken + "-rotated"
	change, err := m.EnsurePlexConnection(ctx, maintainerrSpec(rotated, PlexTokenFingerprint(testMaintainerrToken)))
	if err != nil {
		t.Fatalf("run after a rotation: %v", err)
	}
	if change != Updated {
		t.Fatalf("change = %v, want Updated", change)
	}
	if f.storedToken != rotated {
		t.Fatalf("the app holds %q, want the rotated token", f.storedToken)
	}
	// The fingerprint is now the rotated token's, so the next run is quiet.
	saves := f.tokenSaves
	if _, err := m.EnsurePlexConnection(ctx,
		maintainerrSpec(rotated, PlexTokenFingerprint(rotated))); err != nil {
		t.Fatal(err)
	}
	if f.tokenSaves != saves {
		t.Fatalf("a run after the rotation's record wrote the token again")
	}
}

// With the connection right and a token held, a run with no fingerprint writes
// nothing.
func TestMaintainerrLeavesAMatchingConnectionAlone(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	f.storedToken = testMaintainerrToken
	f.settings = MaintainerrSettings{
		MediaServerType: "plex", PlexHostname: "plex", PlexPort: 32400,
		PlexSSL: 0, PlexManualMode: 1,
	}
	change, err := m.EnsurePlexConnection(context.Background(),
		maintainerrSpec(testMaintainerrToken, ""))
	if err != nil {
		t.Fatalf("EnsurePlexConnection: %v", err)
	}
	// No fingerprint on record means "unknown", which is a write: the same
	// reading Tautulli's link takes of an unrecorded token.
	if change != Updated || f.tokenSaves != 1 {
		t.Fatalf("change = %v after %d token saves, want one re-write of an unrecorded token", change, f.tokenSaves)
	}
}

// A migrated Maintainerr's connection moves to the release's: an update.
func TestMaintainerrMovesAMigratedConnection(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	f.storedToken = "source-token"
	f.settings = MaintainerrSettings{
		MediaServerType: "plex", PlexName: "source-plex", PlexHostname: "plex.media.example.test",
		PlexPort: 32400, PlexSSL: 1, PlexManualMode: 0,
	}
	change, err := m.EnsurePlexConnection(context.Background(),
		maintainerrSpec(testMaintainerrToken, ""))
	if err != nil {
		t.Fatalf("EnsurePlexConnection: %v", err)
	}
	if change != Updated {
		t.Fatalf("change = %v, want Updated", change)
	}
	if f.settings.PlexHostname != "plex" || f.settings.PlexSSL != 0 || f.settings.PlexManualMode != 1 {
		t.Fatalf("the connection is %+v, want the release's address over plain HTTP in manual mode", f.settings)
	}
}

// A connection the app cannot prove fails with its own answer.
func TestMaintainerrProvesTheConnection(t *testing.T) {
	// A NOK test is a failed step, not a configured app.
	hostile := &fakeMaintainerr{testFails: true}
	srv := httptest.NewServer(hostile.handler())
	t.Cleanup(srv.Close)
	_, err := NewMaintainerr(srv.URL, "/maintainerr").EnsurePlexConnection(context.Background(),
		maintainerrSpec(testMaintainerrToken, ""))
	if err == nil {
		t.Fatal("a connection test that answers NOK was reported as success")
	}
	if !strings.Contains(err.Error(), "prove the Plex connection") {
		t.Fatalf("error = %v, want it to name the proof", err)
	}

	// And a write the app refuses is an error naming the write, never a silent
	// half-configured connection.
	broken := &fakeMaintainerr{failPatch: true}
	srv2 := httptest.NewServer(broken.handler())
	t.Cleanup(srv2.Close)
	_, err = NewMaintainerr(srv2.URL, "/maintainerr").EnsurePlexConnection(context.Background(),
		maintainerrSpec(testMaintainerrToken, ""))
	if err == nil {
		t.Fatal("a PATCH the app refused was reported as success")
	}
	if !strings.Contains(err.Error(), "save the Plex connection") {
		t.Fatalf("error = %v, want it to name the connection write", err)
	}
}

// An empty token is refused before any request.
func TestMaintainerrRefusesAnEmptyToken(t *testing.T) {
	m, f := newMaintainerrHarness(t)
	if _, err := m.EnsurePlexConnection(context.Background(), PlexConnection{Host: "plex", Port: 32400}); err == nil {
		t.Fatal("an empty token was accepted")
	}
	if f.tokenSaves != 0 || f.patches != 0 {
		t.Fatalf("the app was written to without a token: %d saves, %d patches", f.tokenSaves, f.patches)
	}
}

// A case-folded or padded host still compares as the same server.
func TestMaintainerrSettingsNormaliseTheHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"plex", "plex"},
		{" Plex ", "plex"},
		{"http://plex", "plex"},
		{"https://plex.example.test", "plex.example.test"},
	} {
		s := MaintainerrSettings{PlexHostname: tc.raw}
		if got := s.PlexHost(); got != tc.want {
			t.Fatalf("PlexHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
