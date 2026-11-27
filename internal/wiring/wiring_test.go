package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/apps"
)

// fakeStack is qBittorrent + the Arrs + Prowlarr behaving like the real APIs where
// wiring touches them, masked secrets and connection tests included.
type fakeStack struct {
	mu          sync.Mutex
	cats        map[string]string
	clients     map[string][]apps.Item // keyed by Arr instance name; each client carries an id
	folders     map[string]bool        // keyed "sonarr:/data/media/tv"
	apps        []apps.Item            // prowlarr applications, flat, each carries an id
	hardlink    bool
	writes      int
	tests       int
	nextID      int
	logins      int // requests to /api/v2/auth/login: there should be none
	prefs       map[string]any
	ignorePrefs bool   // a qBittorrent that accepts setPreferences and changes nothing
	linkFail    string // a save path whose hardlink proof fails
	// bazarr is each instance's settings document, in the app's own shape.
	bazarr map[string]map[string]any
	// sabMisc and sabCats start on the image's defaults, the trap the Job corrects.
	sabMisc map[string]string
	sabCats map[string]string
}

// bazarrHandler serves one instance: ping, the settings document behind an API key,
// and the partial form POST (bazarr/api/system/settings.py at v1.6.2), cast as
// save_settings casts it.
func (f *fakeStack) bazarrHandler(instance, urlBase, key string) http.HandlerFunc {
	f.mu.Lock()
	f.bazarr[instance] = map[string]any{
		"general": map[string]any{"base_url": urlBase, "use_sonarr": false, "use_radarr": false},
		"sonarr":  map[string]any{"apikey": "", "base_url": "/", "ip": "127.0.0.1", "port": 8989, "ssl": false},
		"radarr":  map[string]any{"apikey": "", "base_url": "/", "ip": "127.0.0.1", "port": 7878, "ssl": false},
	}
	f.mu.Unlock()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, urlBase)
		switch {
		case p == "/api/system/ping":
			writeJSON(w, map[string]any{"status": "OK"})
		case p == "/api/system/settings" && r.Method == http.MethodGet:
			if r.Header.Get("X-API-KEY") != key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			writeJSON(w, f.bazarr[instance])
		case p == "/api/system/settings" && r.Method == http.MethodPost:
			if r.Header.Get("X-API-KEY") != key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			doc := f.bazarr[instance]
			for k := range r.Form {
				section, field, ok := strings.Cut(strings.TrimPrefix(k, "settings-"), "-")
				if !ok {
					continue
				}
				sec, _ := doc[section].(map[string]any)
				v := r.Form.Get(k)
				if n, err := strconv.Atoi(v); err == nil {
					sec[field] = n
				} else if v == "true" || v == "false" {
					sec[field] = v == "true"
				} else {
					sec[field] = v
				}
			}
			f.writes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// bazarrValue reads one setting out of the fake's document.
func (f *fakeStack) bazarrValue(instance, section, field string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	sec, _ := f.bazarr[instance][section].(map[string]any)
	return sec[field]
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
			// Recorded, not answered: a client with no password must never
			// send this at all.
			f.logins++
			_, _ = w.Write([]byte("Fails."))
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

// arrAPI is the API version one instance serves: Lidarr /api/v1, the others /api/v3
// (Lidarr.Http/VersionedApiControllerAttribute.cs at v3.1.2.4902).
func arrAPI(instance string) string {
	if strings.HasPrefix(instance, "lidarr") {
		return "/api/v1"
	}
	return "/api/v3"
}

// arrCategoryField is each Arr's download-category field
// (Download/Clients/QBittorrent/QBittorrentSettings.cs).
func arrCategoryField(instance string) string {
	switch {
	case strings.HasPrefix(instance, "radarr"):
		return "movieCategory"
	case strings.HasPrefix(instance, "lidarr"):
		return "musicCategory"
	}
	return "tvCategory"
}

func (f *fakeStack) arrHandler(instance, urlBase string) http.HandlerFunc {
	api := arrAPI(instance)
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.serveArr(w, r, instance, strings.TrimPrefix(r.URL.Path, urlBase), api)
	}
}

// serveArr is one Arr's routes. The caller holds the fake's lock.
func (f *fakeStack) serveArr(w http.ResponseWriter, r *http.Request, instance, p, api string) {
	switch {
	case p == "/ping":
		_, _ = w.Write([]byte("pong"))
	case p == api+"/qualityprofile" && r.Method == http.MethodGet:
		writeJSON(w, []apps.Item{{"id": 1, "name": "Any"}})
	case p == api+"/metadataprofile" && r.Method == http.MethodGet:
		writeJSON(w, []apps.Item{{"id": 2, "name": "Standard"}})
	case strings.HasPrefix(p, api+"/downloadclient"):
		f.serveArrClients(w, r, instance, p, api)
	case p == api+"/rootfolder" && r.Method == http.MethodGet:
		out := []apps.Item{}
		for key := range f.folders {
			name, path, _ := strings.Cut(key, ":")
			if name == instance {
				out = append(out, apps.Item{"path": path, "accessible": true})
			}
		}
		writeJSON(w, out)
	case p == api+"/rootfolder" && r.Method == http.MethodPost:
		f.createRootFolder(w, r, instance, api)
	case p == api+"/config/mediamanagement" && r.Method == http.MethodGet:
		writeJSON(w, apps.Item{"id": 1, "copyUsingHardlinks": f.hardlink})
	case p == api+"/config/mediamanagement" && r.Method == http.MethodPut:
		var body apps.Item
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.hardlink = body.Bool("copyUsingHardlinks")
		f.writes++
		writeJSON(w, body)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// serveArrClients is one Arr's download-client resource: the schema, the list,
// a create, the connection test and an update. The caller holds the lock.
func (f *fakeStack) serveArrClients(w http.ResponseWriter, r *http.Request, instance, p, api string) {
	switch {
	case p == api+"/downloadclient/schema":
		writeJSON(w, []apps.Item{
			{
				"implementation": "QBittorrent", "configContract": "QBittorrentSettings",
				"fields": []any{
					map[string]any{"name": "host"},
					map[string]any{"name": "port"},
					map[string]any{"name": "useSsl"},
					map[string]any{"name": "urlBase"},
					map[string]any{"name": "username"},
					map[string]any{"name": "password", "privacy": "password"},
					map[string]any{"name": arrCategoryField(instance)},
				},
			},
			{
				"implementation": "Sabnzbd", "configContract": "SabnzbdSettings",
				"fields": []any{
					map[string]any{"name": "host"},
					map[string]any{"name": "port"},
					map[string]any{"name": "useSsl"},
					map[string]any{"name": "urlBase"},
					map[string]any{"name": "username"},
					map[string]any{"name": "password", "privacy": "password"},
					map[string]any{"name": "apiKey", "privacy": "password"},
					map[string]any{"name": arrCategoryField(instance)},
				},
			},
		})
	case p == api+"/downloadclient" && r.Method == http.MethodGet:
		out := []apps.Item{}
		for _, c := range f.clients[instance] {
			out = append(out, maskedItem(c, "password"))
		}
		writeJSON(w, out)
	case p == api+"/downloadclient" && r.Method == http.MethodPost:
		var body apps.Item
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		body["id"] = f.nextID
		f.clients[instance] = append(f.clients[instance], body)
		f.writes++
		writeJSON(w, body)
	case strings.HasSuffix(p, "/downloadclient/test"):
		f.tests++
		writeJSON(w, map[string]any{"ok": true})
	case strings.HasPrefix(p, api+"/downloadclient/") && r.Method == http.MethodPut:
		var body apps.Item
		_ = json.NewDecoder(r.Body).Decode(&body)
		for i, c := range f.clients[instance] {
			if c.Int("id") == body.Int("id") {
				f.clients[instance][i] = body
				f.writes++
				writeJSON(w, body)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// createRootFolder records one root folder, refusing a Lidarr body without a name
// and both profile ids.
func (f *fakeStack) createRootFolder(w http.ResponseWriter, r *http.Request, instance, api string) {
	var body apps.Item
	_ = json.NewDecoder(r.Body).Decode(&body)
	if api == "/api/v1" && (body.Str("name") == "" || body.Int("defaultQualityProfileId") <= 0 || body.Int("defaultMetadataProfileId") <= 0) {
		http.Error(w, "Name must not be empty, and both profiles must be chosen", http.StatusBadRequest)
		return
	}
	f.folders[instance+":"+body.Str("path")] = true
	f.writes++
	writeJSON(w, body)
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
				{"implementation": "Lidarr", "configContract": "LidarrSettings", "fields": []any{
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
			f.apps = append(f.apps, body)
			f.writes++
			writeJSON(w, body)
		case p == "/api/v1/applications/test":
			f.tests++
			writeJSON(w, map[string]any{"ok": true})
		case strings.HasPrefix(p, "/api/v1/applications/") && r.Method == http.MethodPut:
			var body apps.Item
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, a := range f.apps {
				if a.Int("id") == body.Int("id") {
					f.apps[i] = body
					f.writes++
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
	stack  *fakeStack
	config Config
	// pbh is the PeerBanHelper fake withApps wires, so a test can read what the
	// Job stored in it.
	pbh     *fakePbh
	runner  *Runner
	dir     string
	results []map[string]string
	// cmData models the wiring ConfigMap across runs: Upsert merges keys, and
	// LoadOwned reads owned.json back, as in a cluster.
	cmData map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := &fakeStack{
		cats: map[string]string{}, clients: map[string][]apps.Item{},
		folders: map[string]bool{}, hardlink: true,
		// The image's own runtime default: on the config volume, category paths off.
		prefs: map[string]any{"save_path": "/config/Downloads/", "temp_path": "/config/Downloads/temp/",
			"use_category_paths_in_manual_mode": false},
		bazarr:  map[string]map[string]any{},
		sabMisc: map[string]string{"complete_dir": "Downloads/complete", "download_dir": "Downloads/incomplete"},
		sabCats: map[string]string{},
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
	// The data paths are real, under a temp directory the run creates itself; a
	// fixed /data would need root and hide a failure to create them.
	data := filepath.Join(dir, "data")
	h.config = Config{
		Namespace: "seedbox", ConfigMap: "s5n-wiring", TimeoutSeconds: 10,
		Qbittorrent: &QbitConfig{URL: qbit.URL},
		Prowlarr:    &ProwlarrConfig{URL: prowlarr.URL, URLBase: "/prowlarr", SelfURL: prowlarr.URL + "/prowlarr", APIKeyFile: write("prowlarr.key", "prowlarr-key")},
		Instances: []Instance{
			{Name: "sonarr", Kind: "sonarr", URL: sonarr.URL, URLBase: "/sonarr", APIKeyFile: write("sonarr.key", "sonarr-key"),
				Category: "tv", SavePath: filepath.Join(data, "downloads/tv"), RootFolder: filepath.Join(data, "media/tv")},
			{Name: "radarr", Kind: "radarr", URL: radarr.URL, URLBase: "/radarr", APIKeyFile: write("radarr.key", "radarr-key"),
				Category: "movies", SavePath: filepath.Join(data, "downloads/movies"), RootFolder: filepath.Join(data, "media/movies")},
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
			if h.cmData == nil {
				h.cmData = map[string]string{}
			}
			for k, v := range data {
				h.cmData[k] = v
			}
			return nil
		},
		LoadOwned: func(_ context.Context) ([]byte, error) {
			v, ok := h.cmData["owned.json"]
			if !ok {
				return nil, nil
			}
			return []byte(v), nil
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
	// The data contract starts on the image's default and is corrected once.
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
	// The connection tests run every time: that is the proof, not a write -- one
	// client test and one application test per Arr.
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

// The third Arr gets what the other two get over /api/v1, plus the root folder's
// name and both profile ids (Lidarr.Api.V1/RootFolders/RootFolderController.cs at
// v3.1.2.4902).
func TestWiringWiresLidarr(t *testing.T) {
	h := newHarness(t)
	lidarr := httptest.NewServer(h.stack.arrHandler("lidarr", "/lidarr"))
	t.Cleanup(lidarr.Close)
	keyFile := filepath.Join(h.dir, "lidarr.key")
	if err := os.WriteFile(keyFile, []byte("lidarr-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(h.dir, "data", "media", "music")
	h.runner.Config.Instances = append(h.runner.Config.Instances, Instance{
		Name: "lidarr", Kind: "lidarr", URL: lidarr.URL, URLBase: "/lidarr",
		APIKeyFile: keyFile, Category: "music", DownloadCategory: "lidarr",
		SavePath:   filepath.Join(h.dir, "data", "downloads", "lidarr"),
		RootFolder: root,
	})

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	for _, name := range []string{
		"lidarr/ready", "lidarr/downloadclient", "lidarr/rootfolder", "lidarr/hardlinks",
		"prowlarr/application/lidarr", "qbittorrent/category/lidarr", "data/hardlink/music",
	} {
		step := stepByName(t, res, name)
		if step.Result != "ok" {
			t.Errorf("%s = %+v, want ok", name, step)
		}
	}
	clients := h.stack.clients["lidarr"]
	if len(clients) != 1 {
		t.Fatalf("lidarr has %d download clients, want 1", len(clients))
	}
	if v, _ := clients[0].FieldValue("musicCategory"); v != "lidarr" {
		t.Errorf("the client's category is %v, want lidarr", v)
	}
	if !h.stack.folders["lidarr:"+root] {
		t.Errorf("root folders = %v, want lidarr's own", h.stack.folders)
	}
	found := false
	for _, a := range h.stack.apps {
		if a.Str("implementation") == "Lidarr" {
			found = true
		}
	}
	if !found {
		t.Errorf("Prowlarr applications = %v, want a Lidarr one", h.stack.apps)
	}

	// A second run is a no-op for the third Arr too.
	writes := h.stack.count()
	second, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if got := h.stack.count(); got != writes {
		t.Fatalf("second run made %d API writes", got-writes)
	}
	if step := stepByName(t, second, "lidarr/downloadclient"); step.Detail != "unchanged" {
		t.Fatalf("second run lidarr/downloadclient = %+v", step)
	}
}

func TestWiringFailsLoudlyAndStillPublishes(t *testing.T) {
	h := newHarness(t)
	// With the login bypassed, an unreachable WebUI is the one way this step fails.
	h.config.Qbittorrent.URL = "http://127.0.0.1:1"
	h.config.TimeoutSeconds = 1
	h.runner.Config = h.config
	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatal("run succeeded with no qBittorrent answering")
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "qbittorrent/ready") {
		t.Fatalf("receipt = %+v", res)
	}
	if len(h.results) != 1 {
		t.Fatalf("failure receipt not published")
	}
}

// The Job never posts qBittorrent's login form: a failed attempt is what gets an
// address banned.
func TestWiringNeverPostsTheQbitLogin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if h.stack.logins != 0 {
		t.Fatalf("the Job posted /api/v2/auth/login %d times, want none", h.stack.logins)
	}
}

// An old install left a qBittorrent password in the Arr; both credential fields are
// owned, so the next run clears them and reports an update.
func TestWiringClearsAStoredQbittorrentPassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	host, port, err := hostPort(h.config.Qbittorrent.URL)
	if err != nil {
		t.Fatal(err)
	}
	// What 0.3.0 wrote: a real username and password.
	h.stack.clients["sonarr"] = []apps.Item{{
		"id": 7, "name": "qBittorrent", "enable": true, "priority": 1,
		"fields": []any{
			map[string]any{"name": "host", "value": host},
			map[string]any{"name": "port", "value": port},
			map[string]any{"name": "useSsl", "value": false},
			map[string]any{"name": "urlBase", "value": ""},
			map[string]any{"name": "username", "value": "admin"},
			map[string]any{"name": "password", "value": "hunter2", "privacy": "password"},
			map[string]any{"name": "tvCategory", "value": "tv"},
		},
	}}
	h.stack.nextID = 7

	res, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "sonarr/downloadclient")
	if step.Result != "ok" || step.Detail != "updated" {
		t.Fatalf("sonarr/downloadclient = %+v, want updated", step)
	}
	if len(step.Drift) != 1 || step.Drift[0].Field != "username" ||
		step.Drift[0].Old != "admin" || step.Drift[0].New != "" {
		t.Fatalf("drift = %+v, want the stored username reported as cleared", step.Drift)
	}
	client := h.stack.clients["sonarr"][0]
	if client.Int("id") != 7 {
		t.Fatalf("the client was recreated (id %d) instead of updated", client.Int("id"))
	}
	for _, field := range []string{"username", "password"} {
		if v, _ := client.FieldValue(field); v != "" {
			t.Fatalf("%s = %v, want it cleared", field, v)
		}
	}

	// Cleared once, it stays cleared without a write.
	if again, err := h.runner.Run(ctx); err != nil || again.Writes.Updated != 0 {
		t.Fatalf("run after clearing = %+v, %v, want no writes\n%s", again.Writes, err, Summary(again))
	}
}

// A failed owned.json read stops the run rather than adopting by name: a renamed
// client would be missed, duplicated, and the duplicate's id recorded. The receipt
// leaves the existing owned.json untouched.
func TestWiringStopsWhenOwnedRecordCannotBeRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := h.cmData["owned.json"]
	if before == "" {
		t.Fatal("first run recorded no owned.json")
	}
	writes := h.stack.count()

	h.runner.LoadOwned = func(context.Context) ([]byte, error) {
		return nil, errors.New("kubernetes API: 503: unavailable")
	}
	res, err := h.runner.Run(ctx)
	if err == nil {
		t.Fatal("run succeeded although owned.json could not be read")
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "wiring/owned") {
		t.Fatalf("receipt = %+v", res)
	}
	if got := h.stack.count(); got != writes {
		t.Fatalf("run wrote %d times to the apps after failing to read ownership", got-writes)
	}
	last := h.results[len(h.results)-1]
	if _, ok := last["owned.json"]; ok {
		t.Fatal("failed read published an owned.json over the existing record")
	}
	if h.cmData["owned.json"] != before {
		t.Fatal("owned.json changed after a failed read")
	}
}

// A release with no wired app still gets an ok receipt: `box apply` waits for one, so
// failing because there was nothing to do would make an apps-off seedbox
// impossible.
func TestWiringWithNoWiredAppReportsOK(t *testing.T) {
	var published []map[string]string
	r := &Runner{
		Config: Config{Namespace: "seedbox", ConfigMap: "s5n-wiring", TimeoutSeconds: 10},
		Now:    func() time.Time { return time.Unix(0, 0).UTC() },
		Publish: func(_ context.Context, data map[string]string) error {
			published = append(published, data)
			return nil
		},
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a run with nothing to wire failed: %v", err)
	}
	if res.Status != "ok" || res.Error != "" {
		t.Fatalf("receipt = %+v", res)
	}
	if res.Writes != (Writes{}) {
		t.Fatalf("a run with nothing to wire wrote something: %+v", res.Writes)
	}
	if len(res.Hardlinks) != 0 {
		t.Fatalf("a run with no download/library pair proved something: %+v", res.Hardlinks)
	}
	if len(published) != 1 {
		t.Fatalf("published %d receipts, want one", len(published))
	}
	if !strings.Contains(published[0]["summary.txt"], "s5n wiring: ok") {
		t.Fatalf("summary is not an ok receipt:\n%s", published[0]["summary.txt"])
	}

	// A Prowlarr is still wired when no Arr and no qBittorrent is: the empty
	// app list is not a reason to skip an app the config does name.
	if _, err := Load(writeFile(t, `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":10,"instances":[],`+
		`"prowlarr":{"url":"http://127.0.0.1:1","urlBase":"/prowlarr","selfUrl":"http://127.0.0.1:1/prowlarr",`+
		`"apiKeyFile":"/missing"}}`)); err != nil {
		t.Fatalf("a Prowlarr-only release was refused: %v", err)
	}
}

// writeFile puts one fixture in a temp file and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wiring.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A qBittorrent that answers 200 but keeps its own default must fail, not read as
// "updated".
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

// An incomplete directory the operator chose -- a migrated one, often a faster
// filesystem mounted inside the downloads tree -- is kept: setting it moves every
// automatic torrent's partial files, and anywhere under /data is reachable.
func TestWiringKeepsAnIncompleteDirectoryUnderData(t *testing.T) {
	h := newHarness(t)
	h.stack.prefs = map[string]any{"save_path": "/data/downloads", "temp_path": "/data/downloads/incomplete/",
		"use_category_paths_in_manual_mode": true}
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if got := h.stack.prefs["temp_path"]; got != "/data/downloads/incomplete/" {
		t.Fatalf("temp_path = %v, want the operator's /data/downloads/incomplete/ left alone", got)
	}
}

// One outside /data is where nothing is mounted, so the Job puts it at the default.
func TestWiringMovesAnIncompleteDirectoryOutsideData(t *testing.T) {
	h := newHarness(t)
	h.stack.prefs = map[string]any{"save_path": "/data/downloads", "temp_path": "/database/temp",
		"use_category_paths_in_manual_mode": true}
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if got := h.stack.prefs["temp_path"]; got != "/data/downloads/temp" {
		t.Fatalf("temp_path = %v, want /data/downloads/temp", got)
	}
}

// Every download/library pair is proven: a separate filesystem under one library is
// caught even when the first pair links.
func TestWiringProvesEveryPair(t *testing.T) {
	h := newHarness(t)
	h.stack.linkFail = h.config.Instances[1].SavePath
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

// stepByName finds one step in the receipt, failing the test if it is absent.
// One Bazarr instance pointed at the release's Sonarr and Radarr over its settings
// API, with the Secret's keys; a second run writes nothing.
func TestWiringWiresBazarr(t *testing.T) {
	h := newHarness(t)
	bazarrSrv := httptest.NewServer(h.stack.bazarrHandler("bazarr", "/bazarr", "bazarr-key"))
	t.Cleanup(bazarrSrv.Close)
	sonarrHost, sonarrPort := mustHostPort(t, h.config.Instances[0].URL)
	radarrHost, radarrPort := mustHostPort(t, h.config.Instances[1].URL)
	keyFile := filepath.Join(h.dir, "bazarr.key")
	if err := os.WriteFile(keyFile, []byte("bazarr-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.config.Bazarrs = append(h.config.Bazarrs, BazarrConfig{
		Name: "bazarr", URL: bazarrSrv.URL, URLBase: "/bazarr", APIKeyFile: keyFile,
		Sonarr: &BazarrSide{Host: sonarrHost, Port: sonarrPort, URLBase: "/sonarr", APIKeyFile: h.config.Instances[0].APIKeyFile},
		Radarr: &BazarrSide{Host: radarrHost, Port: radarrPort, URLBase: "/radarr", APIKeyFile: h.config.Instances[1].APIKeyFile},
	})
	h.runner.Config = h.config

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	for _, name := range []string{"bazarr/ready", "bazarr/sonarr", "bazarr/radarr"} {
		if step := stepByName(t, res, name); step.Result != "ok" {
			t.Errorf("%s = %+v, want ok", name, step)
		}
	}
	if got := h.stack.bazarrValue("bazarr", "sonarr", "ip"); got != sonarrHost {
		t.Errorf("bazarr sonarr.ip = %v, want %s", got, sonarrHost)
	}
	if got := h.stack.bazarrValue("bazarr", "sonarr", "apikey"); got != "sonarr-key" {
		t.Errorf("bazarr sonarr.apikey = %v, want the release Secret's", got)
	}
	if got := h.stack.bazarrValue("bazarr", "radarr", "port"); got != radarrPort {
		t.Errorf("bazarr radarr.port = %v, want %d", got, radarrPort)
	}
	if got := h.stack.bazarrValue("bazarr", "general", "use_radarr"); got != true {
		t.Errorf("bazarr general.use_radarr = %v", got)
	}

	writes := h.stack.count()
	second, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if got := h.stack.count(); got != writes {
		t.Fatalf("a second run made %d writes", got-writes)
	}
	if step := stepByName(t, second, "bazarr/sonarr"); step.Detail != "unchanged" {
		t.Fatalf("second run bazarr/sonarr = %+v", step)
	}
}

// A 4K Bazarr whose pair is only the 4K Radarr switches the missing side off.
func TestWiringBazarrTurnsOffTheSideItHasNoArrFor(t *testing.T) {
	h := newHarness(t)
	bazarrSrv := httptest.NewServer(h.stack.bazarrHandler("bazarr-4k", "/bazarr-4k", "b4k-key"))
	t.Cleanup(bazarrSrv.Close)
	h.stack.bazarr["bazarr-4k"]["general"].(map[string]any)["use_sonarr"] = true
	host, port := mustHostPort(t, h.config.Instances[1].URL)
	keyFile := filepath.Join(h.dir, "bazarr-4k.key")
	if err := os.WriteFile(keyFile, []byte("b4k-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	radarrKey := filepath.Join(h.dir, "radarr-4k.key")
	if err := os.WriteFile(radarrKey, []byte("radarr-4k-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.config.Bazarrs = append(h.config.Bazarrs, BazarrConfig{
		Name: "bazarr-4k", URL: bazarrSrv.URL, URLBase: "/bazarr-4k", APIKeyFile: keyFile,
		Radarr: &BazarrSide{Host: host, Port: port, URLBase: "/radarr-4k", APIKeyFile: radarrKey},
	})
	h.runner.Config = h.config

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if step := stepByName(t, res, "bazarr-4k/sonarr"); step.Result != "ok" || step.Detail != "updated" {
		t.Fatalf("bazarr-4k/sonarr = %+v, want updated (the switch was on)", step)
	}
	if got := h.stack.bazarrValue("bazarr-4k", "general", "use_sonarr"); got != false {
		t.Fatalf("bazarr-4k use_sonarr = %v, want false", got)
	}
	if got := h.stack.bazarrValue("bazarr-4k", "sonarr", "ip"); got != "127.0.0.1" {
		t.Fatalf("bazarr-4k sonarr.ip = %v, want it left alone", got)
	}
	if got := h.stack.bazarrValue("bazarr-4k", "radarr", "apikey"); got != "radarr-4k-key" {
		t.Fatalf("bazarr-4k radarr.apikey = %v", got)
	}
}

// sabnzbdHandler serves SABnzbd's API: version without a key, get_config and
// set_config with one, and the app's own nested answer shape.
func (f *fakeStack) sabnzbdHandler(urlBase, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.TrimPrefix(r.URL.Path, urlBase) != "/api" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		if q.Get("mode") == "version" {
			writeJSON(w, map[string]any{"version": "5.1.3"})
			return
		}
		if q.Get("apikey") != key {
			http.Error(w, "API Key Incorrect", http.StatusForbidden)
			return
		}
		switch q.Get("mode") {
		case "get_config":
			f.serveSabGet(w, q)
		case "set_config":
			f.serveSabSet(w, q)
		default:
			http.Error(w, "unexpected mode "+q.Get("mode"), http.StatusBadRequest)
		}
	}
}

func (f *fakeStack) serveSabGet(w http.ResponseWriter, q url.Values) {
	switch q.Get("section") {
	case "misc":
		key := q.Get("keyword")
		writeJSON(w, map[string]any{"config": map[string]any{"misc": map[string]any{key: f.sabMisc[key]}}})
	case "categories":
		f.serveSabCategories(w, q.Get("keyword"))
	}
}

func (f *fakeStack) serveSabCategories(w http.ResponseWriter, keyword string) {
	if keyword == "" {
		list := []any{}
		for cat, dir := range f.sabCats {
			list = append(list, map[string]any{"name": cat, "dir": dir})
		}
		writeJSON(w, map[string]any{"config": map[string]any{"categories": list}})
		return
	}
	name := strings.ToLower(keyword)
	dir, ok := f.sabCats[name]
	if !ok {
		writeJSON(w, map[string]any{"config": map[string]any{}})
		return
	}
	writeJSON(w, map[string]any{"config": map[string]any{"categories": []any{map[string]any{"name": name, "dir": dir}}}})
}

func (f *fakeStack) serveSabSet(w http.ResponseWriter, q url.Values) {
	f.writes++
	switch q.Get("section") {
	case "misc":
		f.sabMisc[q.Get("keyword")] = q.Get("value")
	case "categories":
		name := strings.ToLower(q.Get("name"))
		if name == "" {
			name = strings.ToLower(q.Get("keyword"))
		}
		f.sabCats[name] = q.Get("dir")
	}
	writeJSON(w, map[string]any{"config": map[string]any{}})
}

// mustHostPort is hostPort for a test: the fake's own address.
func mustHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	host, port, err := hostPort(raw)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func stepByName(t *testing.T, res *Result, name string) Step {
	t.Helper()
	for _, s := range res.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in receipt:\n%s", name, Summary(res))
	return Step{}
}

// With no owned.json the first run adopts by name and records the id, so a later
// process has something to trust.
func TestWiringRecordsOwnershipOnFirstRun(t *testing.T) {
	h := newHarness(t)
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	raw, ok := h.cmData["owned.json"]
	if !ok {
		t.Fatal("owned.json was not published")
	}
	rec, err := ParseOwnedRecord([]byte(raw))
	if err != nil {
		t.Fatalf("owned.json did not parse: %v", err)
	}
	if rec.ArrDownloadClientID("sonarr") == 0 || rec.ArrDownloadClientID("radarr") == 0 {
		t.Fatalf("download client ids not recorded: %+v", rec.Arr)
	}
	if rec.ProwlarrApplicationID("sonarr") == 0 || rec.ProwlarrApplicationID("radarr") == 0 {
		t.Fatalf("prowlarr application ids not recorded: %+v", rec.Prowlarr)
	}
}

// A UI rename is left alone (found by id), a drifted owned field is restored and
// reported with old/new, and a non-owned change survives.
func TestWiringFollowsRenameRestoresOwnedDriftKeepsNonOwned(t *testing.T) {
	h := newHarness(t)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	client := &h.stack.clients["sonarr"][0]
	(*client)["name"] = "qBit (renamed)"
	(*client)["priority"] = 7
	client.SetField("tvCategory", "user-edit")

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(res))
	}
	if len(h.stack.clients["sonarr"]) != 1 {
		t.Fatalf("rename caused a duplicate client: %d", len(h.stack.clients["sonarr"]))
	}
	if h.stack.clients["sonarr"][0]["name"] != "qBit (renamed)" {
		t.Fatal("rename was reverted; names are not owned")
	}
	if got := h.stack.clients["sonarr"][0].Int("priority"); got != 7 {
		t.Fatalf("priority (not owned) was overwritten: %d", got)
	}
	if v, _ := h.stack.clients["sonarr"][0].FieldValue("tvCategory"); v != "tv" {
		t.Fatalf("tvCategory (owned) was not restored: %v", v)
	}

	step := stepByName(t, res, "sonarr/downloadclient")
	if step.Result != "ok" || step.Detail != "updated" {
		t.Fatalf("sonarr/downloadclient step = %+v", step)
	}
	if len(step.Drift) != 1 || step.Drift[0].Field != "tvCategory" ||
		step.Drift[0].Old != "user-edit" || step.Drift[0].New != "tv" {
		t.Fatalf("drift not reported: %+v", step.Drift)
	}
}

// A deleted owned download client is recreated, and the receipt says so by
// naming the id that is now gone.
func TestWiringRecreatesDeletedOwnedDownloadClient(t *testing.T) {
	h := newHarness(t)
	if _, err := h.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldID := h.stack.clients["radarr"][0].Int("id")
	h.stack.clients["radarr"] = nil

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "radarr/downloadclient")
	if step.Result != "ok" || !strings.Contains(step.Detail, "recreated") || !strings.Contains(step.Detail, strconv.Itoa(oldID)) {
		t.Fatalf("radarr/downloadclient step = %+v, want recreated mentioning id %d", step, oldID)
	}
	if len(h.stack.clients["radarr"]) != 1 {
		t.Fatalf("recreate left %d clients", len(h.stack.clients["radarr"]))
	}
	rec, err := ParseOwnedRecord([]byte(h.cmData["owned.json"]))
	if err != nil {
		t.Fatal(err)
	}
	if rec.ArrDownloadClientID("radarr") == oldID || rec.ArrDownloadClientID("radarr") == 0 {
		t.Fatalf("owned.json still points at the deleted id %d: got %d", oldID, rec.ArrDownloadClientID("radarr"))
	}
}

// An unknown Arr kind has no Prowlarr implementation and must fail loudly; Readarr
// is the example because it is not a catalog app at all.
func TestWiringUnknownProwlarrKindErrors(t *testing.T) {
	h := newHarness(t)
	h.runner.Config.Instances[0].Kind = "readarr"

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with an unknown Arr kind\n%s", Summary(res))
	}
	if !strings.Contains(err.Error(), "unknown Arr kind") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	base := Config{
		Qbittorrent: &QbitConfig{URL: "http://qbittorrent:8080"},
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

// A migrated Arr keeps the category its torrents are in; qBittorrent's own category
// is left as it is, and no unused category is created.
func TestWiringKeepsAMigratedDownloadCategory(t *testing.T) {
	h := newHarness(t)
	inst := &h.runner.Config.Instances[0]
	inst.DownloadCategory = "TV"
	inst.SavePath = filepath.Join(h.dir, "data", "downloads", "TV")
	// The chart's own downloads root, which a category with no save path of
	// its own downloads into.
	h.runner.Config.Qbittorrent.SavePath = filepath.Join(h.dir, "data", "downloads")
	// The source's category has no save path, which means the downloads root.
	h.stack.cats["TV"] = ""
	writesBefore := h.stack.count()

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if _, ok := h.stack.cats["tv"]; ok {
		t.Error("box created its own tv category on a box that downloads into TV: nothing would use it")
	}
	if got := h.stack.cats["TV"]; got != "" {
		t.Errorf("the migrated category's save path was rewritten to %q; an empty one is the downloads root plus the name", got)
	}
	clients := h.stack.clients["sonarr"]
	if len(clients) != 1 {
		t.Fatalf("sonarr has %d download clients, want 1", len(clients))
	}
	if v, _ := clients[0].FieldValue("tvCategory"); v != "TV" {
		t.Errorf("the client's category is %v, want the migrated TV", v)
	}
	if len(res.Hardlinks) == 0 || !strings.HasSuffix(res.Hardlinks[0].Source, "/downloads/TV") {
		t.Errorf("the hardlink proof ran on %+v, want the downloads category TV", res.Hardlinks)
	}
	// The receipt names each directory by its tree's name.
	var made []string
	for _, step := range res.Steps {
		if strings.HasPrefix(step.Name, "data/dirs/downloads/") {
			made = append(made, step.Name)
		}
	}
	if strings.Join(made, " ") != "data/dirs/downloads/TV data/dirs/downloads/movies" {
		t.Errorf("the receipt's download directories are %v, want the download categories TV and movies", made)
	}
	if h.stack.count() == writesBefore {
		t.Error("the run wrote nothing at all")
	}
}
