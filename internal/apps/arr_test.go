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

func newFakeArr(t *testing.T, urlBase string) (*fakeArr, *Arr, *httptest.Server) {
	t.Helper()
	f := &fakeArr{urlBase: urlBase, hardlink: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewArr(Sonarr, "sonarr", srv.URL, urlBase, "test-key"), srv
}

func TestEnsureDownloadClientIsIdempotent(t *testing.T) {
	f, arr, _ := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{
		Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080,
		Username: "admin", Password: "hunter2",
	}

	if got, err := arr.EnsureDownloadClient(ctx, spec); err != nil || got != Created {
		t.Fatalf("first ensure = %v, %v; want created", got, err)
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
	if got, err := arr.EnsureDownloadClient(ctx, spec); err != nil || got != Unchanged {
		t.Fatalf("second ensure = %v, %v; want unchanged", got, err)
	}
	if f.writes != 1 {
		t.Fatalf("second run wrote %d times, want 0 more", f.writes-1)
	}
	if f.tests < 2 {
		t.Fatalf("connection test ran %d times, want one per run", f.tests)
	}
}

func TestEnsureDownloadClientRepairsDrift(t *testing.T) {
	f, arr, _ := newFakeArr(t, "/sonarr")
	ctx := context.Background()
	spec := DownloadClientSpec{Name: "qBittorrent", Category: "tv", Host: "qbittorrent", Port: 8080, Username: "admin", Password: "hunter2"}
	if _, err := arr.EnsureDownloadClient(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// Someone pointed the client at the wrong host.
	f.clients[0].SetField("host", "localhost")
	f.maskedClients()

	if got, err := arr.EnsureDownloadClient(ctx, spec); err != nil || got != Updated {
		t.Fatalf("ensure after drift = %v, %v; want updated", got, err)
	}
	if v, _ := f.clients[0].FieldValue("host"); v != "qbittorrent" {
		t.Fatalf("host not repaired: %v", v)
	}
}

func TestEnsureRootFolderAndHardlinks(t *testing.T) {
	f, arr, _ := newFakeArr(t, "/radarr")
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
