package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// jellyfinFake is the smallest server that behaves like the parts of v12.4 the
// Job uses: the public info, the wizard (anonymous until complete), sign-in, the
// user table with policies, the key list and the libraries. It records what was
// written so a test can assert the Job wrote once and only when it had to.
type jellyfinFake struct {
	mu               sync.Mutex
	wizardDone       bool
	firstUser        string
	firstUserPw      string
	users            map[string]map[string]any // name -> id, policy
	passwords        map[string]string         // name -> password
	keys             []APIKey
	libs             []Library
	network          map[string]any
	writes           []string
	tokens           map[string]string // token -> user name
	sessions         int
	libraryAdds      int
	policyWrites     int
	passwordWrites   int
	networkWrites    int
	startupUserCalls int
	defaultUsers     int
}

func newJellyfinFake(t *testing.T) (*jellyfinFake, *Jellyfin) {
	t.Helper()
	f := &jellyfinFake{
		users:     map[string]map[string]any{},
		passwords: map[string]string{},
		network:   map[string]any{"BaseUrl": "", "KnownProxies": []any{}, "EnableRemoteAccess": true},
		tokens:    map[string]string{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, NewJellyfin(srv.URL)
}

func (f *jellyfinFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/System/Info/Public":
		f.info(w)
	case strings.HasPrefix(r.URL.Path, "/Startup/"):
		f.startup(w, r)
	case strings.HasPrefix(r.URL.Path, "/Users"):
		f.userRoutes(w, r)
	case r.URL.Path == "/Auth/Keys":
		f.apiKeys(w, r)
	case strings.HasPrefix(r.URL.Path, "/Library/"):
		f.library(w, r)
	case strings.HasPrefix(r.URL.Path, "/System/Configuration/"):
		f.networkConfig(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *jellyfinFake) info(w http.ResponseWriter) {
	writeJSON(w, map[string]any{
		"ServerName":             "s5n",
		"Version":                "12.4.0",
		"StartupWizardCompleted": f.wizardDone,
	})
}

// startup is the wizard: anonymous until it is complete, and /Startup/User
// refuses once the first account has a password -- the app's own rule, which is
// what makes a re-run safe.
func (f *jellyfinFake) startup(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/Startup/User" && r.Method == http.MethodPost:
		f.startupUserCalls++
		if f.firstUser == "" {
			// No user exists yet: Jellyfin creates its default account only from
			// inside the wizard's own read, and UpdateStartupUser refuses while
			// there is nothing to rename.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.wizardDone || f.firstUserPw != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body struct{ Name, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		delete(f.users, f.firstUser)
		delete(f.passwords, f.firstUser)
		f.firstUser, f.firstUserPw = body.Name, body.Password
		f.users[body.Name] = map[string]any{"Id": "id-" + body.Name, "Name": body.Name,
			"Policy": map[string]any{"IsAdministrator": true, "IsDisabled": false}}
		f.passwords[body.Name] = body.Password
		f.writes = append(f.writes, "startup/user")
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/Startup/User":
		if f.firstUser == "" {
			// UserManager.InitializeAsync: the app's own default account.
			f.defaultUsers++
			f.firstUser = "default-user"
			f.users[f.firstUser] = map[string]any{"Id": "id-" + f.firstUser, "Name": f.firstUser,
				"Policy": map[string]any{"IsAdministrator": true, "IsDisabled": false}}
		}
		writeJSON(w, map[string]any{"Name": f.firstUser})
	case r.URL.Path == "/Startup/RemoteAccess":
		var body struct{ EnableRemoteAccess bool }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.network["EnableRemoteAccess"] = body.EnableRemoteAccess
		f.writes = append(f.writes, "startup/remoteaccess")
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/Startup/Complete":
		f.wizardDone = true
		f.writes = append(f.writes, "startup/complete")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *jellyfinFake) userRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/Users/AuthenticateByName" {
		f.signIn(w, r)
		return
	}
	if !f.auth(w, r) {
		return
	}
	if r.URL.Path == "/Users" && r.Method == http.MethodGet {
		out := make([]map[string]any, 0, len(f.users))
		for _, u := range f.users {
			out = append(out, u)
		}
		writeJSON(w, out)
		return
	}
	if r.URL.Path == "/Users/New" && r.Method == http.MethodPost {
		f.createUser(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/Users/")
	switch {
	case strings.HasSuffix(id, "/Policy"):
		f.setPolicy(w, r, strings.TrimSuffix(id, "/Policy"))
	case strings.HasSuffix(id, "/Password"):
		f.setPassword(w, r, strings.TrimSuffix(id, "/Password"))
	case r.Method == http.MethodGet:
		f.getUser(w, id)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *jellyfinFake) signIn(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Pw string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if f.passwords[body.Username] != body.Pw || body.Pw == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.sessions++
	token := "session-" + body.Username
	f.tokens[token] = body.Username
	writeJSON(w, map[string]any{"AccessToken": token, "User": map[string]any{"Name": body.Username}})
}

func (f *jellyfinFake) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Password string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if _, ok := f.users[body.Name]; ok {
		w.WriteHeader(http.StatusConflict)
		return
	}
	u := map[string]any{"Id": "id-" + body.Name, "Name": body.Name,
		"Policy": map[string]any{"IsAdministrator": false, "IsDisabled": false}}
	f.users[body.Name] = u
	f.passwords[body.Name] = body.Password
	f.writes = append(f.writes, "users/new")
	writeJSON(w, u)
}

func (f *jellyfinFake) setPolicy(w http.ResponseWriter, r *http.Request, id string) {
	var policy map[string]any
	_ = json.NewDecoder(r.Body).Decode(&policy)
	for _, u := range f.users {
		if u["Id"] == id {
			u["Policy"] = policy
		}
	}
	f.policyWrites++
	f.writes = append(f.writes, "users/policy")
	w.WriteHeader(http.StatusNoContent)
}

func (f *jellyfinFake) setPassword(w http.ResponseWriter, r *http.Request, id string) {
	var body struct{ CurrentPw, NewPw string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	for name, u := range f.users {
		if u["Id"] == id {
			f.passwords[name] = body.NewPw
		}
	}
	f.passwordWrites++
	f.writes = append(f.writes, "users/password")
	w.WriteHeader(http.StatusNoContent)
}

func (f *jellyfinFake) getUser(w http.ResponseWriter, id string) {
	for _, u := range f.users {
		if u["Id"] == id {
			writeJSON(w, u)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *jellyfinFake) apiKeys(w http.ResponseWriter, r *http.Request) {
	if !f.auth(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"Items": f.keys, "TotalRecordCount": len(f.keys)})
		return
	}
	app := r.URL.Query().Get("app")
	f.keys = append(f.keys, APIKey{AppName: app, AccessToken: "key-" + app})
	w.WriteHeader(http.StatusNoContent)
}

func (f *jellyfinFake) library(w http.ResponseWriter, r *http.Request) {
	if !f.auth(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, f.libs)
		return
	}
	q := r.URL.Query()
	f.libraryAdds++
	f.libs = append(f.libs, Library{Name: q.Get("name"), CollectionType: q.Get("collectionType"),
		Locations: []string{q.Get("paths")}})
	w.WriteHeader(http.StatusNoContent)
}

func (f *jellyfinFake) networkConfig(w http.ResponseWriter, r *http.Request) {
	if !f.auth(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, f.network)
		return
	}
	var doc map[string]any
	_ = json.NewDecoder(r.Body).Decode(&doc)
	f.network = doc
	f.networkWrites++
	f.writes = append(f.writes, "network")
	w.WriteHeader(http.StatusNoContent)
}

// auth checks the token the client sent, so a test that forgets to sign in
// fails the way the real app would answer: 401, not a silent empty list.
func (f *jellyfinFake) auth(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("X-Emby-Token")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	if _, ok := f.tokens[token]; ok {
		return true
	}
	for _, k := range f.keys {
		if k.AccessToken == token {
			return true
		}
	}
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

// TestJellyfinWizardIsIdempotent: the wizard calls are the only writes a second
// run must not repeat, and the app's own answer is what tells the run so --
// /Startup/User answers 403 once the first user has a password, which the Job
// reads as "the account is there" rather than as a failure.
func TestJellyfinWizardIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)

	if err := wizardUser(ctx, j); err != nil {
		t.Fatalf("first wizard run: %v", err)
	}
	if err := j.AllowRemoteAccess(ctx); err != nil {
		t.Fatalf("remote access: %v", err)
	}
	if err := j.CompleteWizard(ctx); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !f.wizardDone || f.firstUser != "alice" {
		t.Fatalf("wizard state: done=%v user=%q", f.wizardDone, f.firstUser)
	}

	started, err := j.SetFirstUser(ctx, "attacker", "attacker-pw")
	if err != nil {
		t.Fatalf("second wizard run: %v", err)
	}
	if !started {
		t.Fatal("a second SetFirstUser after the wizard must report already started, not success")
	}
	if f.firstUser != "alice" || f.passwords["attacker"] != "" {
		t.Fatalf("the first user was renamed or a second one created: %q", f.firstUser)
	}
}

// TestJellyfinInfoReportsTheWizard: the readiness probe and the wizard gate are
// the same call, so a Job that waits for readiness already knows whether the
// wizard is done.
func TestJellyfinInfoReportsTheWizard(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	info, err := j.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.StartupWizardCompleted {
		t.Fatal("a fresh server reports the wizard complete")
	}
	f.wizardDone = true
	info, err = j.Info(ctx)
	if err != nil || !info.StartupWizardCompleted {
		t.Fatalf("after the wizard: %+v %v", info, err)
	}
	if info.Version != "12.4.0" {
		t.Fatalf("version %q", info.Version)
	}
}

// TestJellyfinAdministratorIsWrittenOnce: POST /Users/{id}/Policy replaces the
// whole policy, so the Job reads it first and sets one field; an account that is
// already an administrator is not written again.
func TestJellyfinAdministratorIsWrittenOnce(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	if err := wizardUser(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteWizard(ctx); err != nil {
		t.Fatal(err)
	}
	token, err := j.AuthenticateByName(ctx, "alice", "person-pw")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	j.SetToken(token)

	user, err := j.CreateUser(ctx, "box", "box-pw")
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	change, err := j.SetAdministrator(ctx, user.ID)
	if err != nil {
		t.Fatalf("make admin: %v", err)
	}
	if change != Updated {
		t.Fatalf("first SetAdministrator = %v, want updated", change)
	}
	f.mu.Lock()
	policy := f.users["box"]["Policy"].(map[string]any)
	f.mu.Unlock()
	if admin, _ := policy["IsAdministrator"].(bool); !admin {
		t.Fatal("box is not an administrator")
	}
	change, err = j.SetAdministrator(ctx, user.ID)
	if err != nil {
		t.Fatalf("second make admin: %v", err)
	}
	if change != Unchanged {
		t.Fatalf("a second run rewrote the policy: %v", change)
	}
	if f.policyWrites != 1 {
		t.Fatalf("policy written %d times, want 1", f.policyWrites)
	}
	// The policy the Job wrote back is the app's own, not a template: a field
	// it did not touch is still there.
	if _, ok := policy["IsDisabled"]; !ok {
		t.Fatalf("the policy lost the fields the app sent: %v", policy)
	}
}

// TestJellyfinAPIKeyIsReused: the key is re-read rather than minted again, so a
// second run leaves the app's key table exactly as it was.
func TestJellyfinAPIKeyIsReused(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	if err := wizardUser(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteWizard(ctx); err != nil {
		t.Fatal(err)
	}
	token, _ := j.AuthenticateByName(ctx, "alice", "person-pw")
	j.SetToken(token)

	key, change, err := j.EnsureAPIKey(ctx, "box")
	if err != nil {
		t.Fatal(err)
	}
	if change != Created || key != "key-box" {
		t.Fatalf("mint: %q %v", key, change)
	}
	key, change, err = j.EnsureAPIKey(ctx, "box")
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged || key != "key-box" {
		t.Fatalf("reuse: %q %v", key, change)
	}
	f.mu.Lock()
	n := len(f.keys)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d keys, want 1", n)
	}
}

// TestJellyfinLibraryAdoptsByPath: an existing library holding the same
// directory is adopted, whatever it is called, and one with the same name but
// another path is not: two libraries over one directory is what this prevents.
func TestJellyfinLibraryAdoptsByPath(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	if err := wizardUser(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteWizard(ctx); err != nil {
		t.Fatal(err)
	}
	token, _ := j.AuthenticateByName(ctx, "alice", "person-pw")
	j.SetToken(token)

	f.libs = []Library{{Name: "Films", Locations: []string{"/data/media/movies/"}, CollectionType: "movies"}}
	change, err := j.EnsureLibrary(ctx, "radarr", "movies", "/data/media/movies")
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged {
		t.Fatalf("existing library over the same path: %v", change)
	}
	f.libs = append(f.libs, Library{Name: "radarr", Locations: []string{"/data/media/movies-4k"}, CollectionType: "movies"})
	change, err = j.EnsureLibrary(ctx, "radarr", "movies", "/data/media/movies-4k")
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged {
		t.Fatalf("same name, different path: %v", change)
	}
	change, err = j.EnsureLibrary(ctx, "sonarr", "tvshows", "/data/media/tv")
	if err != nil {
		t.Fatal(err)
	}
	if change != Created {
		t.Fatalf("new library: %v", change)
	}
	if f.libraryAdds != 1 {
		t.Fatalf("%d libraries created, want 1", f.libraryAdds)
	}
}

// TestJellyfinKnownProxiesAreWrittenOnce: with the list empty Jellyfin ignores
// X-Forwarded-For entirely, and through the Gateway every request then comes from
// a local-looking address. The Job writes the Gateway's peers and, on a second
// run, writes nothing -- and it sends the app's whole network document back, so a
// setting it does not own survives.
func TestJellyfinKnownProxiesAreWrittenOnce(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	if err := wizardUser(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := j.CompleteWizard(ctx); err != nil {
		t.Fatal(err)
	}
	token, _ := j.AuthenticateByName(ctx, "alice", "person-pw")
	j.SetToken(token)
	f.network["BaseUrl"] = "/jellyfin"

	change, obs, err := j.EnsureKnownProxies(ctx, []string{"10.244.0.0/16", "10.96.0.5/32"})
	if err != nil {
		t.Fatal(err)
	}
	if change != Updated {
		t.Fatalf("first write: %v", change)
	}
	if len(obs.Before) != 0 || len(obs.After) != 2 {
		t.Fatalf("observation %+v", obs)
	}
	change, _, err = j.EnsureKnownProxies(ctx, []string{"10.96.0.5/32", "10.244.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged {
		t.Fatalf("the same set in another order was written again: %v", change)
	}
	if f.networkWrites != 1 {
		t.Fatalf("%d network writes, want 1", f.networkWrites)
	}
	if got := f.network["BaseUrl"]; got != "/jellyfin" {
		t.Fatalf("the app's own setting was lost: %v", got)
	}
}

// wizardUser names the first account the way the wizard's own flow does: the
// read that makes the default account exist, then the write that renames it.
func wizardUser(ctx context.Context, j *Jellyfin) error {
	if _, err := j.StartupUser(ctx); err != nil {
		return err
	}
	_, err := j.SetFirstUser(ctx, "alice", "person-pw")
	return err
}

// TestTheWizardReadCreatesTheAccount: the first account does not exist until
// the wizard's own read, and a write before it is a 404 -- the app's behaviour,
// and the reason the Job makes the pair together.
func TestTheWizardReadCreatesTheAccount(t *testing.T) {
	ctx := context.Background()
	f, j := newJellyfinFake(t)
	if _, err := j.SetFirstUser(ctx, "alice", "person-pw"); !IsNotFound(err) {
		t.Fatalf("a write before the read = %v, want the app's 404", err)
	}
	name, err := j.StartupUser(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if name == "" || f.defaultUsers != 1 {
		t.Fatalf("the read did not create the app's default account: name=%q created=%d", name, f.defaultUsers)
	}
	if ok, err := j.SetFirstUser(ctx, "alice", "person-pw"); err != nil || ok {
		t.Fatalf("the write after the read: ok=%v err=%v", ok, err)
	}
	if _, ok := f.users["alice"]; !ok {
		t.Fatal("the first account was not renamed")
	}
}
