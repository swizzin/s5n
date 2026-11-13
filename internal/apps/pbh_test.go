package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePbh answers like PeerBanHelper v9.5.4's WebAPI: every answer is wrapped in
// {"success","message","data"}, downloaders are keyed by a string id, PUT
// creates and PATCH replaces, and the status endpoint reports the last poll
// result together with the config PBH stored - the password included, which is
// what PBH really does.
//
// Two of the real API's failures are reproduced because the shapes matter more
// than the status codes: a create whose config has no basicAuth throws inside
// PBH's own save (500, and the downloader stays registered in memory), and a
// second PUT of the same id is refused (400).
type fakePbh struct {
	mu        sync.Mutex
	token     string
	order     []string // ids, in insertion order
	cfg       map[string]Item
	status    map[string]string
	statusMsg map[string]string
	listCalls int
	puts      int
	patches   int
	requests  int
	// healthyAfter makes the status flip to HEALTHY after that many status
	// reads, which is how PBH's poll loop behaves: UNKNOWN right after a write,
	// HEALTHY a few seconds later.
	healthyAfter int
	statusReads  int
	saveFails    bool // a save that answers 500 like PBH's own NPE
}

func newFakePbh(t *testing.T) (*fakePbh, *httptest.Server) {
	t.Helper()
	f := &fakePbh{token: "pbh-token", cfg: map[string]Item{}, status: map[string]string{}, statusMsg: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePbh) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePbh) ok(w http.ResponseWriter, status int, data any) {
	f.write(w, status, map[string]any{"success": true, "message": nil, "data": data})
}

func (f *fakePbh) fail(w http.ResponseWriter, status int, msg string) {
	f.write(w, status, map[string]any{"success": false, "message": msg, "data": nil})
}

func (f *fakePbh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++

	// The real gate: a token in an Authorization: Bearer header, or nothing.
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		f.fail(w, http.StatusForbidden, "Not logged in")
		return
	}

	path := r.URL.Path
	switch {
	case path == "/api/metadata/manifest":
		f.ok(w, http.StatusOK, map[string]any{"version": map[string]any{"version": "9.5.4"}})

	case path == "/api/downloaders" && r.Method == http.MethodGet:
		f.listCalls++
		out := []map[string]any{}
		for _, id := range f.order {
			c := f.cfg[id]
			// The list reports the API endpoint, not the configured one.
			out = append(out, map[string]any{
				"id": id, "name": c.Str("name"), "endpoint": c.Str("endpoint") + "/api/v2",
				"type": c.Str("type"), "paused": c.Bool("paused"),
			})
		}
		f.ok(w, http.StatusOK, out)

	case path == "/api/downloaders" && r.Method == http.MethodPut:
		f.puts++
		var body struct {
			ID     string `json:"id"`
			Config Item   `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body.Config["basicAuth"]; !ok || f.saveFails {
			// saveToYaml dereferences basicAuth with no nil check
			// (QBittorrentConfigImpl.saveToYaml:60); the exception handler turns
			// it into a 500 after the downloader is already registered.
			f.order = append(f.order, body.ID)
			f.cfg[body.ID] = body.Config
			f.fail(w, http.StatusInternalServerError, "An internal server error occurred while processing the WebAPI request, please check the console logs")
			return
		}
		if _, exists := f.cfg[body.ID]; exists {
			f.fail(w, http.StatusBadRequest, "Downloader creation failed: a downloader with this name or id already exists")
			return
		}
		for _, id := range f.order {
			if strings.EqualFold(f.cfg[id].Str("name"), body.Config.Str("name")) {
				f.fail(w, http.StatusBadRequest, "Downloader creation failed: a downloader with this name or id already exists")
				return
			}
		}
		f.order = append(f.order, body.ID)
		f.cfg[body.ID] = body.Config
		f.status[body.ID] = "UNKNOWN"
		f.statusMsg[body.ID] = "PeerBanHelper not connected to this downloader yet"
		f.ok(w, http.StatusCreated, nil)

	case strings.HasPrefix(path, "/api/downloaders/") && strings.HasSuffix(path, "/status") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/downloaders/"), "/status")
		c, ok := f.cfg[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "Downloader not exists")
			return
		}
		f.statusReads++
		if f.healthyAfter > 0 && f.statusReads >= f.healthyAfter {
			f.status[id] = "HEALTHY"
			f.statusMsg[id] = "Currently working normally"
		}
		st := f.status[id]
		if st == "" {
			st = "UNKNOWN"
		}
		f.ok(w, http.StatusOK, map[string]any{
			"lastStatus": st, "lastStatusMessage": f.statusMsg[id],
			"activeTorrents": 0, "activePeers": 0, "config": c, "paused": c.Bool("paused"),
		})

	case strings.HasPrefix(path, "/api/downloaders/") && r.Method == http.MethodPatch:
		f.patches++
		id := strings.TrimPrefix(path, "/api/downloaders/")
		if _, ok := f.cfg[id]; !ok {
			f.fail(w, http.StatusNotFound, "Downloader not exists")
			return
		}
		var body struct {
			Config Item `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.cfg[id] = body.Config
		f.status[id] = "UNKNOWN"
		f.statusMsg[id] = "PeerBanHelper not connected to this downloader yet"
		f.ok(w, http.StatusOK, nil)

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// pbhSpec is the downloader the wiring Job describes: qBittorrent's Service, no
// trailing slash and no /api/v2 (PBH appends that itself).
func pbhSpec(password string) PbhDownloaderSpec {
	return PbhDownloaderSpec{
		ID: "qbittorrent", Name: "qBittorrent",
		Endpoint: "http://qbittorrent:8080", Username: "admin", Password: password,
	}
}

// A first run creates the downloader with the whole config PBH's own class
// needs, and returns the id it lives under: PUT answers 201 with a null body, so
// the id can only come from what was sent.
func TestPbhCreatesDownloader(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")

	res, err := p.EnsureDownloader(context.Background(), pbhSpec("hunter2"), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.StringID != "qbittorrent" {
		t.Fatalf("result = %+v, want created under id qbittorrent", res)
	}
	stored := f.cfg["qbittorrent"]
	if stored == nil {
		t.Fatal("nothing was stored")
	}
	if stored.Str("endpoint") != "http://qbittorrent:8080" || stored.Str("username") != "admin" ||
		stored.Str("password") != "hunter2" || stored.Str("type") != "qbittorrent" {
		t.Fatalf("stored config = %+v", stored)
	}
	if _, ok := stored["basicAuth"]; !ok {
		t.Fatal("the create carried no basicAuth: PBH's save throws on that")
	}
}

// A second run writes nothing at all: no second PUT (which PBH would refuse),
// no PATCH.
func TestPbhEnsureDownloaderIsIdempotent(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	spec := pbhSpec("hunter2")
	if _, err := p.EnsureDownloader(ctx, spec, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := p.EnsureDownloader(ctx, spec, "qbittorrent")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if res.Change != Unchanged {
		t.Fatalf("second ensure = %+v, want unchanged", res)
	}
	if f.puts != 1 || f.patches != 0 {
		t.Fatalf("second ensure made %d PUTs and %d PATCHes, want 1 and 0", f.puts, f.patches)
	}
}

// An update is the stored config with the owned fields overlaid: a name a person
// changed, and settings s5n has no opinion about, survive it.
func TestPbhUpdateKeepsFieldsItDoesNotOwn(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	spec := pbhSpec("hunter2")
	if _, err := p.EnsureDownloader(ctx, spec, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	// What a person does in PBH's UI: rename it, pause it, shadow-ban instead,
	// and point the login at a different user.
	edited := f.cfg["qbittorrent"].Clone()
	edited["name"] = "qBit (mine)"
	edited["paused"] = true
	edited["useShadowBan"] = true
	edited["username"] = "someone-else"
	f.cfg["qbittorrent"] = edited

	if _, err := p.EnsureDownloader(ctx, spec, "qbittorrent"); err != nil {
		t.Fatalf("ensure after the edit: %v", err)
	}
	stored := f.cfg["qbittorrent"]
	if stored.Str("name") != "qBit (mine)" || !stored.Bool("paused") || !stored.Bool("useShadowBan") {
		t.Fatalf("the update overwrote fields s5n does not own: %+v", stored)
	}
	if stored.Str("username") != "admin" || stored.Str("password") != "hunter2" {
		t.Fatalf("the update did not restore the owned login: %+v", stored)
	}
}

// The endpoint PBH reports it dials (with /api/v2) is not the one stored, so a
// comparison against the list would read as drift on every run. The status
// endpoint's config is what the update compares, and an untouched downloader
// there is unchanged.
func TestPbhDoesNotDriftOnTheApiEndpoint(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	spec := pbhSpec("hunter2")
	if _, err := p.EnsureDownloader(ctx, spec, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := f.cfg["qbittorrent"].Str("endpoint"); got != "http://qbittorrent:8080" {
		t.Fatalf("stored endpoint = %q", got)
	}
	res, err := p.EnsureDownloader(ctx, spec, "qbittorrent")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Change != Unchanged || len(res.Drift) != 0 {
		t.Fatalf("result = %+v (drift %+v), want unchanged", res, res.Drift)
	}
}

// Drift on an owned, visible field is restored and reported - the user changed
// the endpoint in PBH's UI, and the next run puts the data contract back.
func TestPbhRestoresDriftedEndpoint(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	spec := pbhSpec("hunter2")
	if _, err := p.EnsureDownloader(ctx, spec, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	edited := f.cfg["qbittorrent"].Clone()
	edited["endpoint"] = "http://example.com:9000"
	f.cfg["qbittorrent"] = edited

	res, err := p.EnsureDownloader(ctx, spec, "qbittorrent")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Change != Updated || len(res.Drift) != 1 || res.Drift[0].Field != "endpoint" {
		t.Fatalf("result = %+v, want one drifted endpoint restored", res)
	}
	if got := f.cfg["qbittorrent"].Str("endpoint"); got != "http://qbittorrent:8080" {
		t.Fatalf("endpoint = %q, want the data contract's", got)
	}
}

// A downloader the user added by hand is adopted by name on the first run: PBH
// refuses a duplicate name, so a blind create would fail instead of taking over.
func TestPbhAdoptsDownloaderByName(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	adopted := pbhCreateConfig(pbhSpec("old-pass"))
	adopted["endpoint"] = "http://elsewhere:8080"
	f.cfg["mine"] = adopted
	f.order = append(f.order, "mine")
	f.status["mine"] = "HEALTHY"

	res, err := p.EnsureDownloader(ctx, pbhSpec("hunter2"), "")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Change != Updated || res.StringID != "mine" {
		t.Fatalf("result = %+v, want an update of the adopted downloader", res)
	}
	if f.puts != 0 || f.patches != 1 {
		t.Fatalf("adoption made %d PUTs and %d PATCHes, want 0 and 1", f.puts, f.patches)
	}
	if got := f.cfg["mine"].Str("password"); got != "hunter2" {
		t.Fatalf("adopted downloader holds %q", got)
	}
}

// A recorded id that is gone means the user deleted it: the run recreates it and
// says which id disappeared, so the receipt is not silent about it.
func TestPbhRecreatesADeletedDownloader(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")

	res, err := p.EnsureDownloader(context.Background(), pbhSpec("hunter2"), "was-here")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Change != Created || !res.Recreated || res.PreviousStringID != "was-here" {
		t.Fatalf("result = %+v, want a recreate naming was-here", res)
	}
	if f.cfg["qbittorrent"] == nil {
		t.Fatal("nothing was created")
	}
}

// PBH's own save can fail after the downloader is registered: the answer is a
// 500 whose body carries the app's sentence, and the client must not read it as
// a success.
func TestPbhReportsA500WithTheAppsMessage(t *testing.T) {
	f, srv := newFakePbh(t)
	f.saveFails = true
	p := NewPbh(srv.URL, "pbh-token")

	_, err := p.EnsureDownloader(context.Background(), pbhSpec("hunter2"), "")
	if err == nil {
		t.Fatal("ensure succeeded although PBH answered 500")
	}
	for _, want := range []string{"create pbh downloader", "Internal Server Error", "internal server error occurred"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

// A WebUI token that is not PBH's is refused with 403, and waiting cannot fix
// it: PBH blacklists an address for 15 minutes after ten failures on one
// endpoint, so the wait has to give up after the first one.
func TestPbhWrongTokenIsRefusedWithoutRetrying(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "not-the-token")

	err := p.WaitReady(context.Background(), 30*time.Second)
	if err == nil {
		t.Fatal("WaitReady succeeded with a token PBH does not accept")
	}
	if !IsForbidden(err) {
		t.Fatalf("error = %v, want a 403", err)
	}
	if f.requests != 1 {
		t.Fatalf("PBH saw %d requests, want 1: a refused token is not retried", f.requests)
	}
}

// HEALTHY is the proof that PBH logged in to qBittorrent with what was written;
// until its own poll loop has run the status is UNKNOWN, so the wait has to poll
// rather than read once.
func TestPbhWaitHealthy(t *testing.T) {
	f, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	ctx := context.Background()

	if _, err := p.EnsureDownloader(ctx, pbhSpec("hunter2"), ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.healthyAfter = 2 // UNKNOWN on the first read, HEALTHY on the second
	status, err := p.WaitHealthy(ctx, "qbittorrent", 10*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if status != "HEALTHY" {
		t.Fatalf("status = %q", status)
	}

	// A downloader PBH reports as ERROR never becomes healthy, and the error
	// carries the app's own sentence.
	f.status["qbittorrent"] = "ERROR"
	f.statusMsg["qbittorrent"] = "Unable to connect to qBittorrent: login refused"
	f.healthyAfter = 0
	f.statusReads = 0
	if _, err := p.WaitHealthy(ctx, "qbittorrent", time.Second); err == nil {
		t.Fatal("wait succeeded on an ERROR downloader")
	} else if !strings.Contains(err.Error(), "login refused") {
		t.Fatalf("error = %v, want PBH's own message", err)
	}
}

// The version in the receipt comes from the manifest the WebUI reads first, and
// it answers without a token - so a wrong-token pod is still describable.
func TestPbhVersion(t *testing.T) {
	_, srv := newFakePbh(t)
	p := NewPbh(srv.URL, "pbh-token")
	v, err := p.Version(context.Background())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != "9.5.4" {
		t.Fatalf("version = %q", v)
	}
}
