// Package bazarr rewrites what a migrated Bazarr stored about the Arrs it reads
// and the files it subtitles, before box's release takes its config over.
// config/config.yaml (bazarr/app/config.py:519 at v1.6.2) holds each Arr's
// address (sonarr.*, radarr.*) and the path mappings; db/bazarr.db holds the
// library as the Arrs reported it (database.py's table_shows and the rest). Both
// are rewritten before the target starts. general.base_url and auth.apikey are
// the chart's init container's (internal/bazconf), not this package's.
package bazarr

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

func init() { rewrite.Register("bazarr", rewriter{}) }

const (
	// settingsFile is config.yaml under the config directory (config.py's
	// config_base_file_path).
	settingsFile = "config/config.yaml"
	// dbFile is the database in config.py:195's db directory (database.py:82-97).
	dbFile = "db/bazarr.db"
)

type rewriter struct{}

func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// run is both passes: read the two files, decide every edit, and (Write only)
// make them, so the two reports stay the same.
func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	a := &analysis{app: in.App, in: in, changes: map[changeKey]int{}}
	if err := a.settings(); err != nil {
		return rewrite.Report{}, err
	}
	if err := a.database(ctx, write); err != nil {
		return rewrite.Report{}, err
	}
	if write {
		if err := a.applySettings(); err != nil {
			return rewrite.Report{}, err
		}
		if err := a.applyDatabase(ctx); err != nil {
			return rewrite.Report{}, err
		}
	}
	return a.report(), nil
}

type changeKey struct{ file, where, what string }

// analysis is one pass over the app's two files.
type analysis struct {
	app  string
	in   rewrite.Input
	doc  *settingsDoc
	path string // the settings file's path, for the error text

	changes map[changeKey]int
	uncover []plan.Uncovered
	unclass []rewrite.Address
	left    []rewrite.Left
	notes   []string

	// settingsChanged says the document's bytes differ from the file's.
	settingsChanged bool

	// db is the database the pass read; edits are the row updates Write makes,
	// kept apart so Check can close db without writing.
	db    *sql.DB
	edits []edit
}

// edit is one row update the write pass makes.
type edit struct {
	table, column string
	id            int64
	value         string
}

func (a *analysis) add(file, where, what string) {
	a.changes[changeKey{file: file, where: where, what: what}]++
}

// gap records a stored path no rule covers: a path the box does not have is a
// library Bazarr cannot subtitle.
func (a *analysis) gap(app, file, where, stored string) {
	a.uncover = append(a.uncover, plan.Uncovered{App: app, File: file, Key: where, Path: stored})
}

func (a *analysis) report() rewrite.Report {
	rep := rewrite.Report{
		App:          a.app,
		Uncovered:    a.uncover,
		Unclassified: a.unclass,
		Left:         a.left,
		Notes:        a.notes,
	}
	for k, n := range a.changes {
		rep.Changes = append(rep.Changes, rewrite.Change{File: k.file, Where: k.where, What: k.what, Count: n})
	}
	sort.Slice(rep.Changes, func(i, j int) bool {
		x, y := rep.Changes[i], rep.Changes[j]
		if x.File != y.File {
			return x.File < y.File
		}
		if x.Where != y.Where {
			return x.Where < y.Where
		}
		return x.What < y.What
	})
	return rep
}

// pathColumn is one column holding a library path and the key its row is
// updated by, all constants from database.py at v1.6.2.
type pathColumn struct {
	table, key, column string
}

// pathColumns are every stored path the app acts on: show, episode and film
// rows, the root-folder lists, and placed subtitle files.
var pathColumns = []pathColumn{
	{"table_shows", "sonarrSeriesId", columnPath},
	{"table_episodes", "sonarrEpisodeId", columnPath},
	{"table_episodes_subtitles", "id", columnPath},
	{"table_movies", "radarrId", columnPath},
	{"table_movies_subtitles", "id", columnPath},
	{"table_shows_rootfolder", "id", columnPath},
	{"table_movies_rootfolder", "id", columnPath},
}

// columnPath is the one column name every table here uses for a stored path.
const columnPath = "path"

// database opens db/bazarr.db and rewrites every path column. An older source may
// predate a table, and a missing table is not a gap.
func (a *analysis) database(ctx context.Context, write bool) error {
	path := filepath.Join(a.in.Dir, dbFile)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			a.notes = append(a.notes, dbFile+" is not in the staged config; nothing to rewrite in it")
			return nil
		}
		return fmt.Errorf("bazarr: %w", err)
	}
	open := sqlitedb.OpenReadOnly
	if write {
		open = sqlitedb.OpenReadWrite
	}
	db, err := open(ctx, path)
	if err != nil {
		return err
	}
	a.db = db
	if !write {
		defer func() { _ = db.Close() }()
	}
	findings, err := sqlitedb.IntegrityCheck(ctx, db)
	if err != nil {
		return fmt.Errorf("check %s: %w", dbFile, err)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%s is damaged (%s); copy it again from the stopped source", dbFile, joinFindings(findings))
	}
	for _, c := range pathColumns {
		if err := a.column(ctx, db, c); err != nil {
			return err
		}
	}
	return nil
}

// column rewrites one path column.
func (a *analysis) column(ctx context.Context, db *sql.DB, c pathColumn) error {
	//nolint:gosec // identifiers are constants in this package
	rows, err := db.QueryContext(ctx, "SELECT "+c.key+", "+c.column+" FROM "+c.table)
	if err != nil {
		if isNoSuchTable(err) {
			return nil
		}
		return fmt.Errorf("read %s.%s in %s: %w", c.table, c.column, dbFile, err)
	}
	defer rows.Close()
	where := c.table + "." + c.column
	for rows.Next() {
		var id int64
		var stored sql.NullString
		if err := rows.Scan(&id, &stored); err != nil {
			return fmt.Errorf("read %s in %s: %w", where, dbFile, err)
		}
		// An unplaced subtitle or unseen library row can be empty: not a gap.
		if !stored.Valid || stored.String == "" {
			continue
		}
		out, ok := a.in.Plan.Rewrite(a.app, stored.String)
		if !ok {
			a.gap(a.app, dbFile, where, stored.String)
			continue
		}
		if out != stored.String {
			a.add(dbFile, where, "path")
			a.edits = append(a.edits, edit{table: c.table, column: c.column, id: id, value: out})
		}
	}
	return rows.Err()
}

// applyDatabase makes the row updates in one transaction, after the settings
// file: a run that cannot finish must not leave half a library.
func (a *analysis) applyDatabase(ctx context.Context) error {
	if a.db == nil {
		return nil
	}
	defer func() { _ = a.db.Close() }()
	if len(a.edits) == 0 {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", dbFile, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range a.edits {
		//nolint:gosec // identifiers are constants in this package
		if _, err := tx.ExecContext(ctx,
			"UPDATE "+e.table+" SET "+e.column+" = ? WHERE "+pathKey(e.table)+" = ?", e.value, e.id); err != nil {
			return fmt.Errorf("update %s.%s in %s: %w", e.table, e.column, dbFile, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", dbFile, err)
	}
	return nil
}

// pathKey is a table's key, read from pathColumns so the two cannot drift.
func pathKey(table string) string {
	for _, c := range pathColumns {
		if c.table == table {
			return c.key
		}
	}
	return "id"
}

func joinFindings(findings []string) string {
	out := ""
	for i, f := range findings {
		if i > 0 {
			out += "; "
		}
		out += f
	}
	return out
}
