// Package qui rewrites the directories and connections qui stored, and drops
// the caches that a new path invalidates.
//
// qui keeps everything under /config (WORKDIR and VOLUME /config, and
// XDG_CONFIG_HOME=/config is passed straight through by GetDefaultConfigDir,
// internal/config/config.go:778-800 at v1.35.0): a config.toml, and qui.db
// beside it unless dataDir moves it (GetDatabasePath, :923-925).
//
// The database is the interesting half. qui interns repeated strings into a
// string_pool table: the columns that use it hold an integer reference
// (internal/database/migrations/010_add_files_cache_and_string_interning.sql),
// and a value is inserted once and referenced by id. So a rewritten path has to
// be interned first and the row pointed at the new id, never edited in place in
// string_pool -- the same text can be referenced from another table, and
// changing it there would rewrite that too. The app itself does exactly this
// (internal/dbinterface's InternStrings, and the pool is only ever appended to
// and cleaned when unreferenced), so a rewrite through the same two statements
// is safe.
//
// Only two of the path-bearing columns are interned: instances.host and
// arr_instances.base_url. The rest are plain TEXT: the dir-scan and orphan-scan
// settings, the external programs' paths, and the backup directories
// (053_add_dir_scan.sql:35-36, 037_add_orphan_scan.sql:10, 011_add_external_programs.sql:7-11,
// 009_add_instance_backups.sql:15, 040_move_hardlink_to_instance.sql). The scan
// caches and run rows are rows a scan rebuilds: they are dropped and counted,
// which is what makes the rewritten directories consistent instead of matching
// a cache of files found under the old ones.
package qui

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

func init() { rewrite.Register("qui", rewriter{}) }

const (
	configName = "config.toml"
	dbName     = "qui.db"
	// bindAddr is what a pod needs; qui's default is localhost, which inside a
	// pod answers nobody but itself (internal/config/config.go:356).
	bindAddr = "0.0.0.0"
)

// configPaths are config.toml's path keys. backupDir is the one the research
// names (internal/config/config.go:937-950); dataDir and logPath are the same
// kind of value and are rewritten so a box does not inherit a host directory
// the seedbox does not have.
var configPaths = []string{"backupDir", "dataDir", "logPath"}

// scanCaches are the tables a scan rebuilds. They are emptied rather than
// rewritten: a cache of files found under a directory that moved is wrong even
// when every path in it is rewritten, and qui's own rescan is the fix.
// Order matters: children before the runs they point at.
var scanCaches = []string{
	"dir_scan_run_injections",
	"dir_scan_files",
	"dir_scan_runs",
	"orphan_scan_files",
	"orphan_scan_runs",
	"instance_backup_items",
	"instance_backup_runs",
}

type rewriter struct{}

// Check reads both files and reports what it would change, writing nothing.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// work is everything both passes decide, kept in one place so Write cannot
// drift from the Check before it.
type work struct {
	tomlEdits []tomlEdit
	dbEdits   []sqlEdit
	drops     []string
}

func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	end, ok := in.Targets[in.App]
	if !ok {
		return rep, fmt.Errorf("qui: the plan has no endpoint for %q", in.App)
	}
	t := tally{}
	w, err := decide(ctx, in, end, t, &rep)
	if err != nil {
		return rep, err
	}
	rep.Changes = t.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("qui: the plan does not settle every path and address; nothing was written")
	}
	if !write {
		return rep, nil
	}

	if len(w.tomlEdits) > 0 {
		path := filepath.Join(in.Dir, configName)
		raw, err := os.ReadFile(path)
		if err != nil {
			return rep, fmt.Errorf("qui: %w", err)
		}
		f, err := parseTOML(raw)
		if err != nil {
			return rep, fmt.Errorf("qui: %s: %w", configName, err)
		}
		out := f.apply(w.tomlEdits)
		info, err := os.Stat(path)
		if err != nil {
			return rep, fmt.Errorf("qui: %w", err)
		}
		if err := atomicfile.Write(path, out, info.Mode()); err != nil {
			return rep, fmt.Errorf("qui: %w", err)
		}
	}
	if len(w.dbEdits) == 0 && len(w.drops) == 0 {
		return rep, nil
	}

	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(in.Dir, dbName))
	if err != nil {
		return rep, fmt.Errorf("qui: %w", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("qui: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range w.dbEdits {
		if err := e.apply(ctx, tx); err != nil {
			return rep, err
		}
	}
	for _, table := range w.drops {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return rep, fmt.Errorf("qui: empty %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return rep, fmt.Errorf("qui: %w", err)
	}
	return rep, nil
}

// decide reads config.toml and qui.db and builds the work. One function for
// both passes is what makes Write's changes exactly Check's.
func decide(ctx context.Context, in rewrite.Input, end rewrite.Endpoint, t tally, rep *rewrite.Report) (work, error) {
	var w work
	edits, err := planTOML(in, end, t, rep)
	if err != nil {
		return w, err
	}
	w.tomlEdits = edits

	dbPath, err := databasePath(in, rep)
	if err != nil {
		return w, err
	}
	if dbPath == "" {
		return w, nil
	}
	db, err := sqlitedb.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return w, fmt.Errorf("qui: %w", err)
	}
	defer db.Close()

	if err := planConfigColumns(ctx, db, in, t, rep, &w); err != nil {
		return w, err
	}
	if err := planInterned(ctx, db, in, t, rep, &w); err != nil {
		return w, err
	}
	if err := planCaches(ctx, db, t, &w); err != nil {
		return w, err
	}
	return w, nil
}

// databasePath is where qui keeps qui.db: the config directory unless dataDir
// moves it. The staged copy only holds what was under the config mount, so a
// dataDir that maps outside it is refused rather than silently skipped.
func databasePath(in rewrite.Input, rep *rewrite.Report) (string, error) {
	path := filepath.Join(in.Dir, configName)
	raw, err := os.ReadFile(path)
	explicit := false
	dir := in.Dir
	if err == nil {
		if f, perr := parseTOML(raw); perr == nil {
			if v, ok := f.get("dataDir"); ok && strings.TrimSpace(v.str) != "" {
				explicit = true
				moved, covered := in.Plan.Rewrite(in.App, v.str)
				if !covered {
					rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
						App: in.App, File: configName, Key: "dataDir", Path: v.str,
					})
				} else if rel, ok := underConfig(moved); ok {
					dir = filepath.Join(in.Dir, filepath.FromSlash(rel))
				} else {
					return "", fmt.Errorf("qui: dataDir %q maps to %q, which is outside the staged /config; qui.db cannot be rewritten", v.str, moved)
				}
			}
		}
	}
	guess := filepath.Join(dir, dbName)
	if _, err := os.Stat(guess); err != nil {
		if !explicit && os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, dbName+" is not in the staged config; nothing to rewrite in it")
			return "", nil
		}
		if os.IsNotExist(err) {
			return "", fmt.Errorf("qui: %s holds no %s, but config.toml's dataDir says it is there", dir, dbName)
		}
		return "", fmt.Errorf("qui: %w", err)
	}
	return guess, nil
}

// underConfig turns a target path into the staged file's relative name, and
// reports false for a path outside /config.
func underConfig(p string) (string, bool) {
	if p == "/config" {
		return "", true
	}
	if strings.HasPrefix(p, "/config/") {
		return strings.TrimPrefix(p, "/config/"), true
	}
	return "", false
}

// planTOML is config.toml: the log, backup and data paths, and the listen
// settings.
func planTOML(in rewrite.Input, end rewrite.Endpoint, t tally, rep *rewrite.Report) ([]tomlEdit, error) {
	path := filepath.Join(in.Dir, configName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, configName+" is not in the staged config; nothing to rewrite in it")
			return nil, nil
		}
		return nil, fmt.Errorf("qui: %w", err)
	}
	f, err := parseTOML(raw)
	if err != nil {
		return nil, fmt.Errorf("qui: %s: %w", configName, err)
	}
	var edits []tomlEdit
	for _, key := range configPaths {
		v, ok := f.get(key)
		if !ok || strings.TrimSpace(v.str) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, v.str)
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: key, Path: v.str,
			})
			continue
		}
		if moved != v.str {
			edits = append(edits, v.setString(moved))
			t.add(configName, key, "path")
		}
	}
	for _, s := range []struct {
		key   string
		value string
		isStr bool
		what  string
	}{
		{"host", bindAddr, true, "bind"},
		{"port", strconv.Itoa(end.Port), false, "port"},
		{"baseUrl", end.URLBase, true, "url base"},
	} {
		v, ok := f.get(s.key)
		switch {
		case !ok:
			// qui's template ships baseUrl commented out, and a setting that is
			// commented out is not a setting: the chart's value is added so the
			// file agrees with what the release runs.
			if s.key == "baseUrl" {
				edits = append(edits, insertString(s.key, s.value))
				t.add(configName, s.key, s.what)
			}
		case v.str != s.value:
			if s.isStr {
				edits = append(edits, v.setString(s.value))
			} else {
				edits = append(edits, v.setLiteral(s.value))
			}
			t.add(configName, s.key, s.what)
		}
	}
	return edits, nil
}

// sqlEdit is one statement the write pass runs inside the transaction.
type sqlEdit interface {
	apply(ctx context.Context, tx *sql.Tx) error
}

// dbEdit is one column set back to the value the rewrite chose.
type dbEdit struct {
	table, column string
	// where the row is: the primary key, or the whole row for a settings table
	key   string
	keyID int64
	value any
}

func (e dbEdit) apply(ctx context.Context, tx *sql.Tx) error {
	query := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s`, e.table, e.column, e.key)
	res, err := tx.ExecContext(ctx, query, e.value, e.keyID)
	if err != nil {
		return fmt.Errorf("qui: %s.%s: %w", e.table, e.column, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("qui: %s.%s: updated %d rows, want 1", e.table, e.column, n)
	}
	return nil
}

// internEdit points a row at the interned string, inserting it when the pool
// does not have it yet. The old id is left alone: another row may reference the
// same text, and qui's own cleanup removes it once nothing does.
type internEdit struct {
	table, column string
	id            int64
	old, value    string
}

func (e internEdit) apply(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO string_pool(value) VALUES (?)`, e.value); err != nil {
		return fmt.Errorf("qui: intern %q: %w", e.value, err)
	}
	res, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET %s = (SELECT id FROM string_pool WHERE value = ?) WHERE id = ?`,
		e.table, e.column), e.value, e.id)
	if err != nil {
		return fmt.Errorf("qui: %s.%s: %w", e.table, e.column, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("qui: %s.%s: updated %d rows, want 1", e.table, e.column, n)
	}
	return nil
}

// planConfigColumns rewrites the plain TEXT path columns: the dir-scan
// directories and their qBittorrent-side prefix, the orphan scanner's ignore
// list, the external programs, the backup directory and the hardlink base.
func planConfigColumns(ctx context.Context, db *sql.DB, in rewrite.Input, t tally, rep *rewrite.Report, w *work) error {
	type column struct {
		table, column     string
		key               string
		jsonList, mapping bool
	}
	columns := []column{
		{"dir_scan_directories", "path", "id", false, false},
		{"dir_scan_directories", "qbit_path_prefix", "id", false, false},
		{"orphan_scan_settings", "ignore_paths", "id", true, false},
		{"external_programs", "path", "id", false, false},
		{"external_programs", "path_mappings", "id", false, true},
		{"instance_backup_settings", "custom_path", "instance_id", false, false},
		{"instances", "hardlink_base_dir", "id", false, false},
	}
	for _, c := range columns {
		exists, err := hasColumn(ctx, db, c.table, c.column)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		rows, err := db.QueryContext(ctx, fmt.Sprintf(
			`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND %s <> ''`,
			c.key, c.column, c.table, c.column, c.column))
		if err != nil {
			return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
		}
		type row struct {
			id    int64
			value string
		}
		var found []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.value); err != nil {
				rows.Close()
				return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
			}
			found = append(found, r)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
		}
		for _, r := range found {
			where := c.table + "." + c.column
			var moved string
			var gaps []plan.Uncovered
			switch {
			case c.jsonList:
				moved, gaps = rewriteJSONList(in, where, r.value)
			case c.mapping:
				moved, gaps = rewriteMappings(in, where, r.value)
			default:
				var covered bool
				moved, covered = in.Plan.Rewrite(in.App, r.value)
				if !covered {
					gaps = append(gaps, plan.Uncovered{
						App: in.App, File: dbName, Key: where, Path: r.value,
					})
				}
			}
			rep.Uncovered = append(rep.Uncovered, gaps...)
			if len(gaps) > 0 || moved == r.value {
				continue
			}
			w.dbEdits = append(w.dbEdits, dbEdit{
				table: c.table, column: c.column, key: c.key, keyID: r.id, value: moved,
			})
			t.add(dbName, where, "path")
		}
	}
	return nil
}

// rewriteJSONList rewrites a JSON array of paths (qui stores the orphan
// scanner's ignore list and the external programs' path mappings that way).
func rewriteJSONList(in rewrite.Input, where, raw string) (string, []plan.Uncovered) {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return raw, []plan.Uncovered{{App: in.App, File: dbName, Key: where, Path: raw}}
	}
	var gaps []plan.Uncovered
	changed := false
	for i, p := range list {
		moved, covered := in.Plan.Rewrite(in.App, p)
		if !covered {
			gaps = append(gaps, plan.Uncovered{App: in.App, File: dbName, Key: where, Path: p})
			continue
		}
		if moved != p {
			list[i] = moved
			changed = true
		}
	}
	if len(gaps) > 0 || !changed {
		return raw, gaps
	}
	out, err := json.Marshal(list)
	if err != nil {
		return raw, gaps
	}
	return string(out), nil
}

// rewriteMappings rewrites both sides of every path mapping: `from` is what the
// client reports and `to` is what qui sees, and both are absolute paths
// (internal/models/external_program.go:19-22 at v1.35.0).
func rewriteMappings(in rewrite.Input, where, raw string) (string, []plan.Uncovered) {
	type mapping struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	var list []mapping
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return raw, []plan.Uncovered{{App: in.App, File: dbName, Key: where, Path: raw}}
	}
	var gaps []plan.Uncovered
	changed := false
	for i := range list {
		for _, side := range []*string{&list[i].From, &list[i].To} {
			moved, covered := in.Plan.Rewrite(in.App, *side)
			if !covered {
				gaps = append(gaps, plan.Uncovered{App: in.App, File: dbName, Key: where, Path: *side})
				continue
			}
			if moved != *side {
				*side = moved
				changed = true
			}
		}
	}
	if len(gaps) > 0 || !changed {
		return raw, gaps
	}
	out, err := json.Marshal(list)
	if err != nil {
		return raw, gaps
	}
	return string(out), nil
}

// planInterned rewrites the two interned connections: the qBittorrent instance
// URL and the Arr base URLs.
func planInterned(ctx context.Context, db *sql.DB, in rewrite.Input, t tally, rep *rewrite.Report, w *work) error {
	for _, c := range []struct{ table, column string }{
		{"instances", "host"},
		{"arr_instances", "base_url"},
	} {
		// The plain column is gone once a table is interned (010 recreates
		// these tables without it), so the reference column is what says
		// whether the table is there at all.
		poolColumn := c.column + "_id"
		if ok, err := hasColumn(ctx, db, c.table, poolColumn); err != nil {
			return err
		} else if !ok {
			continue
		}
		rows, err := db.QueryContext(ctx, fmt.Sprintf(
			`SELECT r.id, p.value FROM %s r JOIN string_pool p ON r.%s = p.id`,
			c.table, poolColumn))
		if err != nil {
			return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
		}
		type row struct {
			id    int64
			value string
		}
		var found []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.value); err != nil {
				rows.Close()
				return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
			}
			found = append(found, r)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("qui: read %s.%s: %w", c.table, c.column, err)
		}
		for _, r := range found {
			host, port, err := splitURL(r.value)
			if err != nil {
				return fmt.Errorf("qui: %s.%s: %w", c.table, c.column, err)
			}
			where := c.table + "." + c.column
			res := in.Plan.Resolve(in.App, host, port)
			switch res.Verdict {
			case plan.Rewrite:
				end, ok := in.Targets[res.App]
				if !ok {
					return fmt.Errorf("qui: %s: the plan points %s at %q, which has no endpoint on the box",
						where, address(host, port), res.App)
				}
				moved := (&url.URL{Scheme: "http",
					Host: fmt.Sprintf("%s:%d", end.Host, end.Port), Path: end.URLBase}).String()
				if moved == r.value {
					continue
				}
				w.dbEdits = append(w.dbEdits, internEdit{
					table: c.table, column: poolColumn, id: r.id, old: r.value, value: moved,
				})
				t.add(dbName, where, "url")
			case plan.Leave:
			default:
				rep.Unclassified = append(rep.Unclassified, rewrite.Address{
					App: in.App, File: dbName, Where: where, Host: host, Port: port,
				})
			}
		}
	}
	return nil
}

// planCaches empties the scan and backup-run tables, counted per table.
func planCaches(ctx context.Context, db *sql.DB, t tally, w *work) error {
	for _, table := range scanCaches {
		rows, err := db.QueryContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
		if err != nil {
			return fmt.Errorf("qui: %w", err)
		}
		var n int
		if rows.Next() {
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return fmt.Errorf("qui: %w", err)
			}
		}
		rows.Close()
		if n == 0 {
			continue
		}
		rows, err = db.QueryContext(ctx, `SELECT count(*) FROM `+table)
		if err != nil {
			return fmt.Errorf("qui: read %s: %w", table, err)
		}
		var count int
		if rows.Next() {
			if err := rows.Scan(&count); err != nil {
				rows.Close()
				return fmt.Errorf("qui: read %s: %w", table, err)
			}
		}
		rows.Close()
		if count == 0 {
			continue
		}
		w.drops = append(w.drops, table)
		t.addN(dbName, table, "dropped", count)
	}
	return nil
}

// hasColumn reports whether a table exists with a column of that name.
func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("qui: read %s schema: %w", table, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// splitURL separates the host and port a stored URL names, so the plan can
// settle the address.
func splitURL(raw string) (string, int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Hostname() == "" {
		return "", 0, fmt.Errorf("%q is not a URL with a host", raw)
	}
	n, _ := strconv.Atoi(u.Port())
	return u.Hostname(), n, nil
}

func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// tally counts edits by file, key and kind, which is how a plan shows them.
type tally map[[3]string]int

func (t tally) add(file, where, what string) {
	t[[3]string{file, where, what}]++
}

// addN counts n of one kind at once: a dropped table is one line with its row
// count, not one line per row.
func (t tally) addN(file, where, what string, n int) {
	t[[3]string{file, where, what}] += n
}

func (t tally) changes() []rewrite.Change {
	out := make([]rewrite.Change, 0, len(t))
	for k, n := range t {
		out = append(out, rewrite.Change{File: k[0], Where: k[1], What: k[2], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].What < out[j].What
	})
	return out
}
