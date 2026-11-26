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

// fixtureDB is a 12.4 image's real schema, so the rewrite runs against the pinned
// tag's own columns rather than a table this package made up.
const fixtureDB = "testdata/jellyfin-12.4.db"

// planFor: the library under /srv/media, becoming /data/media. Paths are
// invented.
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

// stage copies the fixture into a temp config directory, as apply does.
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

// addRows inserts what a source with a library has: an item whose Path is a media
// file and a collection folder whose roots are in its Data blob.
func addRows(t *testing.T, dir string, rows ...row) {
	t.Helper()
	db, err := sqlitedb.OpenReadWrite(context.Background(), filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, r := range rows {
		// The NOT NULL columns with no default; the app fills the rest.
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

// TestTheRewriteMovesTheDatabasePaths: the item path, the roots in the JSON blob
// and the app's own /config tree are the three shapes the pass tells apart.
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

	path, _ := readRow(t, dir, "00000000-0000-0000-0000-0000000000a1")
	if path != "/data/media/movies/A Film (2020)/A Film.mkv" {
		t.Fatalf("item path = %q", path)
	}
	path, data := readRow(t, dir, "00000000-0000-0000-0000-0000000000a2")
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
	// The blob's other fields survive: only the paths are the rewrite's.
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

// TestAPathNoRuleCoversStopsTheWrite: the rewrite names the path and writes
// nothing at all, not even the rows it understood.
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

// TestAnInternalPathIsNotAGap: leaving the app's /config tree is the decision,
// not a missing rule.
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

// TestTheDatabaseIsTheTagsOwnSchema pins the tables and columns against a 12.4
// image's schema, so a source version that moved a path fails here.
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
	// The absence of a table a 10.x-era reader would look for: roots are in
	// BaseItems.Data.
	var libs int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='Libraries'").Scan(&libs); err != nil {
		t.Fatal(err)
	}
	if libs != 0 {
		t.Fatal("the fixture has a Libraries table, so this rewriter's model of the schema is out of date")
	}
}
