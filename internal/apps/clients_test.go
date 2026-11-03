package apps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeQbit answers like qBittorrent 5: 204 on login, a session cookie that every
// other call must carry, and a 409 on a duplicate category.
type fakeQbit struct {
	mu       sync.Mutex
	creds    [2]string
	cats     map[string]string
	writes   int
	sessions map[string]bool
}

func (f *fakeQbit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v2/auth/login":
		_ = r.ParseForm()
		if r.Header.Get("Referer") == "" {
			http.Error(w, "no referer", http.StatusForbidden)
			return
		}
		if r.PostForm.Get("username") != f.creds[0] || r.PostForm.Get("password") != f.creds[1] {
			_, _ = w.Write([]byte("Fails."))
			return
		}
		id := fmt.Sprintf("sid-%d", len(f.sessions)+1)
		f.sessions[id] = true
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: id, Path: "/"})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		_, _ = w.Write([]byte("v5.3.0"))

	default:
		c, err := r.Cookie("SID")
		if err != nil || !f.sessions[c.Value] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v2/torrents/categories":
			out := map[string]any{}
			for name, path := range f.cats {
				out[name] = map[string]any{"name": name, "savePath": path}
			}
			writeJSON(w, out)
		case "/api/v2/torrents/createCategory":
			_ = r.ParseForm()
			name := r.PostForm.Get("category")
			if _, exists := f.cats[name]; exists {
				http.Error(w, "Unable to create category", http.StatusConflict)
				return
			}
			f.cats[name] = r.PostForm.Get("savePath")
			f.writes++
		case "/api/v2/torrents/editCategory":
			_ = r.ParseForm()
			f.cats[r.PostForm.Get("category")] = r.PostForm.Get("savePath")
			f.writes++
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func TestQbitLoginAndCategories(t *testing.T) {
	f := &fakeQbit{creds: [2]string{"admin", "hunter2"}, cats: map[string]string{}, sessions: map[string]bool{}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	ctx := context.Background()
	q := NewQbit(srv.URL, "admin", "hunter2")
	if err := q.WaitReady(ctx, 0); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	if got, err := q.EnsureCategory(ctx, "tv", "/data/downloads/tv"); err != nil || got != Created {
		t.Fatalf("create = %v, %v", got, err)
	}
	if got, err := q.EnsureCategory(ctx, "tv", "/data/downloads/tv"); err != nil || got != Unchanged {
		t.Fatalf("second = %v, %v", got, err)
	}
	if f.writes != 1 {
		t.Fatalf("second run wrote: %d writes", f.writes)
	}
	if got, err := q.EnsureCategory(ctx, "tv", "/mnt/elsewhere"); err != nil || got != Updated {
		t.Fatalf("repair = %v, %v", got, err)
	}
	if f.cats["tv"] != "/mnt/elsewhere" {
		t.Fatalf("save path not repaired: %q", f.cats["tv"])
	}
}

func TestQbitRejectsWrongPassword(t *testing.T) {
	f := &fakeQbit{creds: [2]string{"admin", "right"}, cats: map[string]string{}, sessions: map[string]bool{}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	err := NewQbit(srv.URL, "admin", "wrong").Login(context.Background())
	if err == nil {
		t.Fatal("wrong password accepted")
	}
}

// fakeProwlarr serves the application schema and endpoints wiring uses.
type fakeProwlarr struct {
	mu      sync.Mutex
	apps    []Item
	writes  int
	tests   int
	nextID  int
	urlBase string
}

func (f *fakeProwlarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/prowlarr/ping":
		_, _ = w.Write([]byte("pong"))
	case p == "/prowlarr/api/v1/applications/schema":
		writeJSON(w, []Item{{
			"implementation": "Sonarr", "configContract": "SonarrSettings",
			"fields": []any{
				map[string]any{"name": "prowlarrUrl", "value": "http://localhost:9696"},
				map[string]any{"name": "baseUrl", "value": "http://localhost:8989"},
				map[string]any{"name": "apiKey", "privacy": "apiKey"},
				map[string]any{"name": "authUsername"},
				map[string]any{"name": "authPassword"},
			},
		}})
	case p == "/prowlarr/api/v1/applications" && r.Method == http.MethodGet:
		writeJSON(w, f.apps)
	case p == "/prowlarr/api/v1/applications" && r.Method == http.MethodPost:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		f.nextID++
		body["id"] = f.nextID
		f.apps = append(f.apps, body)
		f.writes++
		writeJSON(w, body)
	case p == "/prowlarr/api/v1/applications/test":
		f.tests++
		writeJSON(w, map[string]any{"ok": true})
	case strings.HasPrefix(p, "/prowlarr/api/v1/applications/") && r.Method == http.MethodPut:
		body, ok := readBody(w, r)
		if !ok {
			return
		}
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
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
	}
}

func (f *fakeProwlarr) maskedApps() {
	for _, a := range f.apps {
		for _, raw := range a["fields"].([]any) {
			field := raw.(map[string]any)
			if field["name"] == "apiKey" {
				field["value"] = "********"
			}
		}
	}
}

func TestProwlarrEnsureApplicationIsIdempotent(t *testing.T) {
	f := &fakeProwlarr{urlBase: "/prowlarr"}
	srv := httptest.NewServer(f)
	defer srv.Close()

	ctx := context.Background()
	p := NewProwlarr(srv.URL, "/prowlarr", "http://prowlarr:9696/prowlarr", "key")
	spec := ApplicationSpec{Name: "sonarr", Implementation: "Sonarr", BaseURL: "http://sonarr:8989/sonarr", APIKey: "arrkey"}

	result, err := p.EnsureApplication(ctx, spec, 0)
	if err != nil || result.Change != Created {
		t.Fatalf("first = %+v, %v", result, err)
	}
	if result.ID == 0 {
		t.Fatal("created application has no id to record")
	}
	if v, _ := f.apps[0].FieldValue("prowlarrUrl"); v != "http://prowlarr:9696/prowlarr" {
		t.Fatalf("prowlarrUrl = %v", v)
	}
	if v, _ := f.apps[0].FieldValue("baseUrl"); v != "http://sonarr:8989/sonarr" {
		t.Fatalf("baseUrl = %v", v)
	}

	// The API masks the key on read; the second run must still be a no-op.
	f.maskedApps()
	result, err = p.EnsureApplication(ctx, spec, result.ID)
	if err != nil || result.Change != Unchanged {
		t.Fatalf("second = %+v, %v", result, err)
	}
	if f.writes != 1 {
		t.Fatalf("second run wrote: %d", f.writes)
	}
	if f.tests != 2 {
		t.Fatalf("connection test ran %d times, want 2", f.tests)
	}
}

// syncLevel is not owned: the user's choice in the Prowlarr UI must survive.
func TestProwlarrEnsureApplicationSurvivesNonOwnedFieldChange(t *testing.T) {
	f := &fakeProwlarr{urlBase: "/prowlarr"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	p := NewProwlarr(srv.URL, "/prowlarr", "http://prowlarr:9696/prowlarr", "key")
	spec := ApplicationSpec{Name: "sonarr", Implementation: "Sonarr", BaseURL: "http://sonarr:8989/sonarr", APIKey: "arrkey"}
	first, err := p.EnsureApplication(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.apps[0]["syncLevel"] = "addOnly"
	f.maskedApps()

	if result, err := p.EnsureApplication(ctx, spec, first.ID); err != nil || result.Change != Unchanged {
		t.Fatalf("ensure over a non-owned change = %+v, %v; want unchanged", result, err)
	}
	if f.apps[0]["syncLevel"] != "addOnly" {
		t.Fatalf("syncLevel was overwritten: %v", f.apps[0]["syncLevel"])
	}
}

// baseUrl is owned: drift is restored and reported with old/new.
func TestProwlarrEnsureApplicationRepairsDriftAndReportsIt(t *testing.T) {
	f := &fakeProwlarr{urlBase: "/prowlarr"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	p := NewProwlarr(srv.URL, "/prowlarr", "http://prowlarr:9696/prowlarr", "key")
	spec := ApplicationSpec{Name: "sonarr", Implementation: "Sonarr", BaseURL: "http://sonarr:8989/sonarr", APIKey: "arrkey"}
	first, err := p.EnsureApplication(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.apps[0].SetField("baseUrl", "http://wrong:8989/sonarr")
	f.maskedApps()

	result, err := p.EnsureApplication(ctx, spec, first.ID)
	if err != nil || result.Change != Updated {
		t.Fatalf("ensure after drift = %+v, %v; want updated", result, err)
	}
	if v, _ := f.apps[0].FieldValue("baseUrl"); v != "http://sonarr:8989/sonarr" {
		t.Fatalf("baseUrl not repaired: %v", v)
	}
	if len(result.Drift) != 1 || result.Drift[0].Field != "baseUrl" ||
		result.Drift[0].Old != "http://wrong:8989/sonarr" || result.Drift[0].New != "http://sonarr:8989/sonarr" {
		t.Fatalf("drift not reported correctly: %+v", result.Drift)
	}
}

// A rename never creates a duplicate registration: the id is what matters.
func TestProwlarrEnsureApplicationFollowsRename(t *testing.T) {
	f := &fakeProwlarr{urlBase: "/prowlarr"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	p := NewProwlarr(srv.URL, "/prowlarr", "http://prowlarr:9696/prowlarr", "key")
	spec := ApplicationSpec{Name: "sonarr", Implementation: "Sonarr", BaseURL: "http://sonarr:8989/sonarr", APIKey: "arrkey"}
	first, err := p.EnsureApplication(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.apps[0]["name"] = "sonarr (renamed)"
	f.maskedApps()

	result, err := p.EnsureApplication(ctx, spec, first.ID)
	if err != nil || result.Change != Unchanged {
		t.Fatalf("ensure after rename = %+v, %v; want unchanged", result, err)
	}
	if len(f.apps) != 1 {
		t.Fatalf("rename caused a duplicate: %d applications", len(f.apps))
	}
}

// A deleted owned application is recreated and the previous id is reported.
func TestProwlarrEnsureApplicationRecreatesWhenDeleted(t *testing.T) {
	f := &fakeProwlarr{urlBase: "/prowlarr"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	p := NewProwlarr(srv.URL, "/prowlarr", "http://prowlarr:9696/prowlarr", "key")
	spec := ApplicationSpec{Name: "sonarr", Implementation: "Sonarr", BaseURL: "http://sonarr:8989/sonarr", APIKey: "arrkey"}
	first, err := p.EnsureApplication(ctx, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.apps = nil

	result, err := p.EnsureApplication(ctx, spec, first.ID)
	if err != nil || result.Change != Created || !result.Recreated || result.PreviousID != first.ID {
		t.Fatalf("recreate after delete = %+v, %v", result, err)
	}
	if result.ID == 0 || result.ID == first.ID {
		t.Fatalf("recreated application did not get a fresh id: %d (previous %d)", result.ID, first.ID)
	}
}
