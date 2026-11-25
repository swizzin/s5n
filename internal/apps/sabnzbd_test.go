package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeSabnzbd answers like SABnzbd 5: mode=version without a key, everything
// else with one, get_config in the app's own nested shape, and set_config that
// applies what it is given unless the test says otherwise.
type fakeSabnzbd struct {
	mu      sync.Mutex
	key     string
	misc    map[string]string
	cats    map[string]string
	writes  int
	sticky  bool // set_config reports success and changes nothing
	forbid  bool
	lastSet map[string]string
}

func newFakeSabnzbd() *fakeSabnzbd {
	return &fakeSabnzbd{
		key:  "0123456789abcdef0123456789abcdef",
		misc: map[string]string{"complete_dir": "Downloads/complete", "download_dir": "Downloads/incomplete"},
		cats: map[string]string{},
	}
}

func (f *fakeSabnzbd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forbid {
		http.Error(w, "Refused connection with hostname", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/sabnzbd/api") {
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if q.Get("mode") != "version" && q.Get("apikey") != f.key {
		http.Error(w, "API Key Incorrect", http.StatusForbidden)
		return
	}
	switch q.Get("mode") {
	case "version":
		writeJSON(w, map[string]any{"version": "5.1.3"})
	case "get_config":
		f.serveGetConfig(w, q)
	case "set_config":
		f.serveSetConfig(w, q)
	default:
		http.Error(w, "unexpected mode "+q.Get("mode"), http.StatusBadRequest)
	}
}

func (f *fakeSabnzbd) serveGetConfig(w http.ResponseWriter, q url.Values) {
	switch q.Get("section") {
	case "misc":
		key := q.Get("keyword")
		value, ok := f.misc[key]
		if !ok {
			writeJSON(w, map[string]any{"config": map[string]any{}})
			return
		}
		writeJSON(w, map[string]any{"config": map[string]any{"misc": map[string]any{key: value}}})
	case "categories":
		f.serveGetCategories(w, q.Get("keyword"))
	default:
		http.Error(w, "unexpected section", http.StatusBadRequest)
	}
}

func (f *fakeSabnzbd) serveGetCategories(w http.ResponseWriter, keyword string) {
	if keyword == "" {
		list := []any{}
		for name, dir := range f.cats {
			list = append(list, map[string]any{"name": name, "dir": dir})
		}
		writeJSON(w, map[string]any{"config": map[string]any{"categories": list}})
		return
	}
	key := strings.ToLower(keyword)
	dir, ok := f.cats[key]
	if !ok {
		writeJSON(w, map[string]any{"config": map[string]any{}})
		return
	}
	writeJSON(w, map[string]any{"config": map[string]any{"categories": []any{map[string]any{"name": key, "dir": dir}}}})
}

func (f *fakeSabnzbd) serveSetConfig(w http.ResponseWriter, q url.Values) {
	f.writes++
	f.lastSet = map[string]string{}
	for k, v := range q {
		if len(v) > 0 {
			f.lastSet[k] = v[0]
		}
	}
	if !f.sticky {
		switch q.Get("section") {
		case "misc":
			f.misc[q.Get("keyword")] = q.Get("value")
		case "categories":
			name := strings.ToLower(q.Get("name"))
			if name == "" {
				name = strings.ToLower(q.Get("keyword"))
			}
			f.cats[name] = q.Get("dir")
		}
	}
	writeJSON(w, map[string]any{"config": map[string]any{}})
}

func TestSabnzbdSealsTheDataContractAndProvesIt(t *testing.T) {
	f := newFakeSabnzbd()
	srv := httptest.NewServer(f)
	defer srv.Close()

	ctx := context.Background()
	s := NewSabnzbd(srv.URL, "/sabnzbd", f.key)
	if err := s.WaitReady(ctx, 0); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	want := Config{CompleteDir: "/data/downloads", DownloadDir: "/data/downloads/incomplete"}
	change, err := s.EnsureDataContract(ctx, want)
	if err != nil || change != Updated {
		t.Fatalf("ensure = %v, %v; want updated", change, err)
	}
	if f.misc["complete_dir"] != "/data/downloads" || f.misc["download_dir"] != "/data/downloads/incomplete" {
		t.Fatalf("paths not written: %v", f.misc)
	}
	writes := f.writes
	change, err = s.EnsureDataContract(ctx, want)
	if err != nil || change != Unchanged {
		t.Fatalf("second = %v, %v; want unchanged", change, err)
	}
	if f.writes != writes {
		t.Fatalf("the second run wrote: %d writes", f.writes-writes)
	}
}

// TestSabnzbdRefusesADataContractThatDoesNotStick: set_config answers 200 whether
// or not the value was accepted, which is why the read-back is the proof.
func TestSabnzbdRefusesADataContractThatDoesNotStick(t *testing.T) {
	f := newFakeSabnzbd()
	f.sticky = true
	srv := httptest.NewServer(f)
	defer srv.Close()

	s := NewSabnzbd(srv.URL, "/sabnzbd", f.key)
	_, err := s.EnsureDataContract(context.Background(), Config{CompleteDir: "/data/downloads", DownloadDir: "/data/downloads/incomplete"})
	if err == nil || !strings.Contains(err.Error(), "reports complete_dir") {
		t.Fatalf("err = %v, want a refusal naming the value the app reports", err)
	}
}

// TestSabnzbdCreatesACategoryWithItsDirectory is the silent failure this package
// exists for: an unknown category falls back to "*" with no directory, so a job
// unpacks into the root of complete_dir and the Arr's import still works.
func TestSabnzbdCreatesACategoryWithItsDirectory(t *testing.T) {
	f := newFakeSabnzbd()
	srv := httptest.NewServer(f)
	defer srv.Close()

	ctx := context.Background()
	s := NewSabnzbd(srv.URL, "/sabnzbd", f.key)
	// The 4K pair: the Arr's category keeps its spelling and its slash, and
	// SAB's own handler folds only the lookup key.
	change, err := s.EnsureCategory(ctx, "4K/Movies", "4K/Movies")
	if err != nil || change != Created {
		t.Fatalf("create = %v, %v; want created", change, err)
	}
	if got, ok := f.cats["4k/movies"]; !ok || got != "4K/Movies" {
		t.Fatalf("category stored as %q (present %v), want the directory 4K/Movies under a folded name", got, ok)
	}
	if f.lastSet["dir"] != "4K/Movies" || f.lastSet["pp"] != "3" || f.lastSet["script"] != "None" {
		t.Fatalf("the category was written with %v", f.lastSet)
	}
	change, err = s.EnsureCategory(ctx, "4K/Movies", "4K/Movies")
	if err != nil || change != Unchanged {
		t.Fatalf("second = %v, %v; want unchanged", change, err)
	}
	if f.writes != 1 {
		t.Fatalf("the second run wrote: %d writes", f.writes)
	}
	change, err = s.EnsureCategory(ctx, "4K/Movies", "/data/downloads/4K/Movies")
	if err != nil || change != Updated {
		t.Fatalf("repair = %v, %v; want updated", change, err)
	}
	if f.cats["4k/movies"] != "/data/downloads/4K/Movies" {
		t.Fatalf("dir not repaired: %q", f.cats["4k/movies"])
	}
}

func TestSabnzbdRefusesACategoryThatDoesNotStick(t *testing.T) {
	f := newFakeSabnzbd()
	f.sticky = true
	srv := httptest.NewServer(f)
	defer srv.Close()

	s := NewSabnzbd(srv.URL, "/sabnzbd", f.key)
	_, err := s.EnsureCategory(context.Background(), "tv", "tv")
	if err == nil || !strings.Contains(err.Error(), `reports dir ""`) {
		t.Fatalf("err = %v, want a refusal naming the category it could not read back", err)
	}
}

// TestSabnzbdNamesTheEdgeSettingsOnA403: the two traps the chart seals are the
// only things that answer 403 to every request through the Gateway, and the
// message has to name them rather than an HTTP status.
func TestSabnzbdNamesTheEdgeSettingsOnA403(t *testing.T) {
	f := newFakeSabnzbd()
	f.forbid = true
	srv := httptest.NewServer(f)
	defer srv.Close()

	err := NewSabnzbd(srv.URL, "/sabnzbd", f.key).WaitReady(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "host_whitelist or X-Forwarded-For") {
		t.Fatalf("err = %v, want the request refused and the settings named", err)
	}
}

// sabnzbdSchema is the shape Sonarr and Radarr return for their SABnzbd
// implementation: the same fields as qBittorrent's, plus the API key, minus the
// qBittorrent-only ones.
func sabnzbdSchema() []Item {
	var raw []any
	for _, f := range []Item{
		{"name": "host", "type": "textbox"},
		{"name": "port", "type": "textbox"},
		{"name": "useSsl", "type": "checkbox"},
		{"name": "urlBase", "type": "textbox"},
		{"name": "username", "type": "textbox"},
		{"name": "password", "type": "password", "privacy": "password"},
		{"name": "apiKey", "type": "textbox", "privacy": "password"},
		{"name": "tvCategory", "type": "textbox"},
		{"name": "movieCategory", "type": "textbox"},
		{"name": "musicCategory", "type": "textbox"},
	} {
		raw = append(raw, map[string]any(f))
	}
	return []Item{{"implementation": "Sabnzbd", "configContract": "SabnzbdSettings", "fields": raw}}
}

func sabnzbdClient(id int, host, category string, enabled bool) Item {
	fields := []Item{
		{"name": "host", "value": host},
		{"name": "port", "value": 8080},
		{"name": "useSsl", "value": false},
		{"name": "urlBase", "value": "/"},
		{"name": "username", "value": ""},
		{"name": "password", "value": ""},
		{"name": "apiKey", "value": "********"},
		{"name": "tvCategory", "value": category},
		{"name": "movieCategory", "value": category},
		{"name": "musicCategory", "value": category},
	}
	return Item{"id": id, "name": "SAB", "implementation": "Sabnzbd", "enable": enabled, "protocol": "nzb", "priority": 1, "tags": []any{}, "fields": fields}
}

func TestArrAdoptsAMigratedSabnzbdClientByItsHost(t *testing.T) {
	f := &fakeArr{urlBase: "/sonarr", schemas: append(qbitSchema(), sabnzbdSchema()...)}
	f.clients = []Item{sabnzbdClient(7, "sabnzbd", "tv", true)}
	srv := httptest.NewServer(f)
	defer srv.Close()

	a := NewArr(Sonarr, "sonarr", srv.URL, "/sonarr", "key")
	result, err := a.EnsureDownloadClient(context.Background(), DownloadClientSpec{
		Implementation: "Sabnzbd",
		Protocol:       "usenet",
		Name:           "SABnzbd",
		Category:       "tv",
		Host:           "sabnzbd",
		Port:           8080,
		URLBase:        "/sabnzbd",
		APIKey:         "key",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Change != Updated {
		t.Fatalf("change = %v, want updated (the adopted client's urlBase is \"/\")", result.Change)
	}
	if len(f.clients) != 1 || f.clients[0].Int("id") != 7 {
		t.Fatalf("the run created a second client: %v", f.clients)
	}
	if got, _ := f.clients[0].FieldValue("urlBase"); got != "/sabnzbd" {
		t.Fatalf("adopted client urlBase = %v, want the chart's prefix", got)
	}
}

func TestArrDoesNotAdoptASabnzbdClientAtAnotherAddress(t *testing.T) {
	f := &fakeArr{urlBase: "/sonarr", schemas: append(qbitSchema(), sabnzbdSchema()...)}
	f.clients = []Item{sabnzbdClient(7, "192.0.2.10", "tv", true)}
	srv := httptest.NewServer(f)
	defer srv.Close()

	a := NewArr(Sonarr, "sonarr", srv.URL, "/sonarr", "key")
	result, err := a.EnsureDownloadClient(context.Background(), DownloadClientSpec{
		Implementation: "Sabnzbd",
		Protocol:       "usenet",
		Name:           "SABnzbd",
		Category:       "tv",
		Host:           "sabnzbd",
		Port:           8080,
		URLBase:        "/sabnzbd",
		APIKey:         "key",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Change != Created {
		t.Fatalf("change = %v, want created", result.Change)
	}
	if len(f.clients) != 2 {
		t.Fatalf("want the box's own client beside the remote one, got %d", len(f.clients))
	}
}

func TestArrWritesTheSabnzbdClientFieldsTheDataContractNeeds(t *testing.T) {
	f := &fakeArr{urlBase: "/radarr", schemas: append(qbitSchema(), sabnzbdSchema()...)}
	srv := httptest.NewServer(f)
	defer srv.Close()

	a := NewArr(Radarr, "radarr", srv.URL, "/radarr", "key")
	_, err := a.EnsureDownloadClient(context.Background(), DownloadClientSpec{
		Implementation: "Sabnzbd",
		Protocol:       "usenet",
		Name:           "SABnzbd",
		Category:       "4K/Movies",
		Host:           "sabnzbd",
		Port:           8080,
		URLBase:        "/sabnzbd",
		APIKey:         "secret-key",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.clients) != 1 {
		t.Fatalf("want one client, got %d", len(f.clients))
	}
	c := f.clients[0]
	for name, want := range map[string]any{
		"host":          "sabnzbd",
		"port":          float64(8080),
		"urlBase":       "/sabnzbd",
		"apiKey":        "secret-key",
		"movieCategory": "4K/Movies",
	} {
		got, _ := c.FieldValue(name)
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if c.Str("implementation") != "Sabnzbd" || c.Str("protocol") != "usenet" {
		t.Fatalf("implementation/protocol = %q/%q", c.Str("implementation"), c.Str("protocol"))
	}
}
