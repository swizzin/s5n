package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// autobrrFake is the API as the probe found it: GET masks secrets as "<redacted>",
// POST accepts a duplicate name, and PUT replaces the whole row.
type autobrrFake struct {
	mu        sync.Mutex
	rows      []Item
	nextID    int
	writes    int
	tests     int
	testErr   bool
	login     int    // the status /api/auth/login answers with
	loginBody string // the body it answers that status with, when set
}

func (f *autobrrFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/autobrr/api/auth/login":
			if f.login == 0 {
				f.login = http.StatusNoContent
			}
			if f.login == http.StatusNoContent {
				http.SetCookie(w, &http.Cookie{Name: "autobrr_user_session", Value: "sess-1", Path: "/autobrr/"})
			}
			if f.loginBody != "" {
				w.WriteHeader(f.login)
				writeJSON(w, map[string]string{"message": f.loginBody})
				return
			}
			w.WriteHeader(f.login)
		case r.URL.Path == "/autobrr/api/healthz/readiness":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/autobrr/api/downloaders" && r.Method == http.MethodGet:
			writeJSON(w, maskedDownloaders(f.rows))
		case r.URL.Path == "/autobrr/api/downloaders/test":
			f.tests++
			if f.testErr {
				w.WriteHeader(http.StatusInternalServerError)
				writeJSON(w, map[string]string{"message": "error logging into client: dial tcp: connection refused"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/autobrr/api/downloaders" && r.Method == http.MethodPost:
			var body Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			body["id"] = f.nextID
			f.rows = append(f.rows, body)
			f.writes++
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, body)
		case r.URL.Path == "/autobrr/api/downloaders" && r.Method == http.MethodPut:
			var body Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, row := range f.rows {
				if row.Int("id") != body.Int("id") {
					continue
				}
				// The trap the client exists to avoid: an absent or empty
				// password/apikey overwrites the stored one.
				if body.Str("password") == "" {
					body["password"] = ""
				}
				settings, _ := body["settings"].(map[string]any)
				if settings == nil {
					body["settings"] = map[string]any{}
				} else if _, ok := settings["apikey"]; !ok {
					delete(settings, "apikey")
				}
				f.rows[i] = body
				f.writes++
				writeJSON(w, body)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// maskedDownloaders is the read path: neither secret is ever returned.
func maskedDownloaders(rows []Item) []Item {
	out := make([]Item, 0, len(rows))
	for _, row := range rows {
		c := row.Clone()
		c["password"] = "<redacted>"
		if settings, ok := c["settings"].(map[string]any); ok {
			if _, ok := settings["apikey"]; ok {
				settings["apikey"] = "<redacted>"
			}
		}
		out = append(out, c)
	}
	return out
}

func newAutobrrFake(t *testing.T) (*autobrrFake, *Autobrr) {
	t.Helper()
	f := &autobrrFake{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, NewAutobrr(srv.URL, "/autobrr", "s5n", "autobrr-pass-123")
}

func mustLogin(t *testing.T, a *Autobrr) {
	t.Helper()
	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
}

// The update carries the whole object, secrets included: PUT replaces the row and
// only "<redacted>" means keep.
func TestAutobrrEnsureDownloaderSendsTheWholeObjectSoPutDoesNotWipe(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	ctx := context.Background()

	res, err := a.EnsureDownloader(ctx, DownloaderSpec{
		Name: "qBittorrent", Type: "QBITTORRENT", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-pass-1",
	}, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.ID == 0 {
		t.Fatalf("create = %+v", res)
	}

	// A rotated password is visible only through the fingerprint.
	res, err = a.EnsureDownloader(ctx, DownloaderSpec{
		Name: "qBittorrent", Type: "QBITTORRENT", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-pass-2",
	}, res.ID, SecretFingerprint("qbit-pass-1"))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("a rotated password reported %s, want updated", res.Change)
	}
	if got := f.rows[0].Str("password"); got != "qbit-pass-2" {
		t.Fatalf("stored password = %q after the update, want the Secret's", got)
	}

	// An Arr client's key is the other secret that must survive every write.
	res, err = a.EnsureDownloader(ctx, DownloaderSpec{
		Name: "sonarr", Type: "SONARR", Host: "http://sonarr:8989/sonarr", APIKey: "sonarr-key-1",
	}, 0, "")
	if err != nil {
		t.Fatalf("create sonarr: %v", err)
	}
	if _, err := a.EnsureDownloader(ctx, DownloaderSpec{
		Name: "sonarr", Type: "SONARR", Host: "http://sonarr:8989/sonarr", APIKey: "sonarr-key-2",
	}, res.ID, SecretFingerprint("sonarr-key-1")); err != nil {
		t.Fatalf("update sonarr: %v", err)
	}
	for _, row := range f.rows {
		if row.Str("name") != "sonarr" {
			continue
		}
		settings, _ := row["settings"].(map[string]any)
		if settings["apikey"] != "sonarr-key-2" {
			t.Fatalf("stored apikey = %v after the update, want the Secret's", settings["apikey"])
		}
	}
}

// A migrated client keeps the operator's name, so only its address finds it: it is
// adopted and nothing is created beside it.
func TestAutobrrEnsureDownloaderAdoptsAMigratedClientByHost(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{{
		"id": 12, "name": "Sonarr4k", "type": "SONARR", "enabled": true,
		"host": "http://sonarr-4k:8990/sonarr-4k", "port": 0, "tls": false,
		"username": "", "password": "",
		"settings": map[string]any{"apikey": "sonarr-key"},
	}}

	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "sonarr-4k", Type: "SONARR", Host: "http://sonarr-4k:8990/sonarr-4k", APIKey: "sonarr-key",
	}, 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// Nothing is on record yet, so the key it holds cannot be known to be the
	// same one: the row is updated in place, once, rather than duplicated.
	if res.Change != Updated || res.ID != 12 {
		t.Fatalf("adopt = %+v, want an update on id 12", res)
	}
	if len(f.rows) != 1 {
		t.Fatalf("%d rows: the migrated client was duplicated instead of adopted", len(f.rows))
	}
	if len(res.Drift) != 0 {
		t.Fatalf("drift = %+v, want none: every readable field already matched", res.Drift)
	}
	if got := f.rows[0].Str("name"); got != "Sonarr4k" {
		t.Fatalf("the adopted client's name is %q, want the operator's own", got)
	}
}

// A bare host plus a port is the same address as the folded form: adopted, not
// duplicated.
func TestAutobrrEnsureDownloaderAdoptsTheMigratedQBittorrentClient(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{{
		"id": 2, "name": "qbit", "type": "QBITTORRENT", "enabled": true,
		"host": "qbittorrent", "port": 8080, "tls": false, "username": "", "password": "",
	}}

	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "qBittorrent", Type: "QBITTORRENT", Host: "http://qbittorrent:8080",
	}, 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.ID != 2 || len(f.rows) != 1 {
		t.Fatalf("adopt = %+v with %d rows, want id 2 and no second row", res, len(f.rows))
	}
	if got := f.rows[0].Str("host"); got != "http://qbittorrent:8080" {
		t.Fatalf("host = %q, want the client folded into one URL", got)
	}
	if got := f.rows[0].Int("port"); got != 0 {
		t.Fatalf("port = %d, want 0: the host carries it", got)
	}
}

// A deleted recorded id adopts the migrated row at the same address first; the
// receipt names the id it took.
func TestAutobrrEnsureDownloaderAdoptsByHostWhenTheRecordedClientIsGone(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{{
		"id": 12, "name": "Radarr4k", "type": "RADARR", "enabled": true,
		"host": "http://radarr-4k:7879/radarr-4k", "port": 0, "tls": false,
		"username": "", "password": "",
		"settings": map[string]any{"apikey": "radarr-key"},
	}}

	// owned.json keeps the key's fingerprint beside the id, so the repair does
	// not even rewrite the row it adopts.
	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "radarr-4k", Type: "RADARR", Host: "http://radarr-4k:7879/radarr-4k", APIKey: "radarr-key",
	}, 41, SecretFingerprint("radarr-key"))
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Recreated {
		t.Fatal("a deleted recorded id reported the client as recreated although the migrated one was adopted")
	}
	if res.ID != 12 || res.Change != Unchanged {
		t.Fatalf("adopt = %+v, want unchanged on id 12", res)
	}
	if len(f.rows) != 1 || f.writes != 0 {
		t.Fatalf("%d rows, %d writes: the deleted id made a second client beside the migrated one", len(f.rows), f.writes)
	}
	if !strings.Contains(res.Note, "12") || !strings.Contains(res.Note, "41") {
		t.Fatalf("note = %q, want the adopted id and the recorded one it replaced", res.Note)
	}
}

// A disabled client, another type, or TLS at the same host is not the one box
// feeds.
func TestAutobrrEnsureDownloaderOnlyAdoptsAnEnabledClientOfItsOwnType(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{
		{"id": 3, "name": "Sonarr4k", "type": "SONARR", "enabled": false,
			"host": "http://sonarr-4k:8990/sonarr-4k"},
		{"id": 5, "name": "something else", "type": "RADARR", "enabled": true,
			"host": "http://sonarr-4k:8990/sonarr-4k"},
		{"id": 6, "name": "Sonarr4k tls", "type": "SONARR", "enabled": true,
			"host": "sonarr-4k:8990/sonarr-4k", "tls": true},
	}

	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "sonarr-4k", Type: "SONARR", Host: "http://sonarr-4k:8990/sonarr-4k", APIKey: "sonarr-key",
	}, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.ID == 3 || res.ID == 5 || res.ID == 6 {
		t.Fatalf("create = %+v, want a new client rather than a disabled, foreign or TLS one", res)
	}
	if len(f.rows) != 4 {
		t.Fatalf("%d rows, want the three the operator has plus the one box made", len(f.rows))
	}
	for i, host := range []string{"http://sonarr-4k:8990/sonarr-4k", "http://sonarr-4k:8990/sonarr-4k", "sonarr-4k:8990/sonarr-4k"} {
		if got := f.rows[i].Str("host"); got != host {
			t.Fatalf("row %d's host = %q, want %q: nothing of the operator's is rewritten", f.rows[i].Int("id"), got, host)
		}
	}
}

// Two at one address: the lowest id is adopted and named, not a third created.
func TestAutobrrEnsureDownloaderAdoptsTheLowestOfTwoAtOneHost(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{
		{"id": 9, "name": "sonarr-4k", "type": "SONARR", "enabled": true,
			"host": "http://sonarr-4k:8990/sonarr-4k"},
		{"id": 3, "name": "Sonarr4k", "type": "SONARR", "enabled": true,
			"host": "http://sonarr-4k:8990/sonarr-4k/"},
	}

	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "sonarr-4k", Type: "SONARR", Host: "http://sonarr-4k:8990/sonarr-4k", APIKey: "sonarr-key",
	}, 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.ID != 3 {
		t.Fatalf("adopted id %d, want the lowest of 3 and 9", res.ID)
	}
	if len(f.rows) != 2 {
		t.Fatalf("%d rows, want the two that were there", len(f.rows))
	}
	if f.rows[0].Str("name") != "sonarr-4k" || !f.rows[0].Bool("enabled") {
		t.Fatalf("the client that was not adopted changed: %v", f.rows[0])
	}
	if !strings.Contains(res.Note, "3") || !strings.Contains(res.Note, "9") {
		t.Fatalf("note = %q, want both ids named", res.Note)
	}
}

// The user's row with s5n's name is adopted and corrected, not duplicated.
func TestAutobrrEnsureDownloaderMatchesBeforeCreating(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.rows = []Item{{
		"id": 4, "name": "sonarr", "type": "QBITTORRENT", "enabled": false,
		"host": "http://somewhere-else:8080", "username": "someone", "password": "their-pass",
		"settings": map[string]any{"rules": map[string]any{"enabled": true, "max_active_downloads": 3}},
	}}

	res, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "sonarr", Type: "SONARR", Host: "http://sonarr:8989/sonarr", APIKey: "sonarr-key",
	}, 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Change != Updated || res.ID != 4 {
		t.Fatalf("adopt = %+v, want updated on id 4", res)
	}
	if len(f.rows) != 1 {
		t.Fatalf("%d rows after adoption, want 1 (no duplicate)", len(f.rows))
	}
	row := f.rows[0]
	if row.Str("type") != "SONARR" || row.Str("host") != "http://sonarr:8989/sonarr" {
		t.Fatalf("row = %v, want the spec's type and host", row)
	}
	// Not owned: the user's own switch and throttling settings survive.
	if row.Bool("enabled") {
		t.Fatal("the update enabled a client the user had switched off")
	}
	settings, _ := row["settings"].(map[string]any)
	rules, _ := settings["rules"].(map[string]any)
	if rules["enabled"] != true || rules["max_active_downloads"] != float64(3) {
		t.Fatalf("rules = %v, want the user's own", rules)
	}
	// Drift is reported for what s5n restored, and never for the secrets.
	if len(res.Drift) == 0 {
		t.Fatal("adoption reported no drift for a row it rewrote")
	}
	for _, d := range res.Drift {
		if d.Field == "password" || strings.HasSuffix(d.Field, "apikey") {
			t.Fatalf("drift names a secret field: %+v", d)
		}
	}
}

// A second run writes nothing and still proves both clients.
func TestAutobrrEnsureDownloaderIsIdempotent(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	ctx := context.Background()
	spec := DownloaderSpec{Name: "radarr", Type: "RADARR", Host: "http://radarr:7878/radarr", APIKey: "radarr-key"}

	res, err := a.EnsureDownloader(ctx, spec, 0, "")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	writes := f.writes
	res, err = a.EnsureDownloader(ctx, spec, res.ID, SecretFingerprint("radarr-key"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Change != Unchanged {
		t.Fatalf("second run = %s, want unchanged", res.Change)
	}
	if f.writes != writes {
		t.Fatalf("second run made %d writes", f.writes-writes)
	}
	if f.tests != 2 {
		t.Fatalf("connection tests = %d, want one per run (the proof, not a write)", f.tests)
	}
}

// A changed fingerprint is an update even when every readable field matches.
func TestAutobrrEnsureDownloaderRewritesOnRotatedKey(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	ctx := context.Background()
	spec := DownloaderSpec{Name: "sonarr", Type: "SONARR", Host: "http://sonarr:8989/sonarr", APIKey: "new-key"}

	res, err := a.EnsureDownloader(ctx, spec, 0, "")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err = a.EnsureDownloader(ctx, spec, res.ID, SecretFingerprint("old-key"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("a rotated Arr key reported %s, want updated", res.Change)
	}
	if len(res.Drift) != 0 {
		t.Fatalf("drift = %+v, want none: only the secret changed", res.Drift)
	}
	if len(f.rows) != 1 {
		t.Fatalf("%d rows, want 1", len(f.rows))
	}
}

// The test comes before the write, so a client autobrr cannot reach is never
// created or overwritten.
func TestAutobrrEnsureDownloaderTestsBeforeWriting(t *testing.T) {
	f, a := newAutobrrFake(t)
	mustLogin(t, a)
	f.testErr = true

	_, err := a.EnsureDownloader(context.Background(), DownloaderSpec{
		Name: "qBittorrent", Type: "QBITTORRENT", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-pass",
	}, 0, "")
	if err == nil {
		t.Fatal("a failing connection test still created the client")
	}
	if f.writes != 0 || len(f.rows) != 0 {
		t.Fatalf("writes = %d, rows = %d, want none", f.writes, len(f.rows))
	}
	if !strings.Contains(err.Error(), "test autobrr downloader") {
		t.Fatalf("error = %v, want it to name the test", err)
	}
}

func TestAutobrrLoginRejectsCredentials(t *testing.T) {
	f, a := newAutobrrFake(t)
	f.login = http.StatusForbidden
	err := a.Login(context.Background())
	var rej *LoginRejectedError
	if !asLoginRejected(err, &rej) {
		t.Fatalf("error = %v (%T), want a LoginRejectedError", err, err)
	}
	if rej.App != "autobrr" || rej.Username != "s5n" {
		t.Fatalf("rejection = %+v", rej)
	}
}

// A 403 from an auth mode without password login is not a changed password: only one
// of the two is fixed by a restart (internal/http/auth.go:122-126 at v1.91.1).
func TestAutobrrLoginTellsAnAuthModeRefusalFromABadPassword(t *testing.T) {
	f, a := newAutobrrFake(t)
	f.login = http.StatusForbidden
	f.loginBody = "password login is unavailable in oidc authentication mode"
	err := a.Login(context.Background())
	if err == nil {
		t.Fatal("a refused password login reported success")
	}
	var rej *LoginRejectedError
	if asLoginRejected(err, &rej) {
		t.Fatalf("the auth mode's refusal was reported as %v, want the app's configuration named instead", rej)
	}
	var unavailable *LoginUnavailableError
	if !asLoginUnavailable(err, &unavailable) {
		t.Fatalf("error = %v (%T), want a LoginUnavailableError", err, err)
	}
	if unavailable.App != "autobrr" || unavailable.Mode != "oidc" {
		t.Fatalf("refusal = %+v, want autobrr in oidc mode", unavailable)
	}
	if msg := err.Error(); !strings.Contains(msg, "autobrr") || !strings.Contains(msg, "oidc") {
		t.Fatalf("error = %q, want it to name the app and the mode", msg)
	}
}

// asLoginUnavailable is asLoginRejected's twin, for the same reason.
func asLoginUnavailable(err error, target **LoginUnavailableError) bool {
	for err != nil {
		if u, ok := err.(*LoginUnavailableError); ok {
			*target = u
			return true
		}
		w, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}

// asLoginRejected is errors.As specialised for the tests here, so each app's
// test file does not import errors for one call.
func asLoginRejected(err error, target **LoginRejectedError) bool {
	for err != nil {
		if rej, ok := err.(*LoginRejectedError); ok {
			*target = rej
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// The body pins port and tls so the host is used verbatim; settings keeps the
// user's.
func TestAutobrrDesiredPinsPortAndKeepsSettings(t *testing.T) {
	live := Item{
		"id": 2, "name": "qBittorrent", "type": "QBITTORRENT", "enabled": true,
		"host": "qbittorrent", "port": 8080, "tls": true, "username": "admin",
		"settings": map[string]any{"rules": map[string]any{"enabled": true}},
	}
	desired, drift := autobrrDesired(live, DownloaderSpec{
		Name: "qBittorrent", Type: "QBITTORRENT", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-pass",
	})
	if desired.Int("port") != 0 || desired.Bool("tls") {
		t.Fatalf("port/tls = %v/%v, want 0/false: a legacy port would be appended to a host that has one",
			desired["port"], desired["tls"])
	}
	if desired.Str("host") != "http://qbittorrent:8080" || desired.Str("password") != "qbit-pass" {
		t.Fatalf("desired = %v", desired)
	}
	settings, _ := desired["settings"].(map[string]any)
	if settings["rules"] == nil {
		t.Fatal("the user's rules were dropped from the update body")
	}
	var fields []string
	for _, d := range drift {
		fields = append(fields, d.Field)
	}
	if fmt.Sprint(fields) != "[host port tls]" {
		t.Fatalf("drift = %v, want host, port, tls", fields)
	}
}

// Adoption compares case-blind scheme and name, no trailing slash, the path as
// stored, and a port beside the host as the same address.
func TestNormalizeDownloaderHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://sonarr-4k:8990/sonarr-4k", "http://sonarr-4k:8990/sonarr-4k"},
		{"HTTP://Sonarr-4K:8990/sonarr-4k/", "http://sonarr-4k:8990/sonarr-4k"},
		{"sonarr-4k:8990/sonarr-4k", "http://sonarr-4k:8990/sonarr-4k"},
		{"sonarr-4k:8990/Sonarr-4K", "http://sonarr-4k:8990/Sonarr-4K"},
		{"http://qbittorrent:8080", "http://qbittorrent:8080"},
		{"", ""},
	} {
		if got := normalizeDownloaderHost(tc.raw); got != tc.want {
			t.Fatalf("normalizeDownloaderHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
