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
	"time"

	"github.com/swizzin/s5n/internal/apps"
)

// fakeStack is a qBittorrent + Sonarr + Radarr + Prowlarr that behave like the
// real APIs where wiring touches them, including masked secrets and the
// connection-test endpoints.
type fakeStack struct {
	mu          sync.Mutex
	cats        map[string]string
	clients     map[string]apps.Item // keyed "sonarr/qBittorrent"
	folders     map[string]bool      // keyed "sonarr:/data/media/tv"
	apps        map[string]apps.Item // prowlarr applications
	hardlink    bool
	writes      int
	tests       int
	nextID      int
	loginErr    bool
	prefs       map[string]any
	ignorePrefs bool   // a qBittorrent that accepts setPreferences and changes nothing
	linkFail    string // a save path whose hardlink proof fails
}

func (f *fakeStack) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeStack) qbitHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			if f.loginErr {
				_, _ = w.Write([]byte("Fails."))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/app/version":
			_, _ = w.Write([]byte("v5.3.0"))
		case "/api/v2/app/preferences":
			writeJSON(w, f.prefs)
		case "/api/v2/app/setPreferences":
			_ = r.ParseForm()
			var body map[string]any
			_ = json.Unmarshal([]byte(r.PostForm.Get("json")), &body)
			f.writes++
			if !f.ignorePrefs {
				for k, v := range body {
					f.prefs[k] = v
				}
			}
		case "/api/v2/torrents/categories":
			out := map[string]any{}
			for name, path := range f.cats {
				out[name] = map[string]any{"name": name, "savePath": path}
			}
			writeJSON(w, out)
		case "/api/v2/torrents/createCategory", "/api/v2/torrents/editCategory":
			_ = r.ParseForm()
			name := r.PostForm.Get("category")
			if r.URL.Path == "/api/v2/torrents/createCategory" {
				if _, ok := f.cats[name]; ok {
					http.Error(w, "exists", http.StatusConflict)
					return
				}
			}
			f.cats[name] = r.PostForm.Get("savePath")
			f.writes++
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *fakeStack) arrHandler(instance, urlBase string) http.HandlerFunc {
	kind := "tv"
	if strings.HasPrefix(instance, "radarr") {
		kind = "movie"
	}
	categoryField := kind + "Category"
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, urlBase)
		switch {
		case p == "/ping":
			_, _ = w.Write([]byte("pong"))
		case p == "/api/v3/downloadclient/schema":
			writeJSON(w, []apps.Item{{
				"implementation": "QBittorrent", "configContract": "QBittorrentSettings",
				"fields": []any{
					map[string]any{"name": "host"},
					map[string]any{"name": "port"},
					map[string]any{"name": "useSsl"},
					map[string]any{"name": "urlBase"},
					map[string]any{"name": "username"},
					map[string]any{"name": "password", "privacy": "password"},
					map[string]any{"name": categoryField},
				},
			}})
		case p == "/api/v3/downloadclient" && r.Method == http.MethodGet:
			out := []apps.Item{}
			if c, ok := f.clients[instance]; ok {
				out = append(out, maskedItem(c, "password"))
			}
			writeJSON(w, out)
		case p == "/api/v3/downloadclient" && r.Method == http.MethodPost:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			body["id"] = f.nextID
			f.clients[instance] = body
			f.writes++
			writeJSON(w, body)
		case strings.HasSuffix(p, "/downloadclient/test"):
			f.tests++
			writeJSON(w, map[string]any{"ok": true})
		case p == "/api/v3/rootfolder" && r.Method == http.MethodGet:
			out := []apps.Item{}
			for key := range f.folders {
				name, path, _ := strings.Cut(key, ":")
				if name == instance {
					out = append(out, apps.Item{"path": path, "accessible": true})
				}
			}
			writeJSON(w, out)
		case p == "/api/v3/rootfolder" && r.Method == http.MethodPost:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.folders[instance+":"+body.Str("path")] = true
			f.writes++
			writeJSON(w, body)
		case p == "/api/v3/config/mediamanagement" && r.Method == http.MethodGet:
			writeJSON(w, apps.Item{"id": 1, "copyUsingHardlinks": f.hardlink})
		case p == "/api/v3/config/mediamanagement" && r.Method == http.MethodPut:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.hardlink = body.Bool("copyUsingHardlinks")
			f.writes++
			writeJSON(w, body)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *fakeStack) prowlarrHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/prowlarr")
		switch {
		case p == "/ping":
			_, _ = w.Write([]byte("pong"))
		case p == "/api/v1/applications/schema":
			writeJSON(w, []apps.Item{
				{"implementation": "Sonarr", "configContract": "SonarrSettings", "fields": []any{
					map[string]any{"name": "prowlarrUrl"}, map[string]any{"name": "baseUrl"},
					map[string]any{"name": "apiKey", "privacy": "apiKey"},
					map[string]any{"name": "authUsername"}, map[string]any{"name": "authPassword"},
				}},
				{"implementation": "Radarr", "configContract": "RadarrSettings", "fields": []any{
					map[string]any{"name": "prowlarrUrl"}, map[string]any{"name": "baseUrl"},
					map[string]any{"name": "apiKey", "privacy": "apiKey"},
					map[string]any{"name": "authUsername"}, map[string]any{"name": "authPassword"},
				}},
			})
		case p == "/api/v1/applications" && r.Method == http.MethodGet:
			out := []apps.Item{}
			for _, a := range f.apps {
				out = append(out, maskedItem(a, "apiKey"))
			}
			writeJSON(w, out)
		case p == "/api/v1/applications" && r.Method == http.MethodPost:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.nextID++
			body["id"] = f.nextID
			f.apps[body.Str("name")] = body
			f.writes++
			writeJSON(w, body)
		case p == "/api/v1/applications/test":
			f.tests++
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// maskedItem mimics the Arr/Prowlarr read path, which never returns a secret.
func maskedItem(i apps.Item, field string) apps.Item {
	c := i.Clone()
	for _, raw := range c["fields"].([]any) {
		f := raw.(map[string]any)
		if f["name"] == field {
			f["value"] = "********"
		}
	}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	stack   *fakeStack
	config  Config
	runner  *Runner
	dir     string
	results []map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := &fakeStack{
		cats: map[string]string{}, clients: map[string]apps.Item{},
		folders: map[string]bool{}, apps: map[string]apps.Item{}, hardlink: true,
		// The image's own runtime default: on the config volume, category paths off.
		prefs: map[string]any{"save_path": "/config/Downloads/", "temp_path": "/config/Downloads/temp/",
			"use_category_paths_in_manual_mode": false},
	}
	qbit := httptest.NewServer(f.qbitHandler())
	sonarr := httptest.NewServer(f.arrHandler("sonarr", "/sonarr"))
	radarr := httptest.NewServer(f.arrHandler("radarr", "/radarr"))
	prowlarr := httptest.NewServer(f.prowlarrHandler())
	t.Cleanup(func() { qbit.Close(); sonarr.Close(); radarr.Close(); prowlarr.Close() })

	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	h := &harness{stack: f, dir: dir}
	h.config = Config{
		Namespace: "seedbox", ConfigMap: "s5n-wiring", TimeoutSeconds: 10,
		Qbittorrent: &QbitConfig{URL: qbit.URL, Username: "admin", PasswordFile: write("qbit.pw", "hunter2\n")},
		Prowlarr:    &ProwlarrConfig{URL: prowlarr.URL, URLBase: "/prowlarr", SelfURL: prowlarr.URL + "/prowlarr", APIKeyFile: write("prowlarr.key", "prowlarr-key")},
		Instances: []Instance{
			{Name: "sonarr", Kind: "sonarr", URL: sonarr.URL, URLBase: "/sonarr", APIKeyFile: write("sonarr.key", "sonarr-key"),
				Category: "tv", SavePath: "/data/downloads/tv", RootFolder: "/data/media/tv"},
			{Name: "radarr", Kind: "radarr", URL: radarr.URL, URLBase: "/radarr", APIKeyFile: write("radarr.key", "radarr-key"),
				Category: "movies", SavePath: "/data/downloads/movies", RootFolder: "/data/media/movies"},
		},
	}
	h.runner = &Runner{
		Config:        h.config,
		ContractRetry: 2 * time.Second,
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		HardlinkCheck: func(savePath, rootFolder string) *HardlinkCheck {
			if savePath == f.linkFail {
				return &HardlinkCheck{Nlink: 1, Source: savePath, Target: rootFolder, Error: "link: invalid cross-device link"}
			}
			return &HardlinkCheck{OK: true, Nlink: 2, SameDevice: true, Source: savePath, Target: rootFolder}
		},
		Publish: func(_ context.Context, data map[string]string) error {
			h.results = append(h.results, data)
			return nil
		},
	}
	return h
}

func TestWiringIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	if first.Status != "ok" {
		t.Fatalf("first run status = %s", first.Status)
	}
	wantCreated := 8 // 2 categories, 2 download clients, 2 root folders, 2 Prowlarr applications
	// The data contract starts on the image's default and is corrected once; the
	// Arrs' hardlink setting starts true in the fake.
	if first.Writes.Created != wantCreated || first.Writes.Updated != 1 || first.Writes.Unchanged != 2 {
		t.Fatalf("first run writes = %+v, want created=%d updated=1 (data contract) unchanged=2 (hardlinks)\n%s",
			first.Writes, wantCreated, Summary(first))
	}
	if got := h.stack.prefs["save_path"]; got != "/data/downloads" {
		t.Fatalf("save_path after wiring = %v", got)
	}
	if len(first.Hardlinks) != 2 || !first.Hardlinks[0].OK || !first.Hardlinks[1].OK {
		t.Fatalf("want one passing hardlink proof per download/library pair, got %+v", first.Hardlinks)
	}

	writesAfterFirst := h.stack.count()
	second, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if second.Writes.Created != 0 || second.Writes.Updated != 0 {
		t.Fatalf("second run was not a no-op: %+v\n%s", second.Writes, Summary(second))
	}
	if got := h.stack.count(); got != writesAfterFirst {
		t.Fatalf("second run made %d API writes", got-writesAfterFirst)
	}
	if second.Writes.Unchanged != 11 {
		t.Fatalf("second run unchanged = %d, want 11\n%s", second.Writes.Unchanged, Summary(second))
	}
	// The connection tests still run every time: that is the proof, not a write.
	// One download-client test per Arr plus one application test per Arr, per run.
	if h.stack.tests != 8 {
		t.Fatalf("connection tests = %d, want 8 (4 per run)", h.stack.tests)
	}

	// The receipt is published and parseable.
	if len(h.results) != 2 {
		t.Fatalf("published %d receipts, want 2", len(h.results))
	}
	var got Result
	if err := json.Unmarshal([]byte(h.results[1]["status.json"]), &got); err != nil {
		t.Fatalf("status.json is not valid JSON: %v", err)
	}
	if got.Status != "ok" || len(got.Steps) == 0 {
		t.Fatalf("receipt = %+v", got)
	}
	if !strings.Contains(h.results[1]["summary.txt"], "s5n wiring: ok") {
		t.Fatalf("summary is not human-readable:\n%s", h.results[1]["summary.txt"])
	}
}

func TestWiringFailsLoudlyAndStillPublishes(t *testing.T) {
	h := newHarness(t)
	h.stack.loginErr = true
	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatal("run succeeded with a rejected qBittorrent login")
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "qbittorrent/ready") {
		t.Fatalf("receipt = %+v", res)
	}
	if len(h.results) != 1 {
		t.Fatalf("failure receipt not published")
	}
}

// A qBittorrent that answers 200 to setPreferences but keeps its own default must
// fail the run, not report "updated".
func TestWiringFailsWhenQbitIgnoresPreferences(t *testing.T) {
	h := newHarness(t)
	h.stack.ignorePrefs = true
	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded although qBittorrent kept its default save path\n%s", Summary(res))
	}
	if !strings.Contains(res.Error, "qbittorrent/data-contract") || !strings.Contains(res.Error, "save_path") {
		t.Fatalf("error does not name the drift: %s", res.Error)
	}
}

// A save path under /data but not the contract's root is drift, not "fine".
func TestWiringCorrectsNearMissSavePath(t *testing.T) {
	h := newHarness(t)
	h.stack.prefs = map[string]any{"save_path": "/data/media/", "temp_path": "/data/downloads/temp",
		"use_category_paths_in_manual_mode": true}
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if got := h.stack.prefs["save_path"]; got != "/data/downloads" {
		t.Fatalf("save_path = %v, want /data/downloads", got)
	}
}

// Every download/library pair is proven, so a separate filesystem mounted under one
// library folder is caught even when the first pair links fine.
func TestWiringProvesEveryPair(t *testing.T) {
	h := newHarness(t)
	h.stack.linkFail = "/data/downloads/movies"
	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with a cross-device movies pair\n%s", Summary(res))
	}
	if !strings.Contains(res.Error, "data/hardlink/movies") {
		t.Fatalf("error does not name the failing pair: %s", res.Error)
	}
	if len(res.Hardlinks) != 2 || !res.Hardlinks[0].OK || res.Hardlinks[1].OK {
		t.Fatalf("receipt hardlinks = %+v, want tv ok then movies failed", res.Hardlinks)
	}
}

func TestConfigValidation(t *testing.T) {
	base := Config{
		Qbittorrent: &QbitConfig{URL: "http://qbittorrent:8080", Username: "admin", PasswordFile: "/k"},
		Instances: []Instance{{Name: "sonarr", Kind: "sonarr", URL: "http://sonarr:8989", APIKeyFile: "/k2",
			Category: "tv", SavePath: "/data/downloads/tv", RootFolder: "/data/media/tv"}},
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := base
	bad.Instances = append(bad.Instances, Instance{Name: "sonarr-4k", Kind: "sonarr", URL: "http://sonarr-4k:8989",
		APIKeyFile: "/k3", Category: "tv", SavePath: "/data/downloads/tv-4k", RootFolder: "/data/media/tv-4k"})
	if err := bad.validate(); err == nil {
		t.Fatal("two save paths for one category accepted")
	}
}
