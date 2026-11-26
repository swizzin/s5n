package filebrowser

// Encoding the app's own tables, not a shape invented here: the columns are the
// ones its migrations create (internal/database/sqldb at v2.1.0-stable) and a
// user document is what its own User struct marshals.
//
// The fixture source kept its downloads and library under /media -- a mount of
// its own -- and called it "Media". The plan maps that mount onto
// /data/media, which is what apply's renames leave behind, so every reference
// to it is a scope of the box's one source at a position under /data.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

const schema = `
CREATE TABLE users (
    user_id TEXT PRIMARY KEY NOT NULL,
    username TEXT NOT NULL UNIQUE,
    perm_api INTEGER NOT NULL DEFAULT 0,
    perm_admin INTEGER NOT NULL DEFAULT 0,
    perm_realtime INTEGER NOT NULL DEFAULT 0,
    user_data TEXT NOT NULL
);
CREATE TABLE shares (
    hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    source TEXT NOT NULL,
    path TEXT NOT NULL,
    expire INTEGER NOT NULL DEFAULT 0,
    downloads INTEGER NOT NULL DEFAULT 0,
    password_hash TEXT,
    user_downloads TEXT,
    share_settings TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE access_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source TEXT NOT NULL,
    path TEXT NOT NULL,
    rule_data TEXT NOT NULL,
    UNIQUE(source, path)
);
CREATE TABLE quotas (
    id TEXT PRIMARY KEY NOT NULL,
    source TEXT NOT NULL,
    path TEXT NOT NULL,
    user_id TEXT,
    limit_bytes INTEGER NOT NULL DEFAULT 0
);
`

const planYAML = `
version: 2
data:
  root: /srv/library
  downloads: downloads
  media: media
apps:
  filebrowser:
    kind: kubernetes
    source: filebrowser
    config: /srv/apps/filebrowser
    ports: [8081]
    paths:
      - from: /config
        to: /config
      - from: /data
        to: /data
      - from: /media
        to: /data/media
`

// userJSON is one user with a source reference in every place the app stores
// one. The id is a 64-bit number on purpose: a JSON round trip through float64
// would rewrite its low digits.
func userJSON(scopes, permKey, pinnedKey, linkSource, declined string) string {
	return `{
  "id": 16141659585636484512,
  "username": "owner",
  "version": 6,
  "backendScopes": [{"path": "` + scopes + `", "scope": "/", "permissions": {"view": true}}],
  "backendSourcePermissions": {"` + permKey + `": {"view": true, "create": true}},
  "sourcePermissions": {"` + permKey + `": {"view": true, "create": true}},
  "pinnedItems": {"` + pinnedKey + `": {"/": ["film"]}},
  "declinedDefaultSources": ["` + declined + `"],
  "settings": {
    "locale": "en",
    "sidebarLinks": [{"category": "source", "target": "/", "sourceName": "` + linkSource + `"}]
  }
}`
}

func setup(t *testing.T, config string, users ...string) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, configName), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, dbName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	for i, doc := range users {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (user_id, username, user_data) VALUES (?, ?, ?)`,
			"id-"+string(rune('a'+i)), "owner"+string(rune('a'+i)), doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "filebrowser", Dir: dir, Plan: p}
}

// exec runs one statement against the staged database, for a fixture's own
// rows and for reading the result back.
func exec(t *testing.T, dir, stmt string) [][]any {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "SELECT") {
		rows, err := db.QueryContext(ctx, stmt)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var out [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	return nil
}

func userDoc(t *testing.T, dir string) map[string]any {
	t.Helper()
	rows := exec(t, dir, `SELECT user_data FROM users`)
	if len(rows) != 1 {
		t.Fatalf("%d users, want 1", len(rows))
	}
	var raw []byte
	switch v := rows[0][0].(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		t.Fatalf("user_data is a %T", rows[0][0])
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("user_data is no longer JSON: %v\n%s", err, raw)
	}
	return doc
}

// rawUser is one user document exactly as the database holds it.
func rawUser(t *testing.T, dir string) string {
	t.Helper()
	rows := exec(t, dir, `SELECT user_data FROM users`)
	if len(rows) != 1 {
		t.Fatalf("%d users, want 1", len(rows))
	}
	switch v := rows[0][0].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	t.Fatalf("user_data is a %T", rows[0][0])
	return ""
}

func changesByWhere(rep rewrite.Report) map[string]int {
	out := map[string]int{}
	for _, c := range rep.Changes {
		out[c.Where] += c.Count
	}
	return out
}

// TestAlreadyTheBoxesSourceIsANoOp: the common source install already points the
// app at /data, so the database needs no edit at all -- and an edit nobody needs
// is a write a migration should not make.
func TestAlreadyTheBoxesSourceIsANoOp(t *testing.T) {
	in := setup(t, `server:
  sources:
    - path: /data
`, userJSON("/data", "/data", "/data", "/data", "/data"))
	exec(t, in.Dir, `INSERT INTO shares (hash, user_id, source, path, share_settings) VALUES ('h', 'u', 'data', '/films', '{}')`)
	exec(t, in.Dir, `INSERT INTO access_rules (source, path, rule_data) VALUES ('data', '/films', '{}')`)
	before := userDoc(t, in.Dir)

	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: %v", rep.Uncovered)
	}
	if len(rep.Changes) != 0 {
		t.Fatalf("changes for a database that already says /data: %v", rep.Changes)
	}
	if len(rep.Notes) != 0 {
		t.Fatalf("notes for a database that already says /data: %v", rep.Notes)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := userDoc(t, in.Dir); !jsonEqual(t, before, after) {
		t.Fatalf("the user document changed on a no-op run:\n before %v\n after  %v", before, after)
	}
}

// TestRewritesTheSourceAPathAndName: a source the app knew by another directory
// becomes a scope of the box's one source, with its position kept, and its name
// becomes the one the chart's source has.
func TestRewritesTheSourceAPathAndName(t *testing.T) {
	in := setup(t, `server:
  sources:
    - path: /media
      name: Media
    - path: /data
`, userJSON("/media", "/media", "/media", "/media", "/media"))
	exec(t, in.Dir, `INSERT INTO shares (hash, user_id, source, path, share_settings) VALUES ('h', 'u', 'Media', '/Films/one.mkv', '{}')`)
	exec(t, in.Dir, `INSERT INTO access_rules (source, path, rule_data) VALUES ('Media', '/Films', '{}')`)
	exec(t, in.Dir, `INSERT INTO quotas (id, source, path, user_id, limit_bytes) VALUES ('q', 'Media', '/Films', 'u', 0)`)

	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: %v", rep.Uncovered)
	}
	want := map[string]int{
		"users.backendScopes.path":       1,
		"users.backendSourcePermissions": 1,
		"users.sourcePermissions":        1,
		"users.pinnedItems":              1,
		"users.declinedDefaultSources":   1,
		"users.sidebarLinks":             1,
		"shares.source":                  1,
		"shares.path":                    1,
		"access_rules.source":            1,
		"access_rules.path":              1,
		"quotas.source":                  1,
		"quotas.path":                    1,
	}
	got := changesByWhere(rep)
	for where, n := range want {
		if got[where] != n {
			t.Errorf("%s: %d changes, want %d (all: %v)", where, got[where], n, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("changes in places this test does not name: %v", got)
	}

	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	doc := userDoc(t, in.Dir)
	scopes, _ := doc["backendScopes"].([]any)
	if len(scopes) != 1 {
		t.Fatalf("backendScopes = %v", doc["backendScopes"])
	}
	scope := scopes[0].(map[string]any)
	if scope["path"] != "/data" || scope["scope"] != "/media" {
		t.Errorf("scope = %v, want the box's source and the position under it", scope)
	}
	for _, key := range []string{"backendSourcePermissions", "sourcePermissions", "pinnedItems"} {
		obj, _ := doc[key].(map[string]any)
		if _, ok := obj["/data"]; !ok || len(obj) != 1 {
			t.Errorf("%s = %v, want its one key rewritten to /data", key, obj)
		}
	}
	if list, _ := doc["declinedDefaultSources"].([]any); len(list) != 1 || list[0] != "/data" {
		t.Errorf("declinedDefaultSources = %v", doc["declinedDefaultSources"])
	}
	settings, _ := doc["settings"].(map[string]any)
	links, _ := settings["sidebarLinks"].([]any)
	link := links[0].(map[string]any)
	if link["sourceName"] != "/data" || link["target"] != "/media" {
		t.Errorf("sidebar link = %v, want the box's source and the target under it", link)
	}
	// The id is untouched: a document re-encoded through float64 would have
	// rounded it. The bytes are what is checked, because decoding into a map is
	// what would round it.
	if raw := rawUser(t, in.Dir); !strings.Contains(raw, `"id":16141659585636484512`) {
		t.Errorf("the re-encoded user document lost the 64-bit id:\n%s", raw)
	}

	if rows := exec(t, in.Dir, `SELECT source, path FROM shares`); rows[0][0] != "data" || rows[0][1] != "/media/Films/one.mkv" {
		t.Errorf("share = %v %v, want data and the path under the box's source", rows[0]...)
	}
	if rows := exec(t, in.Dir, `SELECT source, path FROM access_rules`); rows[0][0] != "data" || rows[0][1] != "/media/Films" {
		t.Errorf("access rule = %v %v", rows[0]...)
	}
	if rows := exec(t, in.Dir, `SELECT source, path FROM quotas`); rows[0][0] != "data" || rows[0][1] != "/media/Films" {
		t.Errorf("quota = %v %v", rows[0]...)
	}

	// A second pass has nothing left to do: the box's own source is a no-op and
	// the source the config still names has no row left pointing at it.
	rep2, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("after Write, Check still reports %v", rep2.Changes)
	}
}

// TestAnUnmappedSourcePathBlocks: a source the app's config names and the plan
// cannot place is a gap. Nothing is written while it stands.
func TestAnUnmappedSourcePathBlocks(t *testing.T) {
	in := setup(t, `server:
  sources:
    - path: /elsewhere
`, userJSON("/elsewhere", "/elsewhere", "/elsewhere", "/elsewhere", "/elsewhere"))
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Path != "/elsewhere" {
		t.Fatalf("Uncovered = %v, want the one path no rule covers", rep.Uncovered)
	}
	before := userDoc(t, in.Dir)
	if _, err := (rewriter{}).Write(context.Background(), in); err == nil {
		t.Fatal("Write accepted a plan that does not settle every source path")
	}
	if after := userDoc(t, in.Dir); !jsonEqual(t, before, after) {
		t.Fatal("Write changed the database after refusing")
	}
}

// TestAStalePathIsLeftAndCalledOut: a path only the database remembers is not a
// gap -- the app's config no longer names it -- so it is left as it is and said
// once.
func TestAStalePathIsLeftAndCalledOut(t *testing.T) {
	in := setup(t, `server:
  sources:
    - path: /data
`, userJSON("/gone", "/gone", "/gone", "/gone", "/gone"))
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a stale path blocked the run: %v", rep.Uncovered)
	}
	if len(rep.Changes) != 0 {
		t.Errorf("a stale path was rewritten: %v", rep.Changes)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "/gone") {
		t.Fatalf("Notes = %v, want one line naming the stale path", rep.Notes)
	}
}

// TestALocalAccountIsLeftAndCalledOut: the box's app signs its users in through
// the box's login, and it resolves an identity to a stored account by name --
// refusing one that another login method made. An account the source's own form
// created is therefore not the one the person signs in as, and the pass says so
// instead of renaming a row nothing in the plan says is the owner's.
func TestALocalAccountIsLeftAndCalledOut(t *testing.T) {
	local := strings.Replace(userJSON("/data", "/data", "/data", "/data", "/data"),
		`"version": 6,`, `"loginMethod": "password",`+"\n"+`  "version": 6,`, 1)
	in := setup(t, `server:
  sources:
    - path: /data
`, local)
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a local account blocked the run: %v", rep.Uncovered)
	}
	if len(rep.Changes) != 0 {
		t.Errorf("an account the box's login does not use was rewritten: %v", rep.Changes)
	}
	if len(rep.Left) != 1 {
		t.Fatalf("Left = %+v, want one line for the local account", rep.Left)
	}
	left := rep.Left[0]
	if left.File != dbName || left.Where != "users.loginMethod" {
		t.Fatalf("Left[0] = %+v, want the account row's login method", left)
	}
	if !strings.Contains(left.What, "owner") || !strings.Contains(left.What, "password") {
		t.Fatalf("Left[0].What = %q, want the account and its login method", left.What)
	}
	if !strings.Contains(left.Detail, "box's provider") {
		t.Fatalf("Left[0].Detail = %q, want the sentence saying the box's login is the one that signs in", left.Detail)
	}
}

// TestNoConfigFileStillRewrites: a source with no config.yaml defaulted its
// source to the working directory, so the database's own paths are the only
// record of what it had.
func TestNoConfigFileStillRewrites(t *testing.T) {
	in := setup(t, "", userJSON("/media", "/media", "/media", "/media", "/media"))
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: %v", rep.Uncovered)
	}
	if len(rep.Changes) == 0 {
		t.Fatal("nothing was rewritten and nothing said why")
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], configName) {
		t.Fatalf("Notes = %v, want one line saying the config file is absent", rep.Notes)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if scope := userDoc(t, in.Dir)["backendScopes"].([]any)[0].(map[string]any); scope["path"] != "/data" {
		t.Fatalf("scope = %v, want the box's source", scope)
	}
}

// TestNoDatabaseIsANote: an app whose staged config has no database has no
// stored path to rewrite, and saying so beats a silent pass.
func TestNoDatabaseIsANote(t *testing.T) {
	dir := t.TempDir()
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(context.Background(), rewrite.Input{App: "filebrowser", Dir: dir, Plan: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], dbName) {
		t.Fatalf("Notes = %v, want one line naming the absent database", rep.Notes)
	}
}

// TestConfigSourcesReadsTheList pins the scan of config.yaml: the two keys are
// flat scalars under a list item, and the item's own `config:` block must not be
// read as one of them.
func TestConfigSourcesReadsTheList(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, configName)
	body := `http:
  port: 8081
server:
  sources:
    - path: /media
      config:
        defaultEnabled: true
        defaultPermissions:
          view: true
    - name: downloads
      path: /downloads
auth:
  methods:
    noauth: true
`
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := configSources(name)
	if err != nil {
		t.Fatal(err)
	}
	want := []source{{path: "/media"}, {path: "/downloads", name: "downloads"}}
	if len(got) != len(want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestJoinScopeKeepsThePosition is the one arithmetic the rewrite rests on.
func TestJoinScopeKeepsThePosition(t *testing.T) {
	for _, tc := range []struct{ rel, scope, want string }{
		{"", "", ""},
		{"", "/tv", "/tv"},
		{"", "tv", "/tv"},
		{"/media", "", "/media"},
		{"/media", "/", "/media"},
		{"/media", "/tv", "/media/tv"},
		{"/downloads", "incomplete", "/downloads/incomplete"},
	} {
		if got := joinScope(tc.rel, tc.scope); got != tc.want {
			t.Errorf("joinScope(%q, %q) = %q, want %q", tc.rel, tc.scope, got, tc.want)
		}
	}
}

// jsonEqual compares two decoded documents without caring about key order.
func jsonEqual(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(left) == string(right)
}

var _ = sql.ErrNoRows
