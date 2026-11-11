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

// qbitSchema is the shape the real Sonarr/Radarr schema endpoint returns for
// qBittorrent, trimmed to the fields wiring touches.
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
	}
	raw := make([]any, 0, len(fields))
	for _, f := range fields {
		raw = append(raw, map[string]any(f))
	}
	return []Item{{"implementation": "QBittorrent", "configContract": "QBittorrentSettings", "fields": raw}}
}

// fakeArr is a minimal Sonarr/Radarr: schema, download clients, root folders and
// media management, with a write counter so idempotency is testable.
type fakeArr struct {
	mu       sync.Mutex
	urlBase  string
	clients  []Item
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
	api := "/api/v3"
	switch {
	case p == "/ping":
		_, _ = w.Write([]byte("pong"))
	case p == api+"/downloadclient/schema" && r.Method == http.MethodGet:
		writeJSON(w, qbitSchema())
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
	case p == api+"/rootfolder" && r.Method == http.MethodGet:
		writeJSON(w, f.folders)
	case p == api+"/rootfolder" && r.Method == http.MethodPost:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		f.writes++
		f.folders = append(f.folders, Item{"path": body.Str("path"), "accessible": true})
		writeJSON(w, f.folders[len(f.folders)-1])
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

// readBody decodes a request body, answering 400 instead of failing a test from
// inside a handler goroutine.
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

	// ownedID 0: nothing recorded yet, so this both creates and tells the
	// caller the new id to record.
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

// A field the wiring Job does not own (priority, here) must survive an
// ensure even though it differs from what Create would have written: the
// object already exists and is being overlaid, not replaced.
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

// A rename in the Sonarr UI must not cause a second client to be created: the
// recorded id is what makes an object "ours", not its name.
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

// A deleted owned client must be recreated, not silently dropped, and the
// receipt must be able to say what the old id was.
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

// With no owned.json (ownedID 0), an existing client named spec.Name is
// adopted rather than duplicated, and its id comes back so the caller can
// record it.
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

// A second download client the user added by hand is never written to, never
// tested, and never mistaken for the owned one.
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
	// A shallow copy, not Clone(): Clone round-trips through JSON and would
	// turn the literal's int fields into float64, making the comparison below
	// about this test's fixture instead of about what the Job wrote.
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

// A masked password must never be written back as if it were the real
// secret, and it must never appear in the receipt's drift list.
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
