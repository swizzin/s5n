package jellyfin

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

// fixtureDB is the database a 12.4 image creates: the real schema, with the
// three rows its first start writes and nothing else. A test copies it and adds
// the rows a source with a library would have, so the rewrite runs against the
// pinned tag's own column set rather than a table this package made up.
const fixtureDB = "testdata/jellyfin-12.4.db"

// planFor is a source whose library sits under /srv/media on the host and will
// sit under /data/media on the box. Every path in it is invented.
func planFor(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`version: 2
apps:
  jellyfin:
    kind: kubernetes
    source: jellyfin
    config: /srv/apps/jellyfin
    namespace: media
    ports: [8096]
    paths:
      - from: /srv/media
        to: /data/media
      # What discovery writes on a second run, when the tree already sits where
      # box wants it: a rewriter's own work has to read as covered, or the
      # Write after a Check stops on the Check's own output.
      - from: /data/media
        to: /data/media
`))
	if err != nil {
		t.Fatalf("parse plan: %v", err)
	}
	return p
}

// stage copies the fixture into a temp config directory, which is what apply
// hands a rewriter: a private copy of the app's /config.
func stage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixtureDB)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dbFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// addRows inserts the two shapes a source with a library has: an item whose
// Path is a media file, and a collection folder whose library roots are in its
// Data blob (LibraryOptions.PathInfos). The folder's own Path is under /config,
// which is the app's own tree and must never be rewritten.
func addRows(t *testing.T, dir string, rows ...row) {
	t.Helper()
	db, err := sqlitedb.OpenReadWrite(context.Background(), filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, r := range rows {
		// The columns a 12.4 database declares NOT NULL with no default; the
		// rest of the row is the app's to fill.
		if _, err := db.ExecContext(context.Background(),
			"INSERT INTO BaseItems (Id, Type, Name, Path, Data, IsFolder, IsInMixedFolder, IsLocked, IsMovie, IsRepeat, IsSeries, IsVirtualItem) VALUES (?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0, 0)",
			r.id, r.typ, r.name, nullString(r.path), nullString(r.data)); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
}

type row struct{ id, typ, name, path, data string }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func readRow(t *testing.T, dir, id string) (path, data string) {
	t.Helper()
	db, err := sqlitedb.OpenReadOnly(context.Background(), filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var p, d sql.NullString
	if err := db.QueryRowContext(context.Background(),
		"SELECT Path, Data FROM BaseItems WHERE Id = ?", id).Scan(&p, &d); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return p.String, d.String
}

func input(t *testing.T, dir string) rewrite.Input {
	t.Helper()
	return rewrite.Input{App: "jellyfin", Dir: dir, Plan: planFor(t)}
}

// TestTheRewriteMovesTheDatabasePaths: the paths a Jellyfin library holds are
// rows, not settings. The item's file path, the library's roots inside the JSON
// blob beside them, and the app's own /config tree are the three shapes the
// pass has to tell apart -- the third is not media and must not be touched.
func TestTheRewriteMovesTheDatabasePaths(t *testing.T) {
	ctx := context.Background()
	dir := stage(t)
	addRows(t, dir,
		row{id: "00000000-0000-0000-0000-0000000000a1", typ: "MediaBrowser.Controller.Entities.Movies.Movie",
			name: "A Film", path: "/srv/media/movies/A Film (2020)/A Film.mkv"},
		row{id: "00000000-0000-0000-0000-0000000000a2", typ: "MediaBrowser.Controller.Entities.CollectionFolder",
			name: "Movies", path: "/config/root/movies",
			data: `{"PathInfos":[{"Path":"/srv/media/movies"}],"EnableRealtimeMonitor":true}`},
	)

	rep, err := rewriter{}.Check(ctx, input(t, dir))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(rep.Uncovered) != 0 {
		t.Fatalf("uncovered paths: %v", rep.Uncovered)
	}
	// The check wrote nothing.
	if got, _ := readRow(t, dir, "00000000-0000-0000-0000-0000000000a1"); got != "/srv/media/movies/A Film (2020)/A Film.mkv" {
		t.Fatalf("Check wrote to the database: %q", got)
	}

	rep, err = rewriter{}.Write(ctx, input(t, dir))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var total int
	for _, c := range rep.Changes {
		if c.File != dbFile || c.What != "path" {
			t.Fatalf("change %+v", c)
		}
		total += c.Count
	}
	if total != 2 {
		t.Fatalf("%d rows changed, want 2: %+v", total, rep.Changes)
	}

	path, data := readRow(t, dir, "00000000-0000-0000-0000-0000000000a1")
	if path != "/data/media/movies/A Film (2020)/A Film.mkv" {
		t.Fatalf("item path = %q", path)
	}
	path, data = readRow(t, dir, "00000000-0000-0000-0000-0000000000a2")
	if path != "/config/root/movies" {
		t.Fatalf("the app's own path was rewritten: %q", path)
	}
	var doc struct{ PathInfos []struct{ Path string } }
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("library options: %v", err)
	}
	if len(doc.PathInfos) != 1 || doc.PathInfos[0].Path != "/data/media/movies" {
		t.Fatalf("library roots = %+v", doc.PathInfos)
	}
	// Every other field of the blob survives: it is the app's JSON, and the
	// rewrite only owns the paths in it.
	if !strings.Contains(data, `"EnableRealtimeMonitor":true`) {
		t.Fatalf("the blob lost a setting: %s", data)
	}

	// A second pass has nothing to do, which is what makes a re-run safe.
	rep, err = rewriter{}.Check(ctx, input(t, dir))
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if len(rep.Changes) != 0 || len(rep.Uncovered) != 0 {
		t.Fatalf("second pass: %+v", rep)
	}
}

// TestAPathNoRuleCoversStopsTheWrite: a plan that does not say where a library
// lives is a migration that would leave a film unplayable, so the rewrite names
// the path and writes nothing at all -- not the rows it did understand.
func TestAPathNoRuleCoversStopsTheWrite(t *testing.T) {
	ctx := context.Background()
	dir := stage(t)
	addRows(t, dir,
		row{id: "00000000-0000-0000-0000-0000000000b1", typ: "MediaBrowser.Controller.Entities.Movies.Movie",
			name: "Elsewhere", path: "/mnt/other/film.mkv"},
	)
	rep, err := rewriter{}.Check(ctx, input(t, dir))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Path != "/mnt/other/film.mkv" {
		t.Fatalf("uncovered = %+v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, input(t, dir)); err == nil {
		t.Fatal("the write ran with a path no rule covers")
	}
	if got, _ := readRow(t, dir, "00000000-0000-0000-0000-0000000000b1"); got != "/mnt/other/film.mkv" {
		t.Fatalf("a refused write changed the row: %q", got)
	}
}

// TestAnInternalPathIsNotAGap: the root folder row and the playlists folder are
// under /config, which is the app's own data directory. Leaving them is the
// decision, not a missing rule: no plan maps the inside of the app.
func TestAnInternalPathIsNotAGap(t *testing.T) {
	ctx := context.Background()
	dir := stage(t)
	rep, err := rewriter{}.Check(ctx, input(t, dir))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(rep.Uncovered) != 0 {
		t.Fatalf("the fixture's own rows are gaps: %+v", rep.Uncovered)
	}
	if len(rep.Changes) != 0 {
		t.Fatalf("the fixture's own rows changed: %+v", rep.Changes)
	}
}

// TestTheDatabaseIsTheTagsOwnSchema pins the tables and columns the rewriter
// writes against the fixture, which a 12.4 image produced. A source version that
// moved a path somewhere else fails here rather than migrating nothing.
func TestTheDatabaseIsTheTagsOwnSchema(t *testing.T) {
	path, err := filepath.Abs(fixtureDB)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	defer db.Close()
	columns := map[string]bool{}
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info("+baseItems+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["Id"] || !columns[pathColumn] || !columns[dataColumn] {
		t.Fatalf("%s has %v; the rewriter writes Id, %s and %s", baseItems, columns, pathColumn, dataColumn)
	}
	// The entity's own mapped name, and the absence of a table a 10.x-era
	// reader would look for: library roots are in BaseItems.Data.
	var libs int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='Libraries'").Scan(&libs); err != nil {
		t.Fatal(err)
	}
	if libs != 0 {
		t.Fatal("the fixture has a Libraries table, so this rewriter's model of the schema is out of date")
	}
}
