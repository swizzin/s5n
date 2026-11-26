package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBazarr is a minimal Bazarr, with a write counter and a switch that makes the
// POST never answer while its live-sync connection comes up
// (bazarr/app/signalr_client.py at v1.6.2).
type fakeBazarr struct {
	mu       sync.Mutex
	urlBase  string
	key      string
	settings map[string]any
	posts    int
	form     map[string]string
	hang     bool
	ignore   bool
}

// fakeURLBase is the prefix every test serves the fake under: the chart's own.
const fakeURLBase = "/bazarr"

func newFakeBazarr(key string) *fakeBazarr {
	// One top-level key per section, the Arrs as objects
	// (bazarr/api/system/settings.py at v1.6.2).
	return &fakeBazarr{urlBase: fakeURLBase, key: key, settings: map[string]any{
		"general": map[string]any{"base_url": fakeURLBase, "use_sonarr": false, "use_radarr": false},
		"sonarr": map[string]any{
			"apikey": "", "base_url": "/", "ip": "127.0.0.1", "port": 8989, "ssl": false,
		},
		"radarr": map[string]any{
			"apikey": "", "base_url": "/", "ip": "127.0.0.1", "port": 7878, "ssl": false,
		},
	}}
}

func (f *fakeBazarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, f.urlBase)
	switch {
	case p == "/api/system/ping" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"status": "OK"})
	case p == "/api/system/settings" && r.Method == http.MethodGet:
		if r.Header.Get("X-API-KEY") != f.key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, f.settings)
	case p == "/api/system/settings" && r.Method == http.MethodPost:
		if r.Header.Get("X-API-KEY") != f.key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.posts++
		f.form = map[string]string{}
		for k := range r.Form {
			f.form[k] = r.Form.Get(k)
		}
		if !f.ignore {
			for k, v := range f.form {
				section, key, ok := strings.Cut(strings.TrimPrefix(k, "settings-"), "-")
				if !ok {
					continue
				}
				sec, _ := f.settings[section].(map[string]any)
				// save_settings casts integers then booleans
				// (bazarr/app/config.py:724-745), so GET answers types.
				if n, err := strconv.Atoi(v); err == nil {
					sec[key] = n
				} else if v == "true" || v == "false" {
					sec[key] = v == "true"
				} else {
					sec[key] = v
				}
			}
		}
		if f.hang {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *fakeBazarr) value(section, key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	sec, _ := f.settings[section].(map[string]any)
	return sec[key]
}

// One side's contract: written as the app's own form, it says what changed, and a
// second run writes nothing.
func TestBazarrWiresItsPairAndIsIdempotent(t *testing.T) {
	f := newFakeBazarr("bazarr-key")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "bazarr-key")
	ctx := context.Background()

	if err := b.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}

	spec := SubtitleSource{
		Kind: "sonarr", Enabled: true,
		Host: "sonarr", Port: 8989, URLBase: "/sonarr", APIKey: "sonarr-key",
	}
	res, err := b.EnsureSource(ctx, spec)
	if err != nil {
		t.Fatalf("ensure sonarr: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("change = %s, want updated", res.Change)
	}
	for section, want := range map[string]any{
		"general.use_sonarr": true,
		"sonarr.ip":          "sonarr",
		"sonarr.base_url":    "/sonarr",
		"sonarr.apikey":      "sonarr-key",
	} {
		got := f.value(strings.Split(section, ".")[0], strings.Split(section, ".")[1])
		if !SameValue(got, want) {
			t.Errorf("%s = %v, want %v", section, got, want)
		}
	}
	if got := f.value("sonarr", "port"); !SameValue(got, 8989) {
		t.Errorf("sonarr.port = %v, want 8989", got)
	}
	if got := f.value("sonarr", "ssl"); !SameValue(got, false) {
		t.Errorf("sonarr.ssl = %v, want false", got)
	}

	// A second run is a no-op: no POST at all, not a POST that changes nothing.
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	again, err := b.EnsureSource(ctx, spec)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again.Change != Unchanged {
		t.Fatalf("second run = %s, want unchanged", again.Change)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != posts {
		t.Fatalf("a second run made %d POSTs", f.posts-posts)
	}
}

// A value changed in the UI is put back, and the receipt names what it was.
func TestBazarrRestoresADriftedSide(t *testing.T) {
	f := newFakeBazarr("bazarr-key")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "bazarr-key")
	ctx := context.Background()
	spec := SubtitleSource{Kind: "radarr", Enabled: true, Host: "radarr", Port: 7878, URLBase: "/radarr", APIKey: "radarr-key"}
	if _, err := b.EnsureSource(ctx, spec); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.settings["radarr"].(map[string]any)["ip"] = "10.0.0.9"
	f.mu.Unlock()

	res, err := b.EnsureSource(ctx, spec)
	if err != nil {
		t.Fatalf("ensure after drift: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("change = %s, want updated", res.Change)
	}
	if got := f.value("radarr", "ip"); got != "radarr" {
		t.Fatalf("radarr.ip = %v, want it restored", got)
	}
	if len(res.Drift) != 1 || res.Drift[0].Field != "radarr.ip" || res.Drift[0].Old != "10.0.0.9" {
		t.Fatalf("drift = %+v, want the old address named", res.Drift)
	}
}

// A pair with one Arr turns the missing side's switch off and writes nothing else.
func TestBazarrTurnsOffASideItHasNoArrFor(t *testing.T) {
	f := newFakeBazarr("k")
	f.settings["general"].(map[string]any)["use_radarr"] = true
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "k")

	res, err := b.EnsureSource(context.Background(), SubtitleSource{Kind: "radarr", Enabled: false})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("change = %s, want updated", res.Change)
	}
	if got := f.value("general", "use_radarr"); !SameValue(got, false) {
		t.Fatalf("general.use_radarr = %v, want false", got)
	}
	for k := range f.form {
		if k != "settings-general-use_radarr" {
			t.Errorf("wrote %q for a side this box has no Arr for", k)
		}
	}
	if got := f.value("radarr", "ip"); got != "127.0.0.1" {
		t.Errorf("radarr.ip = %v, want it left alone", got)
	}
}

// The write happens before the app's sync loop, so a POST that never answers is
// still a saved setting: the run reads it back.
func TestBazarrAcceptsSettingsWhoseAnswerWasLost(t *testing.T) {
	f := newFakeBazarr("k")
	f.hang = true
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "k")
	b.postTimeout = 100 * time.Millisecond

	res, err := b.EnsureSource(context.Background(), SubtitleSource{
		Kind: "sonarr", Enabled: true, Host: "sonarr", Port: 8989, URLBase: "/sonarr", APIKey: "key",
	})
	if err != nil {
		t.Fatalf("ensure: %v, want the saved settings accepted", err)
	}
	if res.Change != Updated {
		t.Fatalf("change = %s, want updated", res.Change)
	}
	if res.Note == "" {
		t.Fatal("a lost answer must be explained in the receipt")
	}
}

// The app answered 204 but did not change: the run stops naming the field.
func TestBazarrRefusesSettingsThatDoNotStick(t *testing.T) {
	f := newFakeBazarr("k")
	f.ignore = true
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "k")

	_, err := b.EnsureSource(context.Background(), SubtitleSource{
		Kind: "sonarr", Enabled: true, Host: "sonarr", Port: 8989, URLBase: "/sonarr", APIKey: "key",
	})
	if err == nil {
		t.Fatal("want an error when the settings did not stick")
	}
	if !strings.Contains(err.Error(), "sonarr") {
		t.Fatalf("error does not name the side: %v", err)
	}
}

// A typo'd Kind would create settings the app never reads.
func TestBazarrRefusesAnUnknownSide(t *testing.T) {
	f := newFakeBazarr("k")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "k")
	if _, err := b.EnsureSource(context.Background(), SubtitleSource{Kind: "lidarr", Enabled: true}); err == nil {
		t.Fatal("want a refusal for a side this app does not have")
	}
}

// The key goes in a header and the form, never a URL where an error would print it.
func TestBazarrNeverLogsOrWritesItsKeyIntoAPath(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	b := NewBazarr(srv.URL, "/bazarr", "super-secret")
	_, _ = b.Settings(context.Background())
	for _, u := range seen {
		if strings.Contains(u, "super-secret") {
			t.Fatalf("the API key appears in a request path: %s", u)
		}
	}
}
