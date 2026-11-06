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
	pmsToken   string
	apiEnabled bool
	writes     int
	// acceptToken is the server token Plex accepts at /library/sections; the
	// harness sets it to the token the release Secret carries. tokenChecks
	// counts authenticated calls, so a test can prove the token was verified
	// rather than assumed.
	acceptToken string
	tokenChecks int
	// hideToken makes get_settings answer pms_token as null, as Tautulli's
	// PMS_TOKEN do-not-download rule does on a real install.
	hideToken bool
	// cm models the `<release>-wiring` ConfigMap across runs, so owned.json
	// (and the token fingerprint inside it) survives exactly as in a cluster.
	cm map[string]string
	// tokenFile is the mounted Secret's plex-token path, so a rotation test can
	// rewrite it between runs.
	tokenFile string

	seerrUp bool
}

func (f *fakeMediaStack) plexHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/library/sections" {
			// Authenticated, exactly like a real Plex: the token in the
			// header decides, and /identity below stays tokenless so a test
			// cannot pass by reading it.
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

// mediaPlexToken is the Plex server token the media harness mounts as the
// release Secret's plex-token key, standing in for a token taken from a
// claimed server.
const mediaPlexToken = "plex-token-abc123"

// newMediaHarness builds a Runner wired to fake Plex, Tautulli and Seerr
// servers only (no qBittorrent/Arr/Prowlarr), so the media steps can be
// asserted in isolation from the rest of internal/wiring's coverage. The Plex
// token file is present and non-empty; tests about a missing token repoint
// Config.Tautulli.PlexTokenFile at an absent, empty or unreadable path.
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
	plexTokenFile := dir + "/plex-token"
	// Plex accepts exactly the token the release Secret carries, so a test can
	// rotate the file and watch the wiring notice.
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
		Seerr:          &SeerrConfig{URL: seerr.URL},
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
	}
	// The claim token only signs the server in to Plex.tv; the PMS link uses
	// the release Secret's plex-token, so it wires regardless of claim state.
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

// No Plex token: the PMS link is pending, not configured tokenless and not a
// failure of the release. Nothing may be written to Tautulli.
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
			const want = "no Plex token: set plex-token (from a claimed server) to let the wiring Job connect Tautulli"
			if s.Detail != want {
				t.Fatalf("tautulli/plex detail = %q, want %q", s.Detail, want)
			}
			if f.writes != 0 {
				t.Fatalf("Tautulli was written %d time(s) without a Plex token", f.writes)
			}
		})
	}
}

// A token file that exists but cannot be read is an error: fail closed rather
// than quietly configuring a tokenless link.
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

// noTokenAnywhere fails when the Plex token reached the receipt in any form.
// The receipt is a ConfigMap any user in the namespace can read, so the token
// must exist only in the Secret and in the request to Tautulli.
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

// A token Plex accepts is verified against Plex itself before Tautulli is
// configured, and nothing about it reaches the receipt.
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

// A token Plex rejects (expired, revoked, wrong server) is a credential error:
// the step fails with a token-free detail, Tautulli is never written, and the
// run stops instead of reporting a link that Plex would refuse.
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
	// Nothing was proven, so no fingerprint may be recorded: it would make the
	// next run treat this token as current.
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

// Tautulli hides pms_token, so the fingerprint in owned.json is the only record
// of what was last written. Same fingerprint: no write. Different fingerprint:
// the Secret rotated, so the link is rewritten even though the live value is
// unreadable. This is the case that used to be silently skipped forever.
func TestWiringWithheldTokenDetectsRotation(t *testing.T) {
	r, f := newMediaHarness(t, true)
	ctx := context.Background()
	// A real Tautulli withholds PMS_TOKEN from get_settings.
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

	// Same token, still hidden: the recorded fingerprint makes this a no-op.
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

// The fingerprint lives in owned.json, whose read failure already stops the run
// (AGENTS.md invariant 11). That must hold here too: without the record the Job
// cannot tell an unchanged hidden token from a rotated one, and guessing would
// either rewrite every run or never push a rotation.
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
