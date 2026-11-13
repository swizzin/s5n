package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// appFakes are qui and autobrr as the wiring Job finds them: both keep their
// own user table, both answer a login and then a list of the objects s5n owns,
// and both mask the stored secrets. They record what was written so a test can
// prove a second run is a no-op.
type appFakes struct {
	mu         sync.Mutex
	quiRows    []apps.Item
	autoRows   []apps.Item
	quiNext    int
	autoNext   int
	quiWrites  int
	autoWrites int
	quiLogin   int // status the login answers with; 0 is 200
	autoLogin  int
}

func (f *appFakes) quiHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/qui")
		switch {
		case path == "/api/auth/login":
			if f.quiLogin != 0 {
				w.WriteHeader(f.quiLogin)
				writeJSON(w, map[string]string{"error": "Invalid credentials"})
				return
			}
			w.Header().Set("Set-Cookie", "qui_user_session=test; Path=/qui/; HttpOnly; Secure")
			writeJSON(w, map[string]any{"message": "Login successful"})
		case path == "/healthz/readiness":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case path == "/api/instances/" && r.Method == http.MethodGet:
			out := []apps.Item{}
			for _, row := range f.quiRows {
				c := row.Clone()
				delete(c, "password")
				out = append(out, c)
			}
			writeJSON(w, out)
		case path == "/api/instances/" && r.Method == http.MethodPost:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.quiNext++
			body["id"] = f.quiNext
			f.quiRows = append(f.quiRows, body)
			f.quiWrites++
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, body)
		case strings.HasPrefix(path, "/api/instances/") && strings.HasSuffix(path, "/test"):
			writeJSON(w, map[string]any{"connected": true, "message": "Connection successful"})
		case strings.HasPrefix(path, "/api/instances/") && r.Method == http.MethodPut:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, row := range f.quiRows {
				if row.Int("id") == body.Int("id") {
					f.quiRows[i] = body
					f.quiWrites++
					writeJSON(w, body)
					return
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *appFakes) autobrrHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/autobrr")
		switch {
		case path == "/api/auth/login":
			if f.autoLogin != 0 {
				w.WriteHeader(f.autoLogin)
				writeJSON(w, map[string]string{"message": "could not login: bad credentials"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "autobrr_user_session", Value: "test", Path: "/autobrr/"})
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/healthz/readiness":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case path == "/api/downloaders" && r.Method == http.MethodGet:
			out := []apps.Item{}
			for _, row := range f.autoRows {
				c := row.Clone()
				c["password"] = "<redacted>"
				if settings, ok := c["settings"].(map[string]any); ok {
					if _, ok := settings["apikey"]; ok {
						settings["apikey"] = "<redacted>"
					}
				}
				out = append(out, c)
			}
			writeJSON(w, out)
		case path == "/api/downloaders/test":
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/downloaders" && r.Method == http.MethodPost:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.autoNext++
			body["id"] = f.autoNext
			f.autoRows = append(f.autoRows, body)
			f.autoWrites++
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, body)
		case path == "/api/downloaders" && r.Method == http.MethodPut:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, row := range f.autoRows {
				if row.Int("id") == body.Int("id") {
					f.autoRows[i] = body
					f.autoWrites++
					writeJSON(w, body)
					return
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// withApps points the harness at a qui and an autobrr, with the same password
// files the chart mounts. The fake qBittorrent password is the one already in
// the harness's Secret file.
func (h *harness) withApps(t *testing.T) *appFakes {
	t.Helper()
	f := &appFakes{}
	qui := httptest.NewServer(f.quiHandler())
	auto := httptest.NewServer(f.autobrrHandler())
	t.Cleanup(func() { qui.Close(); auto.Close() })
	h.config.Qui = &QuiConfig{URL: qui.URL, URLBase: "/qui", Username: "s5n",
		PasswordFile: h.write(t, "qui.pw", "qui-pass-123\n")}
	h.config.Autobrr = &AutobrrConfig{URL: auto.URL, URLBase: "/autobrr", Username: "s5n",
		PasswordFile: h.write(t, "autobrr.pw", "autobrr-pass-123\n")}
	// PeerBanHelper holds a copy of the same qBittorrent password, so every test
	// that rotates it has to see this copy move too.
	h.withPbh(t)
	h.runner.Config = h.config
	return f
}

func (h *harness) write(t *testing.T, name, content string) string {
	t.Helper()
	path := h.dir + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ownedOf(t *testing.T, h *harness) *OwnedRecord {
	t.Helper()
	raw, ok := h.cmData["owned.json"]
	if !ok {
		t.Fatal("owned.json was not published")
	}
	rec, err := ParseOwnedRecord([]byte(raw))
	if err != nil {
		t.Fatalf("owned.json did not parse: %v", err)
	}
	return rec
}

// Both apps are registered once, with ids and secret fingerprints recorded, and
// a second run writes nothing at all: no duplicate instance, no duplicate
// client.
func TestWiringRegistersQuiAndAutobrr(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	ctx := context.Background()

	first, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	for _, name := range []string{"qui/instance", "autobrr/downloader/qbittorrent",
		"autobrr/downloader/sonarr", "autobrr/downloader/radarr"} {
		step := stepByName(t, first, name)
		if step.Result != "ok" || step.Detail != "created" {
			t.Fatalf("%s = %+v, want created", name, step)
		}
	}
	if got := len(f.quiRows); got != 1 {
		t.Fatalf("qui holds %d instances after the first run, want 1", got)
	}
	if got := len(f.autoRows); got != 3 {
		t.Fatalf("autobrr holds %d clients after the first run, want 3 (qBittorrent + 2 Arrs)", got)
	}
	for _, row := range f.autoRows {
		if row.Str("type") == "QBITTORRENT" {
			continue
		}
		// The Arr's host carries its base path, which autobrr uses verbatim.
		if !strings.HasSuffix(row.Str("host"), "/"+row.Str("name")) {
			t.Fatalf("%s host = %q, want the Arr's base path", row.Str("name"), row.Str("host"))
		}
	}

	rec := ownedOf(t, h)
	if rec.QuiInstanceID() == 0 || rec.QuiQbitPasswordFingerprint() == "" {
		t.Fatalf("qui not recorded: %+v", rec.Qui)
	}
	for _, name := range []string{"qBittorrent", "sonarr", "radarr"} {
		if rec.AutobrrClientID(name) == 0 || rec.AutobrrClientFingerprint(name) == "" {
			t.Fatalf("autobrr client %s not recorded: %+v", name, rec.Autobrr)
		}
	}
	// No secret ever lands in owned.json: only the sha256 of one.
	if strings.Contains(h.cmData["owned.json"], "hunter2") || strings.Contains(h.cmData["owned.json"], "sonarr-key") {
		t.Fatal("owned.json carries a secret value")
	}

	writes := f.quiWrites + f.autoWrites
	second, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if second.Writes.Created != 0 || second.Writes.Updated != 0 {
		t.Fatalf("second run was not a no-op: %+v\n%s", second.Writes, Summary(second))
	}
	if f.quiWrites+f.autoWrites != writes {
		t.Fatalf("second run wrote to an app: qui %d, autobrr %d", f.quiWrites, f.autoWrites)
	}
	if got := len(f.quiRows); got != 1 {
		t.Fatalf("qui holds %d instances after the second run, want 1", got)
	}
	if got := len(f.autoRows); got != 3 {
		t.Fatalf("autobrr holds %d clients after the second run, want 3", got)
	}
	if again := ownedOf(t, h); again.QuiInstanceID() != rec.QuiInstanceID() ||
		again.AutobrrClientID("sonarr") != rec.AutobrrClientID("sonarr") {
		t.Fatalf("ids changed between runs: %+v -> %+v", rec, again)
	}
}

// Rotating the qBittorrent password in the release Secret is visible only
// through the fingerprints, so exactly the objects that hold a copy are
// rewritten: qui's instance, autobrr's qBittorrent client, PeerBanHelper's
// downloader, and the download client of every Arr. An Arr never returns that password, so without its own
// fingerprint it kept dialling with the old one -- and its connection test only
// runs at the next wiring pass. The Arr key copies are untouched.
func TestWiringRewritesEveryCopyWhenTheQbitPasswordRotates(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	ctx := context.Background()
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatal(err)
	}

	h.write(t, "qbit.pw", "rotated-qbit-pass\n")
	res, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("run after the rotation: %v\n%s", err, Summary(res))
	}
	if res.Writes.Updated != 5 {
		t.Fatalf("updated = %d, want qui/instance, autobrr's qBittorrent client, PeerBanHelper's downloader and both Arrs' download clients\n%s",
			res.Writes.Updated, Summary(res))
	}
	for _, name := range []string{
		"qui/instance", "autobrr/downloader/qbittorrent", "pbh/downloader",
		"sonarr/downloadclient", "radarr/downloadclient",
	} {
		if step := stepByName(t, res, name); step.Detail != "updated" {
			t.Fatalf("%s = %+v, want updated", name, step)
		}
	}
	for _, name := range []string{
		"autobrr/downloader/sonarr", "autobrr/downloader/radarr",
		"prowlarr/application/sonarr", "prowlarr/application/radarr",
	} {
		if step := stepByName(t, res, name); step.Detail != "unchanged" {
			t.Fatalf("%s = %+v, want unchanged: its key did not move", name, step)
		}
	}
	if got := f.quiRows[0].Str("password"); got != "rotated-qbit-pass" {
		t.Fatalf("qui holds %q, want the rotated password", got)
	}
	for _, row := range f.autoRows {
		if row.Str("name") == "qBittorrent" && row.Str("password") != "rotated-qbit-pass" {
			t.Fatalf("autobrr holds %q, want the rotated password", row.Str("password"))
		}
	}
	if got := h.pbh.cfg["qbittorrent"].Str("password"); got != "rotated-qbit-pass" {
		t.Fatalf("PeerBanHelper holds %q, want the rotated password", got)
	}
	for _, instance := range []string{"sonarr", "radarr"} {
		if got, _ := h.stack.clients[instance][0].FieldValue("password"); got != "rotated-qbit-pass" {
			t.Fatalf("%s's download client holds %v, want the rotated password", instance, got)
		}
	}

	// With the new fingerprints on record, the run after the rotation writes
	// nothing at all.
	writes := h.stack.count()
	again, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("run after the rotation: %v", err)
	}
	if again.Writes.Created != 0 || again.Writes.Updated != 0 {
		t.Fatalf("a run after the rotation still wrote: %+v\n%s", again.Writes, Summary(again))
	}
	if got := h.stack.count(); got != writes {
		t.Fatalf("a run after the rotation made %d API writes", got-writes)
	}
}

// A password changed inside the app is not a generic HTTP failure: the step
// says what happened and the command that fixes it, and the Job never restarts
// a pod itself.
func TestWiringNamesAChangedPasswordAndTheFix(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	f.quiLogin = http.StatusUnauthorized

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with qui refusing the account\n%s", Summary(res))
	}
	step := stepByName(t, res, "qui/login")
	if step.Result != "failed" {
		t.Fatalf("qui/login = %+v, want failed", step)
	}
	for _, want := range []string{`rejected the account "s5n"`, "changed inside the app",
		"kubectl rollout restart statefulset/qui"} {
		if !strings.Contains(step.Detail, want) {
			t.Fatalf("qui/login detail = %q, want it to contain %q", step.Detail, want)
		}
	}
	if f.quiWrites != 0 || f.autoWrites != 0 {
		t.Fatalf("a refused login still wrote: qui %d, autobrr %d", f.quiWrites, f.autoWrites)
	}
}

// autobrr answers a bad login with 403 rather than 401, so the classification
// cannot be the status alone: it is the app's own body that says "rejected".
func TestWiringNamesAChangedAutobrrPassword(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	f.autoLogin = http.StatusForbidden

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with autobrr refusing the account\n%s", Summary(res))
	}
	step := stepByName(t, res, "autobrr/login")
	if step.Result != "failed" || !strings.Contains(step.Detail, "kubectl rollout restart statefulset/autobrr") {
		t.Fatalf("autobrr/login = %+v", step)
	}
}

// With qBittorrent absent there is nothing for qui's instance to point at, and
// the step says so rather than failing.
func TestWiringSkipsQuiInstanceWithoutQbittorrent(t *testing.T) {
	h := newHarness(t)
	h.withApps(t)
	h.config.Qbittorrent = nil
	h.runner.Config = h.config
	h.runner.Config.Instances = nil

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "qui/instance")
	if step.Result != "skipped" {
		t.Fatalf("qui/instance = %+v, want skipped", step)
	}
}
