package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// qbitSchema is the real schema endpoint's shape for qBittorrent, trimmed to the
// fields wiring touches.
func qbitSchema() []Item {
	fields := []Item{
		{"name": "host", "type": "textbox"},
		{"name": "port", "type": "textbox"},
		{"name": "useSsl", "type": "checkbox"},
		{"name": "urlBase", "type": "textbox"},
		{"name": "username", "type": "textbox"},
		{"name": "password", "type": "password", "privacy": "password"},
		{"name": "tvCategory", "type": "textbox"},
		{"name": "movieCategory", "type": "textbox"},
		{"name": "musicCategory", "type": "textbox"},
	}
	raw := make([]any, 0, len(fields))
	for _, f := range fields {
		raw = append(raw, map[string]any(f))
	}
	return []Item{{"implementation": "QBittorrent", "configContract": "QBittorrentSettings", "fields": raw}}
}

// fakeArr is a minimal Arr with a write counter, in the app's own shape: Lidarr
// answers /api/v1, names the category musicCategory, and validates its root folder.
type fakeArr struct {
	mu       sync.Mutex
	urlBase  string
	api      string
	kind     Kind
	clients  []Item
	schemas  []Item
	folders  []Item
	hardlink bool
	writes   int
	tests    int
	nextID   int
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, f.urlBase)
	api := f.api
	if api == "" {
		api = "/api/v3"
	}
	switch {
	case p == "/ping":
		_, _ = w.Write([]byte("pong"))
	case p == api+"/downloadclient/schema" && r.Method == http.MethodGet:
		schemas := f.schemas
		if schemas == nil {
			schemas = qbitSchema()
		}
		writeJSON(w, schemas)
	case p == api+"/downloadclient" && r.Method == http.MethodGet:
		writeJSON(w, f.clients)
	case p == api+"/downloadclient" && r.Method == http.MethodPost:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		f.nextID++
		body["id"] = f.nextID
		f.clients = append(f.clients, body)
		f.writes++
		writeJSON(w, body)
	case strings.HasPrefix(p, api+"/downloadclient/") && strings.HasSuffix(p, "/test"):
		f.tests++
		writeJSON(w, map[string]any{"ok": true})
	case strings.HasPrefix(p, api+"/downloadclient/") && r.Method == http.MethodPut:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		for i, c := range f.clients {
			if c.Int("id") == body.Int("id") {
				f.clients[i] = body
				f.writes++
				writeJSON(w, body)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	case p == api+"/qualityprofile" && r.Method == http.MethodGet:
		writeJSON(w, []Item{{"id": 1, "name": "Any"}, {"id": 2, "name": "Lossless"}})
	case p == api+"/metadataprofile" && r.Method == http.MethodGet:
		writeJSON(w, []Item{{"id": 2, "name": "Standard"}, {"id": 4, "name": "None"}})
	case p == api+"/rootfolder" && r.Method == http.MethodGet:
		writeJSON(w, f.folders)
	case p == api+"/rootfolder" && r.Method == http.MethodPost:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		if f.kind == Lidarr {
			// Lidarr's RootFolderController needs a name and both positive profile
			// ids (SharedValidator at v3.1.2.4902).
			if body.Str("name") == "" || body.Int("defaultQualityProfileId") <= 0 || body.Int("defaultMetadataProfileId") <= 0 {
				http.Error(w, "Name must not be empty, and both profiles must be chosen", http.StatusBadRequest)
				return
			}
		}
		f.writes++
		stored := body.Clone()
		stored["accessible"] = true
		f.folders = append(f.folders, stored)
		writeJSON(w, stored)
	case p == api+"/config/mediamanagement" && r.Method == http.MethodGet:
		writeJSON(w, Item{"id": 1, "copyUsingHardlinks": f.hardlink})
	case p == api+"/config/mediamanagement" && r.Method == http.MethodPut:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		f.hardlink = body.Bool("copyUsingHardlinks")
		f.writes++
		writeJSON(w, body)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeArr) maskedClients() {
	for _, c := range f.clients {
		for _, raw := range c["fields"].([]any) {
			field := raw.(map[string]any)
			if field["name"] == "password" {
				field["value"] = "********"
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// readBody decodes a request body, answering 400 rather than failing in a handler.
func readBody(w http.ResponseWriter, r *http.Request) (Item, bool) {
	var b Item
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return b, true
}

func newFakeArr(t *testing.T, urlBase string) (*fakeArr, *Arr) {
	t.Helper()
	f := &fakeArr{urlBase: urlBase, hardlink: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewArr(Sonarr, "sonarr", srv.URL, urlBase, "test-key")
}

func TestEnsureDownloadClientIsIdempotent(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{
		Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080,
		Username: "admin", Password: "hunter2",
	}

	// ownedID 0: nothing recorded, so this creates and returns the id to record.
	result, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil || result.Change != Created {
		t.Fatalf("first ensure = %+v, %v; want created", result, err)
	}
	if result.ID == 0 {
		t.Fatal("created client has no id to record")
	}
	if len(f.clients) != 1 || f.writes != 1 {
		t.Fatalf("writes = %d, clients = %d; want 1", f.writes, len(f.clients))
	}
	created := f.clients[0]
	if v, _ := created.FieldValue("password"); v != "hunter2" {
		t.Fatalf("password not sent: %v", v)
	}
	if v, _ := created.FieldValue("tvCategory"); v != "tv" {
		t.Fatalf("category not sent: %v", v)
	}
	if v, _ := created.FieldValue("host"); v != "qbittorrent" {
		t.Fatalf("host not sent: %v", v)
	}

	// The API never returns the password; the second run must still be a no-op.
	f.maskedClients()
	result, err = arr.EnsureDownloadClient(ctx, spec, result.ID)
	if err != nil || result.Change != Unchanged {
		t.Fatalf("second ensure = %+v, %v; want unchanged", result, err)
	}
	if len(result.Drift) != 0 {
		t.Fatalf("second ensure reported drift: %+v", result.Drift)
	}
	if f.writes != 1 {
		t.Fatalf("second run wrote %d times, want 0 more", f.writes-1)
	}
	if f.tests < 2 {
		t.Fatalf("connection test ran %d times, want one per run", f.tests)
	}
}

func TestEnsureDownloadClientRepairsDrift(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Someone pointed the client at the wrong host.
	f.clients[0].SetField("host", "localhost")
	f.maskedClients()

	result, err := arr.EnsureDownloadClient(ctx, spec, first.ID)
	if err != nil || result.Change != Updated {
		t.Fatalf("ensure after drift = %+v, %v; want updated", result, err)
	}
	if v, _ := f.clients[0].FieldValue("host"); v != "qbittorrent" {
		t.Fatalf("host not repaired: %v", v)
	}
	if len(result.Drift) != 1 || result.Drift[0].Field != "host" || result.Drift[0].Old != "localhost" || result.Drift[0].New != "qbittorrent" {
		t.Fatalf("drift not reported correctly: %+v", result.Drift)
	}
}

// A field the Job does not own must survive an ensure: the object is overlaid, not
// replaced.
func TestEnsureDownloadClientSurvivesNonOwnedFieldChange(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The user changed the client's priority and tags in the Sonarr UI.
	f.clients[0]["priority"] = 7
	f.clients[0]["tags"] = []any{"anime"}
	f.maskedClients()

	if result, err := arr.EnsureDownloadClient(ctx, spec, first.ID); err != nil || result.Change != Unchanged {
		t.Fatalf("ensure over a non-owned change = %+v, %v; want unchanged", result, err)
	}
	if got := f.clients[0].Int("priority"); got != 7 {
		t.Fatalf("priority was overwritten: %d", got)
	}
	if got := f.clients[0].TagList("tags"); len(got) != 1 || got[0] != "anime" {
		t.Fatalf("tags were overwritten: %v", got)
	}
}

// A UI rename must not create a second client: the recorded id is what makes an
// object ours.
func TestEnsureDownloadClientFollowsRename(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.clients[0]["name"] = "qBit (renamed)"
	f.clients[0].SetField("tvCategory", "something-else")
	f.maskedClients()

	result, err := arr.EnsureDownloadClient(ctx, spec, first.ID)
	if err != nil || result.Change != Updated {
		t.Fatalf("ensure after rename = %+v, %v; want updated", result, err)
	}
	if len(f.clients) != 1 {
		t.Fatalf("rename caused a duplicate: %d clients", len(f.clients))
	}
	if f.clients[0]["name"] != "qBit (renamed)" {
		t.Fatalf("name was touched: %v", f.clients[0]["name"])
	}
	if v, _ := f.clients[0].FieldValue("tvCategory"); v != "tv" {
		t.Fatalf("category not restored after rename: %v", v)
	}
}

// A deleted owned client is recreated, and the receipt names the old id.
func TestEnsureDownloadClientRecreatesWhenDeleted(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.clients = nil // the user deleted the client entirely

	result, err := arr.EnsureDownloadClient(ctx, spec, first.ID)
	if err != nil || result.Change != Created || !result.Recreated || result.PreviousID != first.ID {
		t.Fatalf("recreate after delete = %+v, %v", result, err)
	}
	if result.ID == 0 || result.ID == first.ID {
		t.Fatalf("recreated client did not get a fresh id: %d (previous %d)", result.ID, first.ID)
	}
	if len(f.clients) != 1 {
		t.Fatalf("recreate left %d clients, want 1", len(f.clients))
	}
}

// With no record, an existing client named spec.Name is adopted and its id returned.
func TestEnsureDownloadClientAdoptsByNameWithoutOwnedID(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.maskedClients()

	// Simulate a fresh process with no owned.json: ownedID 0 again, against the
	// very same live client.
	result, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil || result.Change != Unchanged || result.ID != first.ID {
		t.Fatalf("adopt by name = %+v, %v; want unchanged with id %d", result, err, first.ID)
	}
	if len(f.clients) != 1 {
		t.Fatalf("adoption created a duplicate: %d clients", len(f.clients))
	}
}

// A client the user added by hand is never written, tested or mistaken for ours.
func TestEnsureDownloadClientNeverTouchesUserAddedSecondClient(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.nextID++
	mine := Item{"id": f.nextID, "name": "mine", "implementation": "Transmission", "priority": 3, "tags": []any{}}
	f.clients = append(f.clients, mine)
	// A shallow copy: Clone's JSON round trip turns ints into float64.
	before := Item{}
	for k, v := range mine {
		before[k] = v
	}
	writesBefore, testsBefore := f.writes, f.tests

	if result, err := arr.EnsureDownloadClient(ctx, spec, first.ID); err != nil || result.Change != Unchanged {
		t.Fatalf("ensure with a user-added client present = %+v, %v", result, err)
	}
	if f.writes != writesBefore {
		t.Fatalf("the user-added client was written to: %d extra writes", f.writes-writesBefore)
	}
	if f.tests != testsBefore+1 {
		t.Fatalf("connection tests = %d, want exactly 1 more (the owned client only)", f.tests-testsBefore)
	}
	for _, c := range f.clients {
		if c.Int("id") == mine.Int("id") {
			if !reflect.DeepEqual(c, before) {
				t.Fatalf("user-added client changed: got %+v, want %+v", c, before)
			}
			return
		}
	}
	t.Fatal("user-added client disappeared")
}

// A masked password is never written back as the secret nor shown in the drift.
func TestEnsureDownloadClientNeverLeaksMaskedSecret(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.maskedClients()
	// Also drift an owned field so an update (and a PUT body) actually happens.
	f.clients[0].SetField("host", "localhost")

	result, err := arr.EnsureDownloadClient(ctx, spec, first.ID)
	if err != nil || result.Change != Updated {
		t.Fatalf("ensure = %+v, %v", result, err)
	}
	for _, d := range result.Drift {
		if strings.Contains(d.Field, "password") || strings.Contains(d.Old, "hunter2") || strings.Contains(d.New, "hunter2") {
			t.Fatalf("secret leaked into drift: %+v", d)
		}
	}
	if v, _ := f.clients[0].FieldValue("password"); v != "hunter2" {
		t.Fatalf("PUT body did not carry the real password forward: %v", v)
	}
	// The masked placeholder must never have been written back as the secret.
	b, _ := json.Marshal(f.clients[0])
	if strings.Contains(string(b), "********") {
		t.Fatalf("masked placeholder ended up in the stored client: %s", b)
	}
}

func TestEnsureRootFolderAndHardlinks(t *testing.T) {
	f, arr := newFakeArr(t, "/radarr")
	ctx := context.Background()

	if got, err := arr.EnsureRootFolder(ctx, "/data/media/movies"); err != nil || got != Created {
		t.Fatalf("create root folder = %v, %v", got, err)
	}
	if got, err := arr.EnsureRootFolder(ctx, "/data/media/movies"); err != nil || got != Unchanged {
		t.Fatalf("second root folder = %v, %v", got, err)
	}
	// An inaccessible folder is a real fault, not something to paper over.
	f.folders[0]["accessible"] = false
	if _, err := arr.EnsureRootFolder(ctx, "/data/media/movies"); err == nil {
		t.Fatal("inaccessible root folder was accepted")
	}

	if got, err := arr.EnsureHardlinks(ctx); err != nil || got != Unchanged {
		t.Fatalf("hardlinks already on = %v, %v", got, err)
	}
	f.hardlink = false
	if got, err := arr.EnsureHardlinks(ctx); err != nil || got != Updated {
		t.Fatalf("hardlinks off = %v, %v; want updated", got, err)
	}
	if !f.hardlink {
		t.Fatal("hardlinks not enabled")
	}
}

// Lidarr differs in three ways: /api/v1 (Lidarr.Http/VersionedApiControllerAttribute.cs
// at v3.1.2.4902), musicCategory, and a root folder validated with a name and both
// positive profile ids (RootFolderController.cs), so the box chooses its first ones.
func TestLidarrIsAPIv1WithItsOwnCategoryAndRootFolder(t *testing.T) {
	f := &fakeArr{urlBase: "/lidarr", api: "/api/v1", kind: Lidarr, hardlink: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	arr := NewArr(Lidarr, "lidarr", srv.URL, "/lidarr", "test-key")
	ctx := context.Background()

	result, err := arr.EnsureDownloadClient(ctx, DownloadClientSpec{
		Name: "qBittorrent", Category: "lidarr", Host: "qbittorrent", Port: 8080,
	}, 0)
	if err != nil || result.Change != Created {
		t.Fatalf("download client = %+v, %v; want created", result, err)
	}
	if v, _ := f.clients[0].FieldValue("musicCategory"); v != "lidarr" {
		t.Fatalf("musicCategory = %v, want the instance's category", v)
	}
	if _, ok := f.clients[0].FieldValue("tvCategory"); ok {
		if v, _ := f.clients[0].FieldValue("tvCategory"); v != nil {
			t.Fatalf("a Lidarr client was given tvCategory = %v", v)
		}
	}

	if got, err := arr.EnsureRootFolder(ctx, "/data/media/music"); err != nil || got != Created {
		t.Fatalf("create root folder = %v, %v", got, err)
	}
	if len(f.folders) != 1 {
		t.Fatalf("root folders = %d, want 1", len(f.folders))
	}
	folder := f.folders[0]
	if folder.Str("name") != "music" {
		t.Errorf("root folder name = %q, want the directory's name", folder.Str("name"))
	}
	if folder.Int("defaultQualityProfileId") != 1 || folder.Int("defaultMetadataProfileId") != 2 {
		t.Errorf("root folder profiles = %d/%d, want the app's first quality and metadata profile",
			folder.Int("defaultQualityProfileId"), folder.Int("defaultMetadataProfileId"))
	}
	if got, err := arr.EnsureRootFolder(ctx, "/data/media/music"); err != nil || got != Unchanged {
		t.Fatalf("second root folder = %v, %v; want unchanged", got, err)
	}

	if got, err := arr.EnsureHardlinks(ctx); err != nil || got != Unchanged {
		t.Fatalf("hardlinks already on = %v, %v", got, err)
	}
	f.hardlink = false
	if got, err := arr.EnsureHardlinks(ctx); err != nil || got != Updated {
		t.Fatalf("hardlinks off = %v, %v; want updated", got, err)
	}
}

func TestWaitReadyRetriesUntilPingSucceeds(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	arr := NewArr(Sonarr, "sonarr", srv.URL, "", "k")
	if err := arr.WaitReady(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

// Both credential fields are owned as empty: an older install's password is cleared
// and the receipt says so.
func TestEnsureDownloadClientClearsStoredCredentials(t *testing.T) {
	f, arr := newFakeArr(t, "/sonarr")
	ctx := context.Background()

	// What the previous release left behind: a real password in the Arr.
	old := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080,
		Username: "admin", Password: "hunter2"}
	first, err := arr.EnsureDownloadClient(ctx, old, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.maskedClients()

	empty := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080}
	result, err := arr.EnsureDownloadClient(ctx, empty, first.ID)
	if err != nil {
		t.Fatalf("ensure with empty credentials: %v", err)
	}
	if result.Change != Updated {
		t.Fatalf("clearing the stored credentials reported %s, want updated", result.Change)
	}
	for _, field := range []string{"username", "password"} {
		if v, _ := f.clients[0].FieldValue(field); v != "" {
			t.Fatalf("%s = %v, want it cleared", field, v)
		}
	}
	// And once cleared, the same spec is a no-op again.
	f.maskedClients()
	if again, err := arr.EnsureDownloadClient(ctx, empty, result.ID); err != nil || again.Change != Unchanged {
		t.Fatalf("run after clearing = %+v, %v, want unchanged", again, err)
	}
}

// migratedClient is a source install's client: the operator's name, the release's
// host, and the credential it had.
func migratedClient(id int, name string) Item {
	return Item{
		"id": id, "name": name, "implementation": "QBittorrent", "enable": true,
		"protocol": "torrent", "priority": 1, "tags": []any{},
		"fields": []any{
			map[string]any{"name": "host", "value": "qbittorrent"},
			map[string]any{"name": "port", "value": float64(8080)},
			map[string]any{"name": "useSsl", "value": false},
			map[string]any{"name": "urlBase", "value": ""},
			map[string]any{"name": "username", "value": "admin"},
			map[string]any{"name": "password", "value": "hunter2"},
			map[string]any{"name": "movieCategory", "value": "Movies"},
		},
	}
}

// A source client has the operator's name, so only its host finds it: it is adopted
// with its name kept.
func TestEnsureDownloadClientAdoptsTheMigratedClientByHost(t *testing.T) {
	f, arr := newFakeArr(t, "/radarr")
	f.clients = []Item{migratedClient(7, "qBT")}
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "Movies", Host: "qbittorrent", Port: 8080}

	result, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatalf("EnsureDownloadClient: %v", err)
	}
	if len(f.clients) != 1 {
		t.Fatalf("the migrated client got a second one beside it: %d clients", len(f.clients))
	}
	if result.ID != 7 {
		t.Errorf("adopted id %d, want the migrated client's 7", result.ID)
	}
	if result.Change != Updated {
		t.Errorf("first run = %s, want updated: the credential the source stored is cleared", result.Change)
	}
	if f.clients[0]["name"] != "qBT" {
		t.Errorf("the operator's name was overwritten with %v", f.clients[0]["name"])
	}
	// The second run is a no-op: the client is now box's, and nothing about it
	// changes again.
	second, err := arr.EnsureDownloadClient(ctx, spec, result.ID)
	if err != nil || second.Change != Unchanged {
		t.Fatalf("second run = %+v, %v; want unchanged", second, err)
	}
}

// Two clients at one host cannot be chosen between, so it is a refusal.
func TestEnsureDownloadClientRefusesTwoClientsAtOneHost(t *testing.T) {
	f, arr := newFakeArr(t, "/radarr")
	f.clients = []Item{
		migratedClient(7, "qBT"),
		migratedClient(9, "the other one"),
	}
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "Movies", Host: "qbittorrent", Port: 8080}

	result, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err == nil {
		t.Fatalf("two clients at %s were accepted: %+v", spec.Host, result)
	}
	for _, want := range []string{"7", "9", spec.Host} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if f.writes != 0 {
		t.Errorf("the refused run wrote %d times", f.writes)
	}
}

// A recorded id wins over two at the host.
func TestEnsureDownloadClientOwnedIDWinsOverTwoAtTheHost(t *testing.T) {
	f, arr := newFakeArr(t, "/radarr")
	f.clients = []Item{
		migratedClient(7, "qBT"),
		migratedClient(9, "the other one"),
	}
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "Movies", Host: "qbittorrent", Port: 8080}

	result, err := arr.EnsureDownloadClient(ctx, spec, 7)
	if err != nil {
		t.Fatalf("the recorded id was refused: %v", err)
	}
	if result.ID != 7 {
		t.Fatalf("adopted id %d, want the recorded 7", result.ID)
	}
	if len(f.clients) != 2 {
		t.Fatalf("the run touched the second client: %d clients", len(f.clients))
	}
}

// A disabled client, or one of another type, at the address is not ours.
func TestEnsureDownloadClientIgnoresAClientBoxDoesNotDial(t *testing.T) {
	f, arr := newFakeArr(t, "/radarr")
	disabled := migratedClient(7, "qBT")
	disabled["enable"] = false
	transmission := migratedClient(8, "the source's transmission")
	transmission["implementation"] = "Transmission"
	f.clients = []Item{disabled, transmission}
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "Movies", Host: "qbittorrent", Port: 8080}

	result, err := arr.EnsureDownloadClient(ctx, spec, 0)
	if err != nil {
		t.Fatalf("EnsureDownloadClient: %v", err)
	}
	if result.Change != Created {
		t.Fatalf("a disabled client and another type were adopted: %+v", result)
	}
	if len(f.clients) != 3 {
		t.Fatalf("clients = %d, want the two the operator has plus box's own", len(f.clients))
	}
}
