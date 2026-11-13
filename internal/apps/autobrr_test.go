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

// autobrrFake is autobrr's download-client API as the probe transcript found it:
// GET masks both secrets as "<redacted>", POST accepts a duplicate name, and PUT
// is a whole-row replace that wipes a password or API key it is not sent.
type autobrrFake struct {
	mu      sync.Mutex
	rows    []Item
	nextID  int
	writes  int
	tests   int
	testErr bool
	login   int // the status /api/auth/login answers with
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

// The update must carry the whole object, secrets included: autobrr's PUT
// replaces the row, and only the literal "<redacted>" is read as "keep". A
// client that sent a GET's object back unchanged would wipe the password and
// the Arr key from the live row.
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

	// A second run with a rotated qBittorrent password: the fingerprint is what
	// makes it visible, because the stored one is never returned.
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

// autobrr accepts any number of clients with the same name, so a create has to
// match first: the user's own row with s5n's name is adopted, its type and host
// corrected, and no second row appears.
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

// A second run on the recorded id and fingerprint writes nothing, and still
// proves both clients with autobrr's own test.
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

// A changed fingerprint is an update even when every readable field matches:
// the API cannot show that the secret is stale, so owned.json has to say so.
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

// The update body's own shape: port and tls are pinned to the values that make
// autobrr use the host verbatim, and settings keeps whatever the user set.
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
