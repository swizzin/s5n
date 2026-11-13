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

// quiFake is qui's instance API as the probe transcript found it: the login
// answers a Secure session cookie, a create answers 201 before it tests
// anything, a PUT without a password keeps the stored one, and the instance's
// test answers 200 with a "connected" flag rather than a status.
type quiFake struct {
	mu       sync.Mutex
	rows     []Item
	nextID   int
	writes   int
	created  []Item // bodies the create endpoint received
	updated  []Item // bodies the update endpoint received
	tested   int
	testFail bool
	login    int
	password string // what the app holds; a PUT without one keeps it
	disabled bool   // QUI__AUTH_DISABLED: the API is open, the login refuses
}

func (f *quiFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/qui")
		switch {
		case path == "/api/auth/login":
			f.loginHandler(w, r)
		case path == "/healthz/readiness":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case path == "/api/instances/" && r.Method == http.MethodGet:
			f.listHandler(w, r)
		case path == "/api/instances/" && r.Method == http.MethodPost:
			f.createHandler(w, r)
		case strings.HasPrefix(path, "/api/instances/") && strings.HasSuffix(path, "/test"):
			f.testHandler(w)
		case strings.HasPrefix(path, "/api/instances/") && r.Method == http.MethodPut:
			f.updateHandler(w, r)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}
}

func (f *quiFake) loginHandler(w http.ResponseWriter, r *http.Request) {
	if f.disabled {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]string{"error": "Endpoint disabled when authentication is disabled"})
		return
	}
	var body struct{ Username, Password string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if f.login != 0 {
		w.WriteHeader(f.login)
		writeJSON(w, map[string]string{"error": "Invalid credentials"})
		return
	}
	if body.Password != f.password {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "Invalid credentials"})
		return
	}
	// The Secure attribute is the whole reason the client captures the cookie
	// itself: a jar would refuse to replay it over http://.
	w.Header().Set("Set-Cookie", "qui_user_session=sess-abc; Path=/qui/; HttpOnly; Secure; SameSite=Lax")
	writeJSON(w, map[string]any{"message": "Login successful"})
}

func (f *quiFake) listHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Cookie") == "" && !f.disabled {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Unauthorized"))
		return
	}
	out := []Item{}
	for _, row := range f.rows {
		c := row.Clone()
		delete(c, "password")
		out = append(out, c)
	}
	writeJSON(w, out)
}

func (f *quiFake) createHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") == "" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Missing X-Requested-With header"))
		return
	}
	var body Item
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.created = append(f.created, body)
	f.nextID++
	body["id"] = f.nextID
	body["connected"] = false
	f.password = body.Str("password")
	f.rows = append(f.rows, body)
	f.writes++
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, body)
}

func (f *quiFake) testHandler(w http.ResponseWriter) {
	f.tested++
	if f.testFail {
		writeJSON(w, map[string]any{"connected": false, "error": "login failed: bad credentials"})
		return
	}
	writeJSON(w, map[string]any{"connected": true, "message": "Connection successful"})
}

func (f *quiFake) updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") == "" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Missing X-Requested-With header"))
		return
	}
	var body Item
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.updated = append(f.updated, body)
	for i, row := range f.rows {
		if row.Int("id") != body.Int("id") {
			continue
		}
		if body.Str("password") != "" {
			f.password = body.Str("password")
		}
		f.rows[i] = body
		f.writes++
		writeJSON(w, body)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func newQuiFake(t *testing.T) (*quiFake, *Qui) {
	t.Helper()
	f := &quiFake{password: "qui-pass-123"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, NewQui(srv.URL, "/qui", "s5n", "qui-pass-123")
}

// loginQui logs in and fails the test if the app refuses.
func loginQui(t *testing.T, q *Qui) {
	t.Helper()
	if err := q.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
}

// The session cookie is Secure, so the client has to keep the name=value and
// set Cookie itself: a jar would drop it over the plain http:// Service.
func TestQuiLoginReplaysTheSecureCookieByHand(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)

	rows, err := q.instances(context.Background())
	if err != nil {
		t.Fatalf("list with the captured cookie: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("instances = %v", rows)
	}
	if q.session != "qui_user_session=sess-abc" {
		t.Fatalf("session = %q, want only the cookie's name=value", q.session)
	}
	if f.writes != 0 {
		t.Fatalf("a read wrote %d times", f.writes)
	}
}

// qui has no uniqueness on name or host, so a create that did not match first
// would add a second row beside the user's own instance.
func TestQuiEnsureInstanceAdoptsByNameAndDoesNotDuplicate(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.rows = []Item{{
		"id": 3, "name": "qbittorrent", "host": "http://elsewhere:8080",
		"username": "someone", "hasLocalFilesystemAccess": false, "connected": false,
	}}

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "admin",
		Password: "qbit-pass", HasLocalFilesystemAccess: true,
	}, 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Change != Updated || res.ID != 3 {
		t.Fatalf("adopt = %+v, want updated on id 3", res)
	}
	if len(f.rows) != 1 {
		t.Fatalf("%d instances after adoption, want 1", len(f.rows))
	}
	row := f.rows[0]
	if row.Str("host") != "http://qbittorrent:8080" || !row.Bool("hasLocalFilesystemAccess") {
		t.Fatalf("row = %v, want s5n's host and local filesystem access", row)
	}
	// The name is not an owned field: a rename in qui's UI is the user's.
	if row.Str("name") != "qbittorrent" {
		t.Fatalf("name = %q, want the live one", row.Str("name"))
	}
	if fields := driftFields(res.Drift); strings.Join(fields, ",") != "host,username,hasLocalFilesystemAccess" {
		t.Fatalf("drift = %v", fields)
	}
	if len(f.created) != 0 {
		t.Fatal("adoption created a second instance")
	}
}

// A second run on the recorded id and fingerprint writes nothing and still
// proves the instance with qui's own test.
func TestQuiEnsureInstanceIsIdempotent(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	ctx := context.Background()
	spec := QuiInstanceSpec{Name: "qBittorrent", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-pass", HasLocalFilesystemAccess: true}

	res, err := q.EnsureInstance(ctx, spec, 0, "")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	writes := f.writes
	res, err = q.EnsureInstance(ctx, spec, res.ID, SecretFingerprint("qbit-pass"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Change != Unchanged {
		t.Fatalf("second run = %s, want unchanged", res.Change)
	}
	if f.writes != writes {
		t.Fatalf("second run made %d writes", f.writes-writes)
	}
	if len(f.updated) != 0 {
		t.Fatalf("second run PUT %d times", len(f.updated))
	}
	if f.tested != 2 {
		t.Fatalf("tests = %d, want one per run: the test is the proof, not a write", f.tested)
	}
}

// A rotated qBittorrent password is invisible in the API's answer, so the
// fingerprint in owned.json is what makes the instance get rewritten.
func TestQuiEnsureInstanceRewritesOnRotatedPassword(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	ctx := context.Background()
	base := QuiInstanceSpec{Name: "qBittorrent", Host: "http://qbittorrent:8080",
		Username: "admin", Password: "qbit-old", HasLocalFilesystemAccess: true}

	res, err := q.EnsureInstance(ctx, base, 0, "")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	rotated := base
	rotated.Password = "qbit-new"
	res, err = q.EnsureInstance(ctx, rotated, res.ID, SecretFingerprint("qbit-old"))
	if err != nil {
		t.Fatalf("rotated run: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("a rotated password reported %s, want updated", res.Change)
	}
	if len(res.Drift) != 0 {
		t.Fatalf("drift = %+v, want none: only the secret changed", res.Drift)
	}
	if len(f.updated) != 1 || f.updated[0].Str("password") != "qbit-new" {
		t.Fatalf("PUT bodies = %v, want one carrying the new password", f.updated)
	}
}

// The create's 201 is not proof: qui tests the connection in a goroutine after
// it answers, so the client's own test call is what decides.
func TestQuiEnsureInstanceFailsWhenTheInstanceIsNotConnected(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.testFail = true

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "admin",
		Password: "qbit-pass", HasLocalFilesystemAccess: true,
	}, 0, "")
	if err == nil {
		t.Fatal("a created-but-unreachable instance passed")
	}
	// The id is still reported: the caller must record what exists, and the
	// next run finds it by id instead of creating a second one.
	if res.ID != 1 {
		t.Fatalf("result = %+v, want the created id", res)
	}
}

func TestQuiTestInstanceReportsNotConnected(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.testFail = true

	err := q.TestInstance(context.Background(), 1)
	if err == nil {
		t.Fatal("a disconnected instance passed its test")
	}
	if !strings.Contains(err.Error(), "not connected") || !strings.Contains(err.Error(), "bad credentials") {
		t.Fatalf("error = %v, want qui's own reason", err)
	}
}

func TestQuiLoginRejectsCredentials(t *testing.T) {
	f, q := newQuiFake(t)
	f.password = "changed-inside-the-app"
	err := q.Login(context.Background())
	var rej *LoginRejectedError
	if !asLoginRejected(err, &rej) {
		t.Fatalf("error = %v (%T), want a LoginRejectedError", err, err)
	}
	if rej.App != "qui" || rej.Username != "s5n" {
		t.Fatalf("rejection = %+v", rej)
	}
}

// qui with its login switched off refuses the login endpoint and needs no
// session: that is a plain helm install, not a broken one.
func TestQuiLoginWithAuthenticationDisabledIsNotAFailure(t *testing.T) {
	f, q := newQuiFake(t)
	f.disabled = true
	if err := q.Login(context.Background()); err != nil {
		t.Fatalf("login on an install with qui's login off: %v", err)
	}
	if q.session != "" {
		t.Fatalf("session = %q, want none", q.session)
	}
	if _, err := q.instances(context.Background()); err != nil {
		t.Fatalf("list without a session: %v", err)
	}
}

func TestQuiCreateUsesTheReturnedID(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "admin",
		Password: "qbit-pass", HasLocalFilesystemAccess: true,
	}, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.ID != 1 {
		t.Fatalf("create = %+v", res)
	}
	if len(f.created) != 1 || f.created[0].Str("name") != "qBittorrent" {
		t.Fatalf("create bodies = %v", f.created)
	}
	if f.created[0].Bool("hasLocalFilesystemAccess") != true {
		t.Fatal("the create body did not ask for local filesystem access")
	}
}

func driftFields(drift []FieldChange) []string {
	out := make([]string, 0, len(drift))
	for _, d := range drift {
		out = append(out, d.Field)
	}
	return out
}
