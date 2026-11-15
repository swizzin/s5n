// Package arr rewrites the stored paths of a migrated Sonarr, Radarr or
// Prowlarr install, and the addresses it stored for the other apps on the
// source, before box's release takes its config over.
//
// Everything here works on the copied config directory: the app's SQLite
// database and config.xml beside it. The schema it reads is the source's
// (Sonarr 4.0.14.2938, Radarr 5.20.1.9773, Prowlarr 2.7.0.5723): nothing in
// either migration range renames, drops or re-bases a path column, so a
// rewrite before the target's first start is written against columns that
// exist in both versions, and the first start reads paths that exist
// (research: arr-qbit §3.2).
//
// Check opens the database read-only and writes nothing anywhere. Write makes
// exactly the changes the Check before it reported: one transaction per
// database, one atomic write for config.xml.
package arr

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// The catalog apps this package serves. One rewriter serves every instance,
// because the tables are the app's: a plan's "sonarr-4k" reads its rows with
// the same schema as any other Sonarr.
func init() {
	for _, name := range []string{kindSonarr, kindRadarr, kindProwlarr} {
		rewrite.Register(name, rewriter{kind: name})
	}
}

const (
	kindSonarr   = "sonarr"
	kindRadarr   = "radarr"
	kindProwlarr = "prowlarr"
)

type rewriter struct {
	// kind is the catalog app, which decides the tables and the settings this
	// package reads. in.App is the instance name, the plan's key.
	kind string
}

// dbFile is the database each app keeps in its config directory.
func (r rewriter) dbFile() string { return r.kind + ".db" }

func (r rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(in.Dir, r.dbFile()))
	if err != nil {
		return rewrite.Report{}, err
	}
	defer db.Close()
	a, err := r.analyse(ctx, in, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

func (r rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(in.Dir, r.dbFile()))
	if err != nil {
		return rewrite.Report{}, err
	}
	defer db.Close()
	a, err := r.analyse(ctx, in, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	if err := a.apply(ctx, db); err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

func (r rewriter) analyse(ctx context.Context, in rewrite.Input, db *sql.DB) (*analysis, error) {
	file := r.dbFile()
	findings, err := sqlitedb.IntegrityCheck(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", file, err)
	}
	if len(findings) > 0 {
		// A copy taken from a running app can be torn, and rewriting one would
		// carry the tear into the target. The fix is another copy from the
		// stopped source, so this is an error rather than a report entry.
		return nil, fmt.Errorf("%s is damaged (%s); copy it again from the stopped source", file, strings.Join(findings, "; "))
	}
	a := &analysis{
		kind:          r.kind,
		app:           in.App,
		dir:           in.Dir,
		file:          file,
		plan:          in.Plan,
		targets:       in.Targets,
		changes:       make(map[changeKey]int),
		clientTargets: make(map[string]string),
	}
	if err := a.database(ctx, db); err != nil {
		return nil, err
	}
	if err := a.configXML(); err != nil {
		return nil, err
	}
	return a, nil
}

// edit is one row update the write pass makes. The table and column are this
// package's own constants, never a value read from the database.
type edit struct {
	table  string
	column string
	id     int64
	value  string
}

// changeKey aggregates a report's changes: one line per file, place and kind.
type changeKey struct {
	file  string
	where string
	what  string
}

// analysis is one pass's findings: what to report, and the edits that make it
// true. Check keeps the report and drops the edits; Write keeps both.
type analysis struct {
	kind string
	// app is the instance name the plan is keyed by and the app whose path
	// table its own stored paths go through.
	app     string
	dir     string
	file    string // the database's name, relative to dir
	plan    *plan.Plan
	targets map[string]rewrite.Endpoint

	changes  map[changeKey]int
	uncover  []plan.Uncovered
	unclass  []rewrite.Address
	notes    []string
	owned    []rewrite.Owned
	edits    []edit
	dropped  []int64 // Commands rows that never finished
	mappings []int64 // RemotePathMappings rows that became identity
	conf     []byte  // new config.xml, nil when it does not change
	confMode fs.FileMode

	// clientTargets is the app each download client's stored host now means,
	// by the host as stored: a remote path mapping names a client by that host,
	// so it inherits the client's path table.
	clientTargets map[string]string
}

// add counts one kind of change in one place.
func (a *analysis) add(file, where, what string) {
	a.changes[changeKey{file: file, where: where, what: what}]++
}

// gap records a stored absolute path no rule in the plan covers. The app named
// is the one whose table was asked: a download client's directory is that
// client's path, not this app's.
func (a *analysis) gap(app, file, key, stored string) {
	a.uncover = append(a.uncover, plan.Uncovered{App: app, File: file, Key: key, Path: stored})
}

// report renders the pass in a stable order, so one run reads the same as the
// next and a test can hold it to it.
func (a *analysis) report() rewrite.Report {
	rep := rewrite.Report{
		App:          a.app,
		Uncovered:    a.uncover,
		Unclassified: a.unclass,
		Notes:        a.notes,
		Owned:        a.owned,
	}
	for k, n := range a.changes {
		rep.Changes = append(rep.Changes, rewrite.Change{File: k.file, Where: k.where, What: k.what, Count: n})
	}
	sort.Slice(rep.Changes, func(i, j int) bool {
		x, y := rep.Changes[i], rep.Changes[j]
		switch {
		case x.File != y.File:
			return x.File < y.File
		case x.Where != y.Where:
			return x.Where < y.Where
		}
		return x.What < y.What
	})
	sort.Slice(rep.Owned, func(i, j int) bool {
		x, y := rep.Owned[i], rep.Owned[j]
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.ID < y.ID
	})
	sort.Slice(rep.Uncovered, func(i, j int) bool { return rep.Uncovered[i].String() < rep.Uncovered[j].String() })
	sort.Slice(rep.Unclassified, func(i, j int) bool { return rep.Unclassified[i].String() < rep.Unclassified[j].String() })
	return rep
}

// apply makes exactly what the pass found. The database is one transaction:
// half a rewrite is a target whose rows disagree about where the library is.
func (a *analysis) apply(ctx context.Context, db *sql.DB) error {
	if err := a.refuse(); err != nil {
		return err
	}
	if len(a.edits)+len(a.dropped)+len(a.mappings) > 0 {
		if err := a.applyDB(ctx, db); err != nil {
			return err
		}
	}
	if a.conf == nil {
		return nil
	}
	if err := atomicfile.Write(filepath.Join(a.dir, "config.xml"), a.conf, a.confMode); err != nil {
		return fmt.Errorf("write config.xml: %w", err)
	}
	return nil
}

// refuse stops a write pass that met something its Check did not report. Apply
// runs every app's Check before any app's Write, so a gap or an unclassified
// address here means the staged copy moved under the run; half a migration is
// worse than none.
func (a *analysis) refuse() error {
	if len(a.uncover) > 0 {
		return &plan.UncoveredError{Paths: a.uncover}
	}
	if len(a.unclass) > 0 {
		sort.Slice(a.unclass, func(i, j int) bool { return a.unclass[i].String() < a.unclass[j].String() })
		names := make([]string, len(a.unclass))
		for i, addr := range a.unclass {
			names[i] = addr.String()
		}
		return fmt.Errorf("%d addresses no host rule settles:\n  %s", len(a.unclass), strings.Join(names, "\n  "))
	}
	return nil
}

func (a *analysis) applyDB(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", a.file, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to a no-op
	for _, e := range a.edits {
		//nolint:gosec // the table and column are constants in this package, never a stored value
		if _, err := tx.ExecContext(ctx, "UPDATE "+e.table+" SET "+e.column+" = ? WHERE Id = ?", e.value, e.id); err != nil {
			return fmt.Errorf("update %s.%s row %d in %s: %w", e.table, e.column, e.id, a.file, err)
		}
	}
	for _, id := range a.mappings {
		if _, err := tx.ExecContext(ctx, "DELETE FROM RemotePathMappings WHERE Id = ?", id); err != nil {
			return fmt.Errorf("delete RemotePathMappings row %d in %s: %w", id, a.file, err)
		}
	}
	for _, id := range a.dropped {
		if _, err := tx.ExecContext(ctx, "DELETE FROM Commands WHERE Id = ?", id); err != nil {
			return fmt.Errorf("delete Commands row %d in %s: %w", id, a.file, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", a.file, err)
	}
	return nil
}
