// Package jellyfin rewrites the media paths a migrated Jellyfin install stored,
// before box's release takes its config over.
//
// Unlike every other app in the catalog, Jellyfin's paths are not in an XML
// file: they are rows in data/jellyfin.db. The columns are taken from the pinned
// tag's own entity classes (v12.4), not guessed from a 10.x schema:
//
//   - BaseItems.Path — Jellyfin.Database/Jellyfin.Database.Implementations/
//     Entities/BaseItemEntity.cs: `public string? Path`, the absolute path of
//     every item it has indexed (a film's file, a collection folder's own
//     directory).
//   - BaseItems.Data — the same class's `public string? Data`: the JSON blob
//     that carries what the entity has no column for, and where a library's
//     roots live (MediaBrowser.Model/Configuration/LibraryOptions.cs,
//     PathInfos, and a media source's Path). It is rewritten as JSON, string by
//     string, because the blob's shape is the app's and a rewriter that named
//     one key would miss the next.
//
// There is no Libraries table in v12.4's database: the entity exists in the
// source tree but is not mapped, and a library is a CollectionFolder BaseItems
// row. That was checked against the schema a 12.4 image creates, which is the
// test fixture this package ships.
//
// A path under the app's own data directory (/config, and the cache and temp
// trees it uses) is not a media path: it is where Jellyfin keeps its database,
// its library view and its transcodes. Those are left exactly as they are and
// are not reported as gaps, because no plan rule can or should map them.
//
// Check opens the database read-only and writes nothing anywhere. Write makes
// exactly the changes the Check before it reported, in one transaction.
package jellyfin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

func init() {
	rewrite.Register("jellyfin", rewriter{})
}

const (
	// dbFile is the database, relative to the app's config directory. The
	// image's JELLYFIN_DATA_DIR is /config and the file is data/jellyfin.db
	// inside it.
	dbFile = "data/jellyfin.db"
	// baseItems is the one table this package writes. Its name is the entity's
	// (BaseItemEntity.cs is mapped to BaseItems by the model builder).
	baseItems = "BaseItems"
	// pathColumn and dataColumn are the two columns it writes, named as the
	// entity's properties are.
	pathColumn = "Path"
	dataColumn = "Data"
)

// internalPrefixes are the directories inside the app that hold its own state.
// A stored path under one of them is not media: the root folder row is
// /config/root, a playlist is under %AppDataPath%, and transcodes go to the
// cache. Mapping them through a plan rule would be wrong even if a rule
// happened to cover them, so they are skipped before the plan is asked.
var internalPrefixes = []string{"/config", "/cache", "/tmp"}

type rewriter struct{}

// edit is one row's new value, keyed by the row's own id.
type edit struct {
	id    string
	value string
}

type analysis struct {
	app       string
	changes   map[string]int // "BaseItems.Path" -> count
	uncover   []plan.Uncovered
	pathEdits []edit
	dataEdits []edit
	notes     []string
}

func (r rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(in.Dir, dbFile))
	if err != nil {
		return rewrite.Report{}, err
	}
	defer db.Close()
	a, err := analyseRows(ctx, in, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

func (r rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(in.Dir, dbFile))
	if err != nil {
		return rewrite.Report{}, err
	}
	defer db.Close()
	a, err := analyseRows(ctx, in, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	// A gap means the plan does not say where a path goes. Writing the rows it
	// does say and leaving the rest would be a database that disagrees with
	// itself about where the library is, so nothing is written at all.
	if len(a.uncover) > 0 {
		return a.report(), fmt.Errorf("%s: %d stored paths have no rule in the plan; nothing was written", dbFile, len(a.uncover))
	}
	if err := a.apply(ctx, db); err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

// analyseRows reads every row once and decides what to write. It is the same
// code in both passes, so a Write can only make what a Check reported.
func analyseRows(ctx context.Context, in rewrite.Input, db *sql.DB) (*analysis, error) {
	a := &analysis{app: in.App, changes: map[string]int{}}
	rows, err := db.QueryContext(ctx, "SELECT Id, "+pathColumn+", "+dataColumn+" FROM "+baseItems)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", baseItems, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var path, data sql.NullString
		if err := rows.Scan(&id, &path, &data); err != nil {
			return nil, fmt.Errorf("read %s: %w", baseItems, err)
		}
		if path.Valid {
			if next, ok := rewritePath(in, a, baseItems+"."+pathColumn, path.String); ok {
				a.pathEdits = append(a.pathEdits, edit{id: id, value: next})
				a.changes[baseItems+"."+pathColumn]++
			}
		}
		if data.Valid && strings.TrimSpace(data.String) != "" {
			next, changed, err := rewriteJSON(in, a, baseItems+"."+dataColumn, data.String)
			if err != nil {
				return nil, fmt.Errorf("%s.%s row %s: %w", baseItems, dataColumn, id, err)
			}
			if changed {
				a.dataEdits = append(a.dataEdits, edit{id: id, value: next})
				a.changes[baseItems+"."+dataColumn]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", baseItems, err)
	}
	return a, nil
}

// rewritePath maps one stored path. ok is false when the path is not media (it
// is internal, relative or empty) or when a rule already leaves it as it is.
func rewritePath(in rewrite.Input, a *analysis, where, stored string) (string, bool) {
	if strings.TrimSpace(stored) == "" || !filepath.IsAbs(stored) {
		return stored, false
	}
	for _, prefix := range internalPrefixes {
		if stored == prefix || strings.HasPrefix(stored, prefix+"/") {
			return stored, false
		}
	}
	next, covered := in.Plan.Rewrite(in.App, stored)
	if !covered {
		a.uncover = append(a.uncover, plan.Uncovered{App: in.App, File: dbFile, Key: where, Path: stored})
		return stored, false
	}
	if next == stored {
		return stored, false
	}
	return next, true
}

// rewriteJSON walks a JSON document and rewrites every string that is an
// absolute path. The value's own JSON shape is kept: only the strings change,
// and the document is re-encoded with the app's own nesting, because the app is
// the only reader.
func rewriteJSON(in rewrite.Input, a *analysis, where, document string) (string, bool, error) {
	var doc any
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		// A column that is not JSON is not this rewriter's business; the app
		// reads it as data it wrote, and nothing here can improve it.
		return document, false, nil
	}
	changed := false
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			next, ok := rewritePath(in, a, where, t)
			if ok {
				changed = true
				return next
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
			return t
		}
		return v
	}
	walk(doc)
	if !changed {
		return document, false, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return document, false, err
	}
	return string(out), true, nil
}

// report renders the pass in a stable order.
func (a *analysis) report() rewrite.Report {
	rep := rewrite.Report{App: a.app, Uncovered: a.uncover, Notes: a.notes}
	where := make([]string, 0, len(a.changes))
	for k := range a.changes {
		where = append(where, k)
	}
	sort.Strings(where)
	for _, k := range where {
		rep.Changes = append(rep.Changes, rewrite.Change{File: dbFile, Where: k, What: "path", Count: a.changes[k]})
	}
	sort.Slice(rep.Uncovered, func(i, j int) bool { return rep.Uncovered[i].String() < rep.Uncovered[j].String() })
	return rep
}

// apply writes the rows in one transaction: half a rewrite is a database whose
// items and whose library disagree about where the media is.
func (a *analysis) apply(ctx context.Context, db *sql.DB) error {
	if len(a.pathEdits)+len(a.dataEdits) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", dbFile, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to a no-op
	for _, e := range a.pathEdits {
		//nolint:gosec // the table and column are constants in this file, never a stored value
		if _, err := tx.ExecContext(ctx, "UPDATE "+baseItems+" SET "+pathColumn+" = ? WHERE Id = ?", e.value, e.id); err != nil {
			return fmt.Errorf("update %s.%s row %s: %w", baseItems, pathColumn, e.id, err)
		}
	}
	for _, e := range a.dataEdits {
		//nolint:gosec // the table and column are constants in this file, never a stored value
		if _, err := tx.ExecContext(ctx, "UPDATE "+baseItems+" SET "+dataColumn+" = ? WHERE Id = ?", e.value, e.id); err != nil {
			return fmt.Errorf("update %s.%s row %s: %w", baseItems, dataColumn, e.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", dbFile, err)
	}
	return nil
}
