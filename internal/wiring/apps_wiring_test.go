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
	autoLogin  int // status the login answers with; 0 is 204
	// autoLoginBody is the message autobrr's refusal carries, which is what
	// separates a bad password from a login the app's auth mode does not offer.
	autoLoginBody string
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
				body := "could not login: bad credentials"
				if f.autoLoginBody != "" {
					body = f.autoLoginBody
				}
				w.WriteHeader(f.autoLogin)
				writeJSON(w, map[string]string{"message": body})
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
	// PeerBanHelper's downloader is the release's qBittorrent too, and it holds
	// no more of a credential than the others.
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
	if rec.QuiInstanceID() == 0 {
		t.Fatalf("qui not recorded: %+v", rec.Qui)
	}
	if rec.AutobrrClientID("qBittorrent") == 0 || rec.AutobrrClientFingerprint("qBittorrent") != "" {
		t.Fatalf("autobrr's qBittorrent client = id %d, fingerprint %q: the id is owned, no secret is",
			rec.AutobrrClientID("qBittorrent"), rec.AutobrrClientFingerprint("qBittorrent"))
	}
	for _, name := range []string{"sonarr", "radarr"} {
		if rec.AutobrrClientID(name) == 0 || rec.AutobrrClientFingerprint(name) == "" {
			t.Fatalf("autobrr client %s not recorded: %+v", name, rec.Autobrr)
		}
	}
	// No secret ever lands in owned.json: only the sha256 of the Arr keys.
	if strings.Contains(h.cmData["owned.json"], "sonarr-key") {
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

// qBittorrent skips its login for the release's addresses, so the Job hands
// every caller an empty username and password. That is the rule this test
// pins: the five objects that used to hold a copy of the password (the Arrs'
// download clients, qui's instance, autobrr's qBittorrent client and
// PeerBanHelper's downloader) hold nothing, and the release Secret's
// password is read by qBittorrent's own init container alone.
func TestWiringGivesQbittorrentNoCredentials(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}

	for _, instance := range []string{"sonarr", "radarr"} {
		client := h.stack.clients[instance][0]
		for _, field := range []string{"username", "password"} {
			if v, _ := client.FieldValue(field); v != "" {
				t.Fatalf("%s's download client holds %s = %v, want it empty", instance, field, v)
			}
		}
	}
	if got := f.quiRows[0].Str("username"); got != "" {
		t.Fatalf("qui's instance holds username %q, want it empty", got)
	}
	if got := f.quiRows[0].Str("password"); got != "" {
		t.Fatalf("qui's instance holds a password, want it empty")
	}
	for _, row := range f.autoRows {
		if row.Str("name") != "qBittorrent" {
			continue
		}
		if row.Str("username") != "" || row.Str("password") != "" {
			t.Fatalf("autobrr's qBittorrent client holds %q/%q, want both empty",
				row.Str("username"), row.Str("password"))
		}
	}
	if got := h.pbh.cfg["qbittorrent"]; got.Str("username") != "" || got.Str("password") != "" {
		t.Fatalf("PeerBanHelper's downloader holds %q/%q, want both empty",
			got.Str("username"), got.Str("password"))
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

// autobrr v1.91 refuses the password login itself when OIDC is on with the
// built-in login switched off ("password login is unavailable in oidc
// authentication mode", internal/http/auth.go:122-126 at v1.91.1), and the
// wiring Job's account then never signs in, so no downloader is registered. The
// receipt has to say that: a restart would not put a password nobody changed
// back, and an operator told to run one would look for the wrong fault.
func TestWiringNamesAnAppThatRefusesItsOwnPasswordLogin(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	f.autoLogin = http.StatusForbidden
	f.autoLoginBody = "password login is unavailable in oidc authentication mode"

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with autobrr refusing its own login\n%s", Summary(res))
	}
	step := stepByName(t, res, "autobrr/login")
	if step.Result != "failed" {
		t.Fatalf("autobrr/login = %+v, want failed", step)
	}
	if strings.Contains(step.Detail, "changed inside the app") {
		t.Fatalf("autobrr/login detail = %q, want the auth mode named rather than a changed password", step.Detail)
	}
	for _, want := range []string{"autobrr", "own password login", "oidc"} {
		if !strings.Contains(step.Detail, want) {
			t.Fatalf("autobrr/login detail = %q, want it to contain %q", step.Detail, want)
		}
	}
	if f.autoWrites != 0 {
		t.Fatalf("a refused login still wrote %d clients", f.autoWrites)
	}
	// And the step it would have reached never ran, so the receipt is honest
	// about what did not happen.
	for _, s := range res.Steps {
		if s.Name == "autobrr/downloader/qbittorrent" {
			t.Fatalf("a downloader step ran after a refused login: %+v", s)
		}
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

// The box a migration can leave behind: autobrr holds the source's own client
// for each Arr -- the 4K ones named "Sonarr4k"/"Radarr4k" -- and the Job had
// already made its own beside them, each by name. The operator deletes the
// Job's rows, and owned.json still records the ids they had.
//
// The next run adopts the migrated row by the Arr it dials, which is the client
// the operator's filters refer to by id, instead of recreating the Job's. Run
// one proves the Job never removes the operator's row: that is theirs, and the
// duplicate is theirs to delete.
func TestWiringAdoptsTheMigratedAutobrrClientAfterItsRecordedOneIsDeleted(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	inst := h.config.Instances[0]
	host := strings.TrimRight(inst.URL, "/") + inst.URLBase
	migrated := apps.Item{
		"id": 12, "name": "Sonarr", "type": "SONARR", "enabled": true,
		"host": host, "port": 0, "tls": false, "username": "", "password": "",
		"settings": map[string]any{"apikey": "sonarr-key"},
	}
	job := apps.Item{
		"id": 41, "name": "sonarr", "type": "SONARR", "enabled": true,
		"host": host, "port": 0, "tls": false, "username": "", "password": "",
		"settings": map[string]any{"apikey": "sonarr-key"},
	}
	f.autoRows = []apps.Item{migrated.Clone(), job.Clone()}
	rec := NewOwnedRecord()
	rec.SetAutobrrClientID("sonarr", 41)
	rec.SetAutobrrClientFingerprint("sonarr", apps.SecretFingerprint("sonarr-key"))
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	h.cmData = map[string]string{"owned.json": string(raw)}

	first, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	// The recorded id is authoritative: the Job keeps its own row and never
	// touches the operator's, so the receipt must not claim otherwise.
	if got := ownedOf(t, h).AutobrrClientID("sonarr"); got != 41 {
		t.Fatalf("owned.json records sonarr = %d, want the recorded 41", got)
	}
	for _, row := range f.autoRows {
		if row.Int("id") == 12 && (row.Str("name") != "Sonarr" || !row.Bool("enabled")) {
			t.Fatalf("the operator's own client was rewritten: %v", row)
		}
	}

	// The operator deletes the Job's row, the way they would in autobrr.
	rows := f.autoRows[:0]
	for _, row := range f.autoRows {
		if row.Int("id") != 41 {
			rows = append(rows, row)
		}
	}
	f.autoRows = rows
	writes := f.autoWrites

	second, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	step := stepByName(t, second, "autobrr/downloader/sonarr")
	if step.Result != "ok" || !strings.Contains(step.Detail, "adopted") {
		t.Fatalf("autobrr/downloader/sonarr = %+v, want the migrated client adopted", step)
	}
	if !strings.Contains(step.Detail, "41") {
		t.Fatalf("detail = %q, want the recorded id it replaced", step.Detail)
	}
	if f.autoWrites != writes {
		t.Fatalf("adoption wrote %d clients: the migrated row already matched", f.autoWrites-writes)
	}
	var sonarrRows []apps.Item
	for _, row := range f.autoRows {
		if row.Str("type") == "SONARR" {
			sonarrRows = append(sonarrRows, row)
		}
	}
	if len(sonarrRows) != 1 || sonarrRows[0].Int("id") != 12 {
		t.Fatalf("autobrr holds %d SONARR clients, want the migrated id 12 alone", len(sonarrRows))
	}
	if got := ownedOf(t, h).AutobrrClientID("sonarr"); got != 12 {
		t.Fatalf("owned.json records sonarr = %d, want the adopted 12", got)
	}
	if !strings.Contains(Summary(second), "adopted") {
		t.Fatalf("the receipt a person reads says nothing about the adoption:\n%s", Summary(second))
	}
}
