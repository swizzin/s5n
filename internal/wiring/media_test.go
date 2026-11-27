package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// fakeMediaStack is Plex + Tautulli + Seerr + Maintainerr: just enough of each API
// for the media steps to be provable without a cluster.
type fakeMediaStack struct {
	mu      sync.Mutex
	claimed bool
	machine string

	apiKey     string
	pmsIP      string
	pmsPort    int
	pmsID      string
	pmsToken   string
	apiEnabled bool
	writes     int
	// acceptToken is the token Plex accepts; tokenChecks counts authenticated calls,
	// so a test can prove the token was verified rather than assumed.
	acceptToken string
	tokenChecks int
	// hideToken makes get_settings answer pms_token null, as Tautulli's
	// do-not-download rule does.
	hideToken bool
	// cm models the wiring ConfigMap, so owned.json survives across runs.
	cm map[string]string
	// tokenFile is the mounted Secret's plex-token path, so a rotation test can
	// rewrite it between runs.
	tokenFile string

	seerrUp bool
	// initialized is Seerr's own "an admin exists" flag, which a migrated
	// Seerr brings in the settings its snapshot carried.
	initialized bool
	// seerrFailFrom makes every public-settings call from that number on answer 503:
	// a Seerr that passes readiness and then refuses the read.
	seerrReads    int
	seerrFailFrom int

	// Maintainerr's settings row, with its masked token and the writes seen.
	maintainerrSettings apps.MaintainerrSettings
	maintainerToken     string
	maintainerrSaves    int
	maintainerrPatches  int
}

func (f *fakeMediaStack) plexHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/library/sections" {
			// Authenticated like a real Plex: the header's token decides, and
			// /identity stays tokenless.
			f.tokenChecks++
			if r.Header.Get("X-Plex-Token") != f.acceptToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
			return
		}
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
			f.pmsToken = r.Form.Get("pms_token")
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
		var token any = f.pmsToken
		if f.hideToken {
			token = nil
		}
		writeJSONAny(w, map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{
			"General": map[string]any{"api_enabled": apiEnabled},
			"PMS": map[string]any{
				"pms_ip":         f.pmsIP,
				"pms_port":       f.pmsPort,
				"pms_identifier": f.pmsID,
				"pms_token":      token,
			},
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
		f.seerrReads++
		if !f.seerrUp || (f.seerrFailFrom > 0 && f.seerrReads >= f.seerrFailFrom) {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		writeJSONAny(w, map[string]any{"initialized": f.initialized})
	}
}

// maintainerrHandler is Maintainerr's settings API at v3.33.0: health, the masked
// row, the token endpoint, the merge PATCH, and the connection test.
func (f *fakeMediaStack) maintainerrHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/maintainerr")
		switch {
		case path == "/api/health/ready":
			writeJSONAny(w, map[string]any{"status": "ok", "database": "ok"})
		case path == "/api/settings" && r.Method == http.MethodGet:
			f.maintainerrGet(w)
		case path == "/api/settings" && r.Method == http.MethodPatch:
			f.maintainerrPatch(w, r)
		case path == "/api/settings/plex/token" && r.Method == http.MethodPost:
			f.maintainerrToken(w, r)
		case path == "/api/settings/test/plex" && r.Method == http.MethodGet:
			f.maintainerrTest(w)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *fakeMediaStack) maintainerrGet(w http.ResponseWriter) {
	out := f.maintainerrSettings
	out.PlexAuthToken = nil
	if f.maintainerToken != "" {
		masked := f.maintainerToken[:3] + "..." + f.maintainerToken[len(f.maintainerToken)-3:]
		out.PlexAuthToken = &masked
	}
	writeJSONAny(w, out)
}

func (f *fakeMediaStack) maintainerrToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"plex_auth_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSONAny(w, map[string]any{"status": "NOK", "code": 0, "message": "Plex auth token is required"})
		return
	}
	f.maintainerToken = body.Token
	f.maintainerrSaves++
	writeJSONAny(w, map[string]any{"status": "OK", "code": 1, "message": "Success"})
}

// maintainerrPatch merges a body over the row, refusing a server change until a
// token is stored.
func (f *fakeMediaStack) maintainerrPatch(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, ok := body["plex_hostname"]; ok && f.maintainerToken == "" {
		writeJSONAny(w, map[string]any{"status": "NOK", "code": 0,
			"message": "Authenticate with Plex before saving Plex server settings."})
		return
	}
	f.maintainerrSettings = applyMaintainerrPatch(f.maintainerrSettings, body)
	f.maintainerrPatches++
	writeJSONAny(w, map[string]any{"status": "OK", "code": 1, "message": "Success"})
}

// applyMaintainerrPatch merges a PATCH body; an absent key is left alone.
func applyMaintainerrPatch(row apps.MaintainerrSettings, body map[string]any) apps.MaintainerrSettings {
	if v, ok := body["plex_hostname"].(string); ok {
		row.PlexHostname = v
	}
	if v, ok := body["plex_port"].(float64); ok {
		row.PlexPort = int(v)
	}
	if v, ok := body["plex_ssl"].(float64); ok {
		row.PlexSSL = int(v)
	}
	if v, ok := body["plex_manual_mode"].(float64); ok {
		row.PlexManualMode = int(v)
	}
	if v, ok := body["plex_name"].(string); ok {
		row.PlexName = v
	}
	if v, ok := body["media_server_type"].(string); ok {
		row.MediaServerType = v
	}
	return row
}

func (f *fakeMediaStack) maintainerrTest(w http.ResponseWriter) {
	if f.maintainerToken == "" {
		writeJSONAny(w, map[string]any{"status": "NOK", "code": 0,
			"message": "Authenticate with Plex before testing the connection."})
		return
	}
	writeJSONAny(w, map[string]any{"status": "OK", "code": 1, "message": "1.43.5"})
}

func writeJSONAny(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// mediaPlexToken is the token the harness mounts as the Secret's plex-token.
const mediaPlexToken = "plex-token-abc123"

// mediaSeerrPublicURL is <app>.<domain>, never the Service the Job dials.
const mediaSeerrPublicURL = "seerr.example.test"

// newMediaHarness wires fake Plex, Tautulli and Seerr only, so the media steps are
// asserted apart from the rest. The token file is present and non-empty; tests
// about a missing token repoint it.
func newMediaHarness(t *testing.T, claimed bool) (*Runner, *fakeMediaStack) {
	t.Helper()
	f := &fakeMediaStack{claimed: claimed, machine: "machine-123", apiKey: "tautulli-key", seerrUp: true}
	plex := httptest.NewServer(f.plexHandler())
	tautulli := httptest.NewServer(f.tautulliHandler())
	seerr := httptest.NewServer(f.seerrHandler())
	maintainerr := httptest.NewServer(f.maintainerrHandler())
	t.Cleanup(func() { plex.Close(); tautulli.Close(); seerr.Close(); maintainerr.Close() })

	dir := t.TempDir()
	apiKeyFile := dir + "/tautulli.key"
	if err := os.WriteFile(apiKeyFile, []byte(f.apiKey), 0o600); err != nil {
		t.Fatal(err)
	}
	plexTokenFile := dir + "/plex-token"
	// Plex accepts exactly the Secret's token, so a test can rotate the file.
	f.acceptToken = mediaPlexToken
	f.tokenFile = plexTokenFile
	// A trailing newline, as a Secret patched from a shell prompt usually has.
	if err := os.WriteFile(plexTokenFile, []byte(mediaPlexToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		TimeoutSeconds: 10,
		Plex:           &PlexConfig{URL: plex.URL},
		Tautulli:       &TautulliConfig{URL: tautulli.URL, APIKeyFile: apiKeyFile, PlexTokenFile: plexTokenFile},
		Seerrs:         []SeerrConfig{{Name: "seerr", URL: seerr.URL, PublicURL: mediaSeerrPublicURL}},
		Maintainerr: &MaintainerrConfig{
			URL: maintainerr.URL, URLBase: "/maintainerr",
			PlexTokenFile: plexTokenFile,
		},
	}
	runner := &Runner{
		Config: cfg,
		// A ConfigMap stand-in, so owned.json (and the Plex token fingerprint
		// recorded in it) survives across runs the way it does in a cluster.
		Publish: func(_ context.Context, data map[string]string) error {
			if f.cm == nil {
				f.cm = map[string]string{}
			}
			for k, v := range data {
				f.cm[k] = v
			}
			return nil
		},
		LoadOwned: func(context.Context) ([]byte, error) {
			v, ok := f.cm["owned.json"]
			if !ok {
				return nil, nil
			}
			return []byte(v), nil
		},
	}
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
	} else if s.Detail != "machineIdentifier=machine-123" {
		// An already-claimed server needs no claim warning.
		t.Fatalf("plex/claim detail = %q, want the machineIdentifier only", s.Detail)
	}
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("tautulli/plex = %+v, want ok/created", s)
	}
	if f.pmsID != "machine-123" {
		t.Fatalf("tautulli PMS identifier = %q", f.pmsID)
	}
	if f.pmsToken != mediaPlexToken {
		t.Fatalf("tautulli PMS token = %q, want the mounted plex-token", f.pmsToken)
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
	} else {
		// With Seerr deployed, the pending row says what skipping the claim costs.
		if !strings.HasPrefix(s.Detail, "Plex is not claimed yet: ") {
			t.Fatalf("plex/claim detail = %q, want it to start with the claim state", s.Detail)
		}
		if !strings.Contains(s.Detail, "whoever signs in to Seerr first becomes its owner") {
			t.Fatalf("plex/claim detail = %q, want the Seerr-owner consequence", s.Detail)
		}
		if !strings.HasSuffix(s.Detail, "https://plex.tv/claim") {
			t.Fatalf("plex/claim detail = %q, want it to end on the claim URL", s.Detail)
		}
	}
	// The claim signs the server in to Plex.tv; the PMS link uses plex-token, so it
	// wires regardless of claim state.
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

// Without Seerr the pending row keeps the narrow sentence and never mentions it.
func TestWiringUnclaimedPlexWithoutSeerrDoesNotMentionAnOwner(t *testing.T) {
	r, _ := newMediaHarness(t, false)
	r.Config.Seerrs = nil
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	s := step(res, "plex/claim")
	if s == nil || s.Result != "pending" {
		t.Fatalf("plex/claim = %+v, want pending (no claim token)", s)
	}
	const want = "no claim token (or it expired before Plex started): Plex is not signed in to a Plex.tv account"
	if s.Detail != want {
		t.Fatalf("plex/claim detail = %q, want %q", s.Detail, want)
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

// No Plex token: the link is pending, nothing is written to Tautulli.
func TestWiringTautulliPlexPendingWithoutToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"no token file configured", func(t *testing.T) string { return "" }},
		{"token file absent", func(t *testing.T) string { return t.TempDir() + "/plex-token" }},
		{"token file empty", func(t *testing.T) string {
			p := t.TempDir() + "/plex-token"
			if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, f := newMediaHarness(t, true)
			r.Config.Tautulli.PlexTokenFile = tc.path(t)

			res, err := r.Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v\n%s", err, Summary(res))
			}
			s := step(res, "tautulli/plex")
			if s == nil || s.Result != "pending" {
				t.Fatalf("tautulli/plex = %+v, want pending", s)
			}
			const want = "no plex-token in the release Secret: box reads it from Plex on the next apply"
			if s.Detail != want {
				t.Fatalf("tautulli/plex detail = %q, want %q", s.Detail, want)
			}
			if f.writes != 0 {
				t.Fatalf("Tautulli was written %d time(s) without a Plex token", f.writes)
			}
		})
	}
}

// No token on an unclaimed server: the reason is the claim, and box takes the token
// from a claimed server's settings on the next apply.
func TestWiringTautulliPlexPendingWithoutTokenOnAnUnclaimedServer(t *testing.T) {
	r, f := newMediaHarness(t, false)
	r.Config.Tautulli.PlexTokenFile = t.TempDir() + "/plex-token" // absent

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	s := step(res, "tautulli/plex")
	if s == nil || s.Result != "pending" {
		t.Fatalf("tautulli/plex = %+v, want pending", s)
	}
	const want = "Plex is not claimed yet: claim it at https://plex.tv/claim, then apply again to connect Tautulli"
	if s.Detail != want {
		t.Fatalf("tautulli/plex detail = %q, want %q", s.Detail, want)
	}
	if f.writes != 0 {
		t.Fatalf("Tautulli was written %d time(s) without a Plex token", f.writes)
	}
}

// A token file that cannot be read is an error: fail closed.
func TestWiringTautulliPlexErrorsOnUnreadableToken(t *testing.T) {
	r, f := newMediaHarness(t, true)
	r.Config.Tautulli.PlexTokenFile = t.TempDir() // a directory: EISDIR, not ENOENT

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with an unreadable Plex token file\n%s", Summary(res))
	}
	if f.writes != 0 {
		t.Fatalf("Tautulli was written %d time(s) after a token read failure", f.writes)
	}
}

// noTokenAnywhere fails when the token reached the receipt: it must exist only in
// the Secret and the request to Tautulli.
func noTokenAnywhere(t *testing.T, res *Result, token string) {
	t.Helper()
	if strings.Contains(Summary(res), token) {
		t.Fatalf("summary.txt contains the Plex token:\n%s", Summary(res))
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("receipt error contains the Plex token: %q", res.Error)
	}
	for _, s := range res.Steps {
		if strings.Contains(s.Detail, token) {
			t.Fatalf("step %s detail contains the Plex token: %q", s.Name, s.Detail)
		}
		for _, d := range s.Drift {
			if strings.Contains(d.Old, token) || strings.Contains(d.New, token) {
				t.Fatalf("step %s drift contains the Plex token", s.Name)
			}
		}
	}
}

// A token Plex accepts is verified against Plex before Tautulli is configured.
func TestWiringProvesThePlexTokenAgainstPlex(t *testing.T) {
	r, f := newMediaHarness(t, true)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "tautulli/plex"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("tautulli/plex = %+v, want ok/created", s)
	}
	if f.tokenChecks == 0 {
		t.Fatal("the wiring never called Plex's authenticated API: the token was assumed, not proven")
	}
	if f.pmsToken != mediaPlexToken {
		t.Fatalf("Tautulli PMS token = %q, want the mounted plex-token", f.pmsToken)
	}
	noTokenAnywhere(t, res, mediaPlexToken)
}

// A token Plex rejects is a credential error: the step fails token-free, Tautulli is
// never written, and the run stops.
func TestWiringRejectedPlexTokenFailsBeforeAnyTautulliWrite(t *testing.T) {
	r, f := newMediaHarness(t, true)
	// Plex now only accepts a different token: the mounted one is no longer
	// valid for this server.
	f.acceptToken = "rotated-elsewhere"

	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with a token Plex rejects\n%s", Summary(res))
	}
	if res.Status != "failed" {
		t.Fatalf("receipt status = %q, want failed", res.Status)
	}
	s := step(res, "tautulli/plex")
	if s == nil {
		t.Fatalf("no tautulli/plex step in the receipt\n%s", Summary(res))
	}
	if s.Result != "failed" {
		t.Fatalf("tautulli/plex result = %q, want failed", s.Result)
	}
	const wantDetail = "plex-token rejected by Plex (401)"
	if s.Detail != wantDetail {
		t.Fatalf("tautulli/plex detail = %q, want %q", s.Detail, wantDetail)
	}
	if f.writes != 0 {
		t.Fatalf("Tautulli was written %d time(s) with a token Plex rejects", f.writes)
	}
	noTokenAnywhere(t, res, mediaPlexToken)
	if s := step(res, "tautulli/ready"); s != nil {
		t.Fatal("Tautulli was contacted before the Plex token was proven")
	}
	if s := step(res, "seerr/ready"); s != nil {
		t.Fatal("the run continued past a rejected Plex token")
	}
	// Nothing was proven, so no fingerprint is recorded.
	var rec OwnedRecord
	if raw, ok := f.cm["owned.json"]; ok {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("owned.json: %v", err)
		}
	}
	if got := rec.TautulliPlexTokenFingerprint(); got != "" {
		t.Fatalf("rejected token recorded a fingerprint: %q", got)
	}
}

// An unclaimed server refuses every token, so a 401 there is the claim's absence,
// not a bad token: the row says so once and nothing is written with it.
func TestWiringUnclaimedPlexRefusesTokensAndSaysWhy(t *testing.T) {
	r, f := newMediaHarness(t, false)
	f.acceptToken = "a-token-no-unclaimed-server-knows"

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("an unclaimed server's refusal failed the run: %v\n%s", err, Summary(res))
	}
	const want = "Plex is not claimed yet, so it refuses every server token: claim it at https://plex.tv/claim, then run box apply"
	for _, name := range []string{"tautulli/plex", "maintainerr/plex"} {
		s := step(res, name)
		if s == nil || s.Result != "pending" {
			t.Fatalf("%s = %+v, want pending", name, s)
		}
		if s.Detail != want {
			t.Fatalf("%s detail = %q, want %q", name, s.Detail, want)
		}
	}
	if f.writes != 0 {
		t.Fatalf("Tautulli was written %d time(s) with a token its server refuses", f.writes)
	}
	if f.maintainerrSaves != 0 || f.maintainerrPatches != 0 {
		t.Fatalf("Maintainerr was written with a token its server refuses")
	}
	noTokenAnywhere(t, res, mediaPlexToken)
}

// Tautulli hides pms_token, so the fingerprint in owned.json is the only record of
// what was last written: a changed one rewrites the link.
func TestWiringWithheldTokenDetectsRotation(t *testing.T) {
	r, f := newMediaHarness(t, true)
	ctx := context.Background()
	// A real Tautulli withholds PMS_TOKEN.
	f.hideToken = true

	first, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	if s := step(first, "tautulli/plex"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("first tautulli/plex = %+v, want ok/created", s)
	}
	var rec OwnedRecord
	raw := f.cm["owned.json"]
	if raw == "" {
		t.Fatal("first run recorded no owned.json")
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("owned.json: %v", err)
	}
	if got, want := rec.TautulliPlexTokenFingerprint(), apps.PlexTokenFingerprint(mediaPlexToken); got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
	if strings.Contains(raw, mediaPlexToken) {
		t.Fatalf("owned.json contains the Plex token: %s", raw)
	}

	// Same token, still hidden: the fingerprint makes this a no-op.
	writes := f.writes
	second, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if s := step(second, "tautulli/plex"); s == nil || s.Detail != "unchanged" {
		t.Fatalf("second tautulli/plex = %+v, want unchanged", s)
	}
	if got := f.writes; got != writes {
		t.Fatalf("second run wrote to Tautulli %d time(s)", got-writes)
	}

	// Rotate the Secret's token and Plex's accepted token together.
	const rotated = mediaPlexToken + "-rotated"
	if err := os.WriteFile(f.tokenFile, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.acceptToken = rotated
	writes = f.writes
	third, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run after rotation: %v\n%s", err, Summary(third))
	}
	if s := step(third, "tautulli/plex"); s == nil || s.Detail != "updated" {
		t.Fatalf("tautulli/plex after rotation = %+v, want updated", s)
	}
	if got := f.writes; got != writes+1 {
		t.Fatalf("rotation caused %d write(s), want 1", got-writes)
	}
	if f.pmsToken != rotated {
		t.Fatalf("Tautulli PMS token = %q, want the rotated token", f.pmsToken)
	}
	if err := json.Unmarshal([]byte(f.cm["owned.json"]), &rec); err != nil {
		t.Fatalf("owned.json after rotation: %v", err)
	}
	if got, want := rec.TautulliPlexTokenFingerprint(), apps.PlexTokenFingerprint(rotated); got != want {
		t.Fatalf("fingerprint after rotation = %q, want %q", got, want)
	}
	noTokenAnywhere(t, third, rotated)

	// And the new fingerprint makes the next run a no-op again.
	writes = f.writes
	fourth, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("fourth run: %v\n%s", err, Summary(fourth))
	}
	if s := step(fourth, "tautulli/plex"); s == nil || s.Detail != "unchanged" {
		t.Fatalf("fourth tautulli/plex = %+v, want unchanged", s)
	}
	if got := f.writes; got != writes {
		t.Fatalf("fourth run wrote to Tautulli %d time(s)", got-writes)
	}
}

// A failed owned.json read must stop the run (AGENTS.md invariant 11): without the
// record the Job cannot tell an unchanged hidden token from a rotated one.
func TestWiringFailsClosedWhenOwnedStateCannotBeRead(t *testing.T) {
	r, f := newMediaHarness(t, true)
	f.hideToken = true
	r.LoadOwned = func(context.Context) ([]byte, error) {
		return nil, errors.New("kubernetes API: 503: unavailable")
	}
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded although owned.json could not be read\n%s", Summary(res))
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "wiring/owned") {
		t.Fatalf("receipt = %+v", res)
	}
	if f.writes != 0 {
		t.Fatalf("Tautulli was written %d time(s) after a failed owned.json read", f.writes)
	}
	if _, ok := f.cm["owned.json"]; ok {
		t.Fatal("a failed read published an owned.json")
	}
}
