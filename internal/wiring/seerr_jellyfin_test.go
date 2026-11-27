package wiring

// A Jellyfin-mode Seerr is the one Seerr the Job sets up itself: its owner is the
// person's own Jellyfin account, whose password is in the release Secret.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// fakeSeerr is Seerr's setup API at v3.5.2: the public settings, the Jellyfin sign-in
// with the session it sets, the library sync and its per-library switch, the settings
// write and initialize. The settings calls need the session, as the app's do.
type fakeSeerr struct {
	mu          sync.Mutex
	initialized bool
	person      string
	password    string
	refusal     int    // the status the sign-in answers, 0 for success
	code        string // the message in that refusal
	libs        []apps.SeerrLibrary

	// mediaServer is Seerr's settings.main.mediaServerType (4 is NOT_CONFIGURED, 0 here
	// too); a successful sign-in records the server as Seerr's does.
	mediaServer int
	serverIP    string
	failSyncs   int // library syncs that answer 500 before one succeeds

	hostname    string
	port        int
	serverType  int
	signIns     int
	writes      int
	initialize  int
	hostnameSet string
	cm          map[string]string
}

func (f *fakeSeerr) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/settings/public":
			_ = json.NewEncoder(w).Encode(map[string]any{"initialized": f.initialized, "mediaServerType": f.mediaServer})
		case r.URL.Path == "/api/v1/auth/jellyfin":
			f.signIn(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/v1/settings/jellyfin"),
			r.URL.Path == "/api/v1/settings/initialize":
			f.settings(w, r)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// signIn is the Jellyfin sign-in: the account it was given, the refusal a test configured,
// and the session the answer sets.
func (f *fakeSeerr) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Hostname   string `json:"hostname"`
		Port       int    `json:"port"`
		ServerType int    `json:"serverType"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.signIns++
	f.hostname = body.Hostname
	f.port = body.Port
	f.serverType = body.ServerType
	// server/routes/auth.ts:270-289 at v3.5.2: a Seerr set up for another server refuses the
	// route, and one whose Jellyfin address is recorded refuses a second address.
	if f.mediaServer != 0 && f.mediaServer != 4 && f.mediaServer != 2 {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Jellyfin login is disabled"}`))
		return
	}
	if f.serverIP != "" && body.Hostname != "" {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Jellyfin hostname already configured"}`))
		return
	}
	if f.refusal != 0 {
		w.WriteHeader(f.refusal)
		_, _ = w.Write([]byte(`{"message":"` + f.code + `"}`))
		return
	}
	if body.Username != f.person || body.Password != f.password {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"INVALID_CREDENTIALS"}`))
		return
	}
	if f.serverIP == "" {
		f.serverIP = body.Hostname
		f.mediaServer = 2
	}
	http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "s%3Asession", Path: "/"})
	f.writes++
	_, _ = w.Write([]byte(`{"id":1,"jellyfinUsername":"` + f.person + `"}`))
}

// settings is the four settings endpoints the setup uses, each behind the session.
func (f *fakeSeerr) settings(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	switch r.URL.Path {
	case "/api/v1/settings/jellyfin/library/sync":
		if f.failSyncs > 0 {
			f.failSyncs--
			http.Error(w, `{"message":"sync failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(f.libs)
	case "/api/v1/settings/jellyfin":
		var body struct {
			ExternalHostname string `json:"externalHostname"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.hostnameSet = body.ExternalHostname
		f.writes++
		_, _ = w.Write([]byte(`{}`))
	case "/api/v1/settings/initialize":
		f.initialized = true
		f.initialize++
		f.writes++
		_, _ = w.Write([]byte(`{"initialized":true}`))
	default:
		if !strings.HasPrefix(r.URL.Path, "/api/v1/settings/jellyfin/library/") || r.Method != http.MethodPut {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		f.enableLibrary(w, r)
	}
}

// enableLibrary switches one library on or off, by the server's own key.
func (f *fakeSeerr) enableLibrary(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/settings/jellyfin/library/")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	for i := range f.libs {
		if f.libs[i].ID == id {
			f.libs[i].Enabled = body.Enabled
		}
	}
	f.writes++
	_, _ = w.Write([]byte(`{}`))
}

// authed is Seerr's own rule for a settings endpoint: no session, no answer.
func (f *fakeSeerr) authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Cookie") != "connect.sid=s%3Asession" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You do not have permission to access this endpoint"}`))
		return false
	}
	return true
}

// seerrPersonPassword is what the person's Jellyfin account holds, as the release
// Secret does.
const seerrPersonPassword = "person-secret-4b1d"

const seerrJellyfinURL = "https://jellyfin.example.test"

// newSeerrHarness wires a Jellyfin-mode Seerr and a Jellyfin box cannot sign in to: the
// Job's Seerr setup does not depend on that sign-in, so it is the plainest server that
// lets the Seerr steps be asserted alone.
func newSeerrHarness(t *testing.T) (*Runner, *fakeSeerr) {
	t.Helper()
	f := &fakeSeerr{
		person:   "alice",
		password: seerrPersonPassword,
		libs: []apps.SeerrLibrary{
			{ID: "a1", Name: "movies"},
			{ID: "b2", Name: "tv"},
		},
	}
	seerr := httptest.NewServer(f.handler())
	jf := &jellyfinNoAccount{}
	jellyfin := httptest.NewServer(jf.handler())
	t.Cleanup(func() { seerr.Close(); jellyfin.Close() })

	dir := t.TempDir()
	userPassword := dir + "/jellyfin-user-password"
	boxPassword := dir + "/jellyfin-box-password"
	for _, p := range []struct{ path, value string }{
		{userPassword, seerrPersonPassword + "\n"},
		{boxPassword, "box-secret\n"},
	} {
		if err := os.WriteFile(p.path, []byte(p.value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		TimeoutSeconds: 10,
		Seerr: &SeerrConfig{
			URL:            seerr.URL,
			PublicURL:      mediaSeerrPublicURL,
			MediaServer:    mediaServerJellyfin,
			MediaServerURL: seerrJellyfinURL,
		},
		Jellyfin: &JellyfinConfig{
			URL:              jellyfin.URL,
			Username:         "alice",
			UserPasswordFile: userPassword,
			BoxPasswordFile:  boxPassword,
			Libraries: []JellyfinLibrary{
				{Name: "movies", CollectionType: "movies", Path: "/data/media/movies"},
				{Name: "tv", CollectionType: "tvshows", Path: "/data/media/tv"},
			},
		},
	}
	runner := &Runner{
		Config: cfg,
		Publish: func(_ context.Context, data map[string]string) error {
			if f.cm == nil {
				f.cm = map[string]string{}
			}
			for k, v := range data {
				f.cm[k] = v
			}
			return nil
		},
		LoadOwned: func(context.Context) ([]byte, error) {
			v, ok := f.cm["owned.json"]
			if !ok {
				return nil, nil
			}
			return []byte(v), nil
		},
	}
	return runner, f
}

// The fresh instance: the Job signs the person's own server account in, which is what
// makes it Seerr's owner, switches on the libraries this release made, stores the
// address people reach the server at, and initializes -- the setup page's own order.
func TestWiringSeerrJellyfinSetsUpAFreshInstance(t *testing.T) {
	r, f := newSeerrHarness(t)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if f.signIns != 1 {
		t.Fatalf("sign-ins = %d, want one", f.signIns)
	}
	// The address is the Service the Job dials, not the host people open: Seerr stores
	// it and reaches the server itself for every later scan.
	wantHost, wantPort, err := hostPort(r.Config.Jellyfin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f.hostname != wantHost || f.port != wantPort {
		t.Fatalf("the sign-in named %s:%d, want the Jellyfin Service %s:%d", f.hostname, f.port, wantHost, wantPort)
	}
	if f.serverType != 2 {
		t.Fatalf("serverType = %d, want 2 (Seerr's Jellyfin)", f.serverType)
	}
	if !f.initialized || f.initialize != 1 {
		t.Fatalf("initialized = %v after %d calls, want it set once", f.initialized, f.initialize)
	}
	if f.hostnameSet != seerrJellyfinURL {
		t.Fatalf("external hostname = %q, want %q", f.hostnameSet, seerrJellyfinURL)
	}
	for _, lib := range f.libs {
		if !lib.Enabled {
			t.Fatalf("library %q is off after the setup: the request page would show nothing for it", lib.Name)
		}
	}
	for _, name := range []string{"seerr/login", "seerr/libraries", "seerr/media-server"} {
		s := step(res, name)
		if s == nil || s.Result != "ok" {
			t.Fatalf("%s = %+v, want ok\n%s", name, s, Summary(res))
		}
	}
	if s := step(res, "seerr/initialize"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("seerr/initialize = %+v, want ok/created", s)
	}
	if s := step(res, "seerr/login"); s.Detail != "alice" {
		t.Fatalf("seerr/login detail = %q, want the account it signed in as", s.Detail)
	}
	// The owner is on record: a later run whose Seerr has lost its flag is not set up
	// a second time.
	rec, err := ParseOwnedRecord([]byte(f.cm["owned.json"]))
	if err != nil {
		t.Fatalf("owned.json: %v", err)
	}
	if !rec.SeerrInitialized() {
		t.Fatal("the run did not record that it initialized Seerr")
	}
	if strings.Contains(f.cm["owned.json"], seerrPersonPassword) {
		t.Fatal("owned.json carries the person's password")
	}
}

// A Seerr that is already initialized is the person's: a migrated one brought its
// settings and an admin, and redoing the setup would overwrite them.
func TestWiringSeerrJellyfinLeavesAnInitializedInstanceAlone(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.initialized = true
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if f.signIns != 0 || f.writes != 0 {
		t.Fatalf("an initialized Seerr was set up again (%d sign-ins, %d writes)", f.signIns, f.writes)
	}
	s := step(res, "seerr/setup")
	if s == nil || s.Result != "ok" || !strings.Contains(s.Detail, "already initialized") {
		t.Fatalf("seerr/setup = %+v, want the row that says it was left as it is", s)
	}
}

// The record outranks the flag: a person who reset a Seerr this Job set up, or deleted
// the admin it made, is not given a second setup written under them.
func TestWiringSeerrJellyfinIsNotSetUpTwice(t *testing.T) {
	r, f := newSeerrHarness(t)
	ctx := context.Background()
	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The settings Seerr published are gone; the record in the wiring ConfigMap is not.
	f.initialized = false
	signIns, writes := f.signIns, f.writes
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(res))
	}
	if f.signIns != signIns || f.writes != writes {
		t.Fatalf("the second run set Seerr up again (%d sign-ins, %d writes)", f.signIns-signIns, f.writes-writes)
	}
	s := step(res, "seerr/setup")
	if s == nil || s.Result != stepSkipped {
		t.Fatalf("seerr/setup = %+v, want skipped", s)
	}
	if !strings.Contains(s.Detail, "earlier run") {
		t.Fatalf("seerr/setup detail = %q, want it to name the earlier run", s.Detail)
	}
}

// A password changed in Jellyfin is the person's to settle: the run reports the account
// and stops at that row, and nothing is half-configured.
func TestWiringSeerrJellyfinWithAChangedPasswordIsPending(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.refusal = http.StatusUnauthorized
	f.code = "INVALID_CREDENTIALS"
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a refused account failed the Job: %v\n%s", err, Summary(res))
	}
	if res.Status != "ok" {
		t.Fatalf("receipt status = %q, want ok\n%s", res.Status, Summary(res))
	}
	s := step(res, "seerr/login")
	if s == nil || s.Result != stepPending {
		t.Fatalf("seerr/login = %+v, want pending", s)
	}
	for _, want := range []string{"alice", "jellyfin-user-password"} {
		if !strings.Contains(s.Detail, want) {
			t.Errorf("the reason does not name %q: %s", want, s.Detail)
		}
	}
	if strings.Contains(s.Detail, seerrPersonPassword) {
		t.Fatalf("the reason prints the password: %s", s.Detail)
	}
	for _, name := range []string{"seerr/libraries", "seerr/media-server", "seerr/initialize"} {
		if got := step(res, name); got != nil {
			t.Fatalf("%s = %+v: the setup ran on past a refused account", name, got)
		}
	}
	if f.writes != 0 {
		t.Fatalf("Seerr was written %d time(s) with an account it refused", f.writes)
	}
	if rec, err := ParseOwnedRecord([]byte(f.cm["owned.json"])); err == nil && rec.SeerrInitialized() {
		t.Fatal("a refused setup was recorded as initialized")
	}
}

// An account that is not the server's administrator cannot own a Seerr, and Seerr says
// so with its own code: the row says what to change in Jellyfin.
func TestWiringSeerrJellyfinWithANonAdminAccountIsPending(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.refusal = http.StatusForbidden
	f.code = "NOT_ADMIN"
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a non-admin account failed the Job: %v\n%s", err, Summary(res))
	}
	s := step(res, "seerr/login")
	if s == nil || s.Result != stepPending {
		t.Fatalf("seerr/login = %+v, want pending", s)
	}
	if !strings.Contains(s.Detail, "administrator") {
		t.Fatalf("seerr/login detail = %q, want it to name the administrator it needs", s.Detail)
	}
}

// A server with libraries this release does not make keeps them off: enabling them is
// the person's choice, and the row says which were left.
func TestWiringSeerrJellyfinLeavesAForeignLibraryOff(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.libs = append(f.libs, apps.SeerrLibrary{ID: "c3", Name: "home videos"})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	for _, lib := range f.libs {
		want := lib.Name != "home videos"
		if lib.Enabled != want {
			t.Fatalf("library %q enabled = %v, want %v", lib.Name, lib.Enabled, want)
		}
	}
	s := step(res, "seerr/libraries")
	if s == nil || !strings.Contains(s.Detail, "left off: home videos") {
		t.Fatalf("seerr/libraries = %+v, want the library it left named", s)
	}
}

// With none of the release's libraries on the server there is nothing to enable, and a
// Seerr nobody can request anything from is a failed setup, not a quiet one.
func TestWiringSeerrJellyfinFailsWhenNoLibraryMatches(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.libs = []apps.SeerrLibrary{{ID: "c3", Name: "home videos"}}
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with no library enabled\n%s", Summary(res))
	}
	s := step(res, "seerr/libraries")
	if s == nil || s.Result != "failed" {
		t.Fatalf("seerr/libraries = %+v, want failed", s)
	}
	for _, want := range []string{"home videos", "movies, tv"} {
		if !strings.Contains(s.Detail, want) {
			t.Errorf("the refusal does not name %q: %s", want, s.Detail)
		}
	}
	if f.initialize != 0 {
		t.Fatal("Seerr was initialized with no library switched on")
	}
}

// Plex mode is the shape it has always been: the same one row asking for the person's
// own sign-in, and no settings call, because the Job has no session to make one with.
func TestWiringSeerrPlexModeIsUnchanged(t *testing.T) {
	r, f := newSeerrHarness(t)
	r.Config.Seerr.MediaServer = mediaServerPlex
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	s := step(res, "seerr/plex")
	if s == nil || s.Result != stepPending {
		t.Fatalf("seerr/plex = %+v, want pending\n%s", s, Summary(res))
	}
	const want = "Seerr has no admin account yet: sign in once at " + mediaSeerrPublicURL + " with Plex to let the wiring Job take over"
	if s.Detail != want {
		t.Fatalf("seerr/plex detail = %q, want %q", s.Detail, want)
	}
	if f.signIns != 0 || f.writes != 0 {
		t.Fatalf("Plex mode signed in or wrote (%d sign-ins, %d writes)", f.signIns, f.writes)
	}
	if s := step(res, "seerr/setup"); s != nil {
		t.Fatalf("seerr/setup = %+v, want no row in Plex mode", s)
	}
}

// The Job refuses a Jellyfin-mode Seerr it cannot set up, before it touches anything.
func TestSeerrJellyfinConfigNeedsTheServerAndItsAddress(t *testing.T) {
	base := Config{Seerr: &SeerrConfig{
		URL: "http://seerr:5055", PublicURL: "seerr.example.test",
		MediaServer: mediaServerJellyfin, MediaServerURL: seerrJellyfinURL,
	}}
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "jellyfin is not deployed") {
		t.Fatalf("a Jellyfin-mode Seerr with no Jellyfin was accepted: %v", err)
	}
	base.Jellyfin = &JellyfinConfig{
		URL: "http://jellyfin:8096", Username: "alice",
		UserPasswordFile: "/u", BoxPasswordFile: "/b",
	}
	base.Seerr.MediaServerURL = ""
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "mediaServerUrl") {
		t.Fatalf("a Jellyfin-mode Seerr with no server address was accepted: %v", err)
	}
	base.Seerr.MediaServer = "emby"
	base.Seerr.MediaServerURL = seerrJellyfinURL
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "plex or jellyfin") {
		t.Fatalf("a Seerr against a server this chart does not run was accepted: %v", err)
	}
}

// An entry written before the media server was named is a Plex one: that is the only
// setup an older release could have had.
func TestAnOlderSeerrConfigLoadsAsPlex(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wiring.json"
	body := `{"namespace":"s5n","configMap":"s5n-wiring","instances":[],` +
		`"seerr":{"url":"http://seerr:5055","publicUrl":"seerr.example.test"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Seerr == nil || c.Seerr.MediaServer != mediaServerPlex {
		t.Fatalf("seerr = %+v, want a Plex one", c.Seerr)
	}
}

// A refusal that is not about the account is the release's: a Seerr that cannot be
// reached after its readiness is a failed step, never a pending row.
func TestWiringSeerrJellyfinUnreachableSettingsFails(t *testing.T) {
	r, f := newSeerrHarness(t)
	f.refusal = http.StatusInternalServerError
	f.code = "UNKNOWN"
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded past a 500\n%s", Summary(res))
	}
	s := step(res, "seerr/login")
	if s == nil || s.Result != "failed" {
		t.Fatalf("seerr/login = %+v, want failed", s)
	}
	var refused *apps.SeerrSignInRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("a 500 was reported as an account refusal: %v", err)
	}
}

// A run that signs in and then fails leaves Seerr with its owner and its Jellyfin address
// but not initialized. The next run signs in again without an address: Seerr refuses a
// second one, and a refusal on every run would be a box that never finishes.
func TestWiringSeerrJellyfinFinishesASetupAnEarlierRunLeftHalfDone(t *testing.T) {
	runner, f := newSeerrHarness(t)
	f.failSyncs = 1
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("the first run's failed library sync was not an error")
	}
	if f.initialized || f.serverIP == "" {
		t.Fatalf("after the first run initialized=%v server=%q, want a half-done setup", f.initialized, f.serverIP)
	}
	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("the second run: %v (steps %+v)", err, res.Steps)
	}
	if !f.initialized {
		t.Fatal("the second run did not finish the setup")
	}
	if f.hostname != "" || f.port != 0 {
		t.Fatalf("the second sign-in sent hostname=%q port=%d, want none: Seerr already has the server", f.hostname, f.port)
	}
}

// A Seerr someone pointed at Plex by hand, with nobody signed in yet, is theirs: the Job
// says so instead of failing on Seerr's "Jellyfin login is disabled".
func TestWiringSeerrJellyfinOnASeerrSetUpForPlexIsPending(t *testing.T) {
	runner, f := newSeerrHarness(t)
	f.mediaServer = 1
	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.signIns != 0 {
		t.Fatalf("the Job tried %d sign-ins on a Seerr set up for Plex", f.signIns)
	}
	var found bool
	for _, st := range res.Steps {
		if strings.HasSuffix(st.Name, "/setup") && st.Result == stepPending && strings.Contains(st.Detail, "Plex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no pending setup row naming Plex in %+v", res.Steps)
	}
}
