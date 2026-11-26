package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// quiFake is qui's instance API: a Secure cookie, a 201 before the connection test,
// a PUT without a password keeping the stored one, and a 200 test with a flag.
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

	// Arr rows: a create is refused 409 at an existing URL, the list never carries a
	// key, and an update leaves unsent fields alone
	// (internal/api/handlers/arr.go at v1.36.1).
	arrRows     []Item
	arrNext     int
	arrWrites   int
	arrCreated  []Item
	arrUpdated  []Item
	arrTested   int
	arrTestFail bool
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
		case path == "/api/arr/instances" && r.Method == http.MethodGet:
			f.arrListHandler(w)
		case path == "/api/arr/instances" && r.Method == http.MethodPost:
			f.arrCreateHandler(w, r)
		case strings.HasPrefix(path, "/api/arr/instances/") && strings.HasSuffix(path, "/test"):
			f.arrTestHandler(w)
		case strings.HasPrefix(path, "/api/arr/instances/") && r.Method == http.MethodPut:
			f.arrUpdateHandler(w, r)
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
	// Secure is why the client captures the cookie: a jar would not replay it over
	// http.
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

// arrListHandler is GET /api/arr/instances: no stored key at all
// (internal/models/arr_instance.go marks it json:"-").
func (f *quiFake) arrListHandler(w http.ResponseWriter) {
	out := []Item{}
	for _, row := range f.arrRows {
		c := row.Clone()
		delete(c, "api_key")
		out = append(out, c)
	}
	writeJSON(w, out)
}

// arrCreateHandler is POST /api/arr/instances: UNIQUE on (type, base_url), answered
// 409.
func (f *quiFake) arrCreateHandler(w http.ResponseWriter, r *http.Request) {
	var body Item
	_ = json.NewDecoder(r.Body).Decode(&body)
	for _, row := range f.arrRows {
		if strings.EqualFold(row.Str("type"), body.Str("type")) &&
			strings.TrimRight(row.Str("base_url"), "/") == strings.TrimRight(body.Str("base_url"), "/") {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]string{"error": "An instance with this URL already exists for this type"})
			return
		}
	}
	f.arrCreated = append(f.arrCreated, body)
	f.arrNext++
	body["id"] = f.arrNext
	body["last_test_status"] = "unknown"
	f.arrRows = append(f.arrRows, body)
	f.arrWrites++
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, body)
}

// arrUpdateHandler is PUT /api/arr/instances/{id}: unsent fields stay, so "write only
// the key" works.
func (f *quiFake) arrUpdateHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/qui/api/arr/instances/"))
	var body Item
	_ = json.NewDecoder(r.Body).Decode(&body)
	for i, row := range f.arrRows {
		if row.Int("id") != id {
			continue
		}
		f.arrUpdated = append(f.arrUpdated, body)
		for _, key := range []string{"name", "base_url", "api_key", "enabled", "priority", "timeout_seconds"} {
			value, ok := body[key]
			if !ok || (key == "api_key" && value == "") {
				continue
			}
			row[key] = value
		}
		f.arrRows[i] = row
		f.arrWrites++
		writeJSON(w, row)
		return
	}
	http.Error(w, "ARR instance not found", http.StatusNotFound)
}

// arrTestHandler is POST /api/arr/instances/{id}/test: 200 either way, with
// {"success": false, "error": ...} when qui cannot reach the Arr.
func (f *quiFake) arrTestHandler(w http.ResponseWriter) {
	f.arrTested++
	if f.arrTestFail {
		writeJSON(w, map[string]any{"success": false, "error": "connection refused"})
		return
	}
	writeJSON(w, map[string]any{"success": true})
}

// loginQui logs in and fails the test if the app refuses.
func loginQui(t *testing.T, q *Qui) {
	t.Helper()
	if err := q.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
}

// A Secure cookie over plain http means the client sets Cookie itself.
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

// No uniqueness on name or host, so a create must match first.
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
	}, 0)
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

// A migrated instance is adopted by its host, its id recorded, its name kept.
func TestQuiEnsureInstanceAdoptsByHostAndKeepsTheOperatorsName(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.rows = []Item{{
		"id": 7, "name": "source-qbit", "host": "http://qbittorrent:8080",
		"username": "", "hasLocalFilesystemAccess": true, "connected": false,
	}}

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "",
		Password: "", HasLocalFilesystemAccess: true,
	}, 0)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Change != Unchanged || res.ID != 7 {
		t.Fatalf("adopt = %+v, want unchanged on the source's id 7", res)
	}
	if len(f.created) != 0 {
		t.Fatal("adoption created a second instance")
	}
	if len(f.rows) != 1 || f.rows[0].Str("name") != "source-qbit" {
		t.Fatalf("rows = %v, want the operator's one instance under their own name", f.rows)
	}
	if f.tested != 1 {
		t.Fatalf("tests = %d, want the adopted instance proved once", f.tested)
	}
}

// The host is compared in qui's own stored form: the operator may have typed a
// trailing slash in qui's UI, and that is still the release's qBittorrent.
func TestQuiEnsureInstanceAdoptsAHostWithATrailingSlash(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.rows = []Item{{
		"id": 4, "name": "mine", "host": "http://qbittorrent:8080/",
		"username": "", "hasLocalFilesystemAccess": true,
	}}

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "",
		HasLocalFilesystemAccess: true,
	}, 0)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.ID != 4 {
		t.Fatalf("adopt = %+v, want the instance at the same host", res)
	}
	if res.Change != Updated {
		t.Fatalf("change = %s, want the host written in the spec's spelling", res.Change)
	}
	if len(f.created) != 0 || len(f.rows) != 1 {
		t.Fatalf("rows = %v (created %d), want the one instance adopted", f.rows, len(f.created))
	}
}

// Nothing recorded and nothing at the host or name: first run, so create.
func TestQuiEnsureInstanceCreatesWhenNothingIsThere(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "",
		HasLocalFilesystemAccess: true,
	}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.ID != 1 {
		t.Fatalf("create = %+v, want created id 1", res)
	}
	if len(f.created) != 1 || len(f.rows) != 1 {
		t.Fatalf("created %d rows, hold %d", len(f.created), len(f.rows))
	}
}

// Two at one host are a refusal that writes nothing.
func TestQuiEnsureInstanceRefusesTwoAtTheHost(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.rows = []Item{
		{"id": 4, "name": "mine", "host": "http://qbittorrent:8080", "username": ""},
		{"id": 9, "name": "qBittorrent", "host": "http://qbittorrent:8080/", "username": ""},
	}

	_, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "",
		HasLocalFilesystemAccess: true,
	}, 0)
	if err == nil {
		t.Fatal("two instances at the same qBittorrent passed")
	}
	for _, want := range []string{"ids 4 and 9", "http://qbittorrent:8080"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to name %q", err, want)
		}
	}
	if f.writes != 0 || len(f.created) != 0 || len(f.updated) != 0 {
		t.Fatalf("a refusal wrote: %d writes, %d creates, %d updates", f.writes, len(f.created), len(f.updated))
	}
}

// The recorded id wins over a second instance at the host.
func TestQuiEnsureInstanceOwnedIDWinsOverAHostMatch(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.rows = []Item{
		{"id": 3, "name": "mine", "host": "http://elsewhere:8080", "username": ""},
		{"id": 9, "name": "qBittorrent", "host": "http://qbittorrent:8080", "username": ""},
	}

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "",
		HasLocalFilesystemAccess: true,
	}, 3)
	if err != nil {
		t.Fatalf("adopt the recorded id: %v", err)
	}
	if res.ID != 3 || res.Change != Updated {
		t.Fatalf("result = %+v, want the recorded id 3 updated", res)
	}
	if len(f.created) != 0 || len(f.rows) != 2 {
		t.Fatalf("created %d rows, hold %d; the recorded id must not create another", len(f.created), len(f.rows))
	}
	if got := f.rows[0].Str("host"); got != "http://qbittorrent:8080" {
		t.Fatalf("recorded instance host = %q, want the spec's", got)
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

	res, err := q.EnsureInstance(ctx, spec, 0)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	writes := f.writes
	res, err = q.EnsureInstance(ctx, spec, res.ID)
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

// The 201 is not proof: qui tests in a goroutine, so the client's own test decides.
func TestQuiEnsureInstanceFailsWhenTheInstanceIsNotConnected(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.testFail = true

	res, err := q.EnsureInstance(context.Background(), QuiInstanceSpec{
		Name: "qBittorrent", Host: "http://qbittorrent:8080", Username: "admin",
		Password: "qbit-pass", HasLocalFilesystemAccess: true,
	}, 0)
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
	}, 0)
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
