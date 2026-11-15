// Package maintainerr rewrites the connections Maintainerr stored for the
// other apps on the box.
//
// Maintainerr keeps no filesystem path of its own. Everything it knows is in
// one SQLite database at the root of the directory DATA_DIR moves to /config
// (apps/server/src/app/config/dataDir.ts and typeOrmConfig.ts at v3.32.0), and
// the columns that name a place are connections: Plex's host name and port, the
// Seerr, Tautulli and download-client URLs, and one URL per Sonarr and Radarr
// (apps/server/src/modules/settings/entities/settings.entities.ts and
// radarr_settings.entities.ts / sonarr_settings.entities.ts at v3.32.0). A rule
// that cannot reach the app it calls silently never runs, so every one of them
// is rewritten through the plan's host rules.
//
// The columns keep TypeORM's default names, which are the property names
// unchanged (`plex_hostname`, `plex_port`, `seerr_url`, `url`) even where the
// rest of the table is snake_case, and the tables are the class names
// snake_cased (`settings`, `radarr_settings`, `sonarr_settings`). Which columns
// exist depends on the version the source ran, so each read asks the file's own
// schema rather than assuming one.
package maintainerr

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

func init() { rewrite.Register("maintainerr", rewriter{}) }

// dbName is Maintainerr's database, at the root of DATA_DIR
// (apps/server/src/app/config/dataDir.ts at v3.32.0).
const dbName = "maintainerr.sqlite"

// settingsURLs are the settings columns holding a URL another app answers on.
// `applicationUrl` is deliberately absent: it is the address people open
// Maintainerr at, not one it dials, so it follows the box's edge and not the
// plan's app table.
var settingsURLs = []string{"seerr_url", "tautulli_url", "download_client_url"}

// settingsPlex is the Plex connection, stored apart because it is a host name
// and a port rather than a URL.
const (
	colPlexHost = "plex_hostname"
	colPlexPort = "plex_port"
	colPlexSSL  = "plex_ssl"
)

// settingsExternal are the media servers box does not migrate, listed in a note
// when they are set: they point outside the release, and leaving them alone is
// the operator's call, not a rewrite gone wrong.
var settingsExternal = []string{"jellyfin_url", "emby_url", "ombi_url", "streamystats_url", "tracearr_url"}

// arrTables are the Arr connections: one table per Arr, each row a server with
// a URL.
var arrTables = []string{"radarr_settings", "sonarr_settings"}

type rewriter struct{}

// Check reads the database and reports every connection it would rewrite.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// run is both passes: gather the edits, then (for Write only) apply them. One
// code path is what keeps the two reports identical when nothing changed
// between them.
func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	path := filepath.Join(in.Dir, dbName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, dbName+" is not in the staged config; nothing to rewrite in it")
			return rep, nil
		}
		return rep, fmt.Errorf("maintainerr: %w", err)
	}
	// Check reads a copy: OpenReadOnly cannot write, so a read that would
	// recover a journal fails instead of changing what was checked.
	db, err := open(ctx, path, write)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	t := tally{}
	edits, err := settingsEdits(ctx, db, in, &rep, t)
	if err != nil {
		return rep, err
	}
	arr, err := arrEdits(ctx, db, in, &rep, t)
	if err != nil {
		return rep, err
	}
	edits = append(edits, arr...)
	rep.Changes = t.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("maintainerr: the plan does not settle every connection; nothing was written")
	}
	if !write || len(edits) == 0 {
		return rep, nil
	}
	for _, e := range edits {
		if err := e.apply(ctx, db); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func open(ctx context.Context, path string, write bool) (*sql.DB, error) {
	if write {
		return sqlitedb.OpenReadWrite(ctx, path)
	}
	return sqlitedb.OpenReadOnly(ctx, path)
}

// edit is one column set back to the value the rewrite chose.
type edit struct {
	table, column string
	id            int64
	value         string
}

func (e edit) apply(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, e.table, e.column), e.value, e.id)
	if err != nil {
		return fmt.Errorf("maintainerr: %s.%s: %w", e.table, e.column, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("maintainerr: %s.%s: updated %d rows, want 1", e.table, e.column, n)
	}
	return nil
}

// settingsEdits is the one settings row: Plex's host and port, the URLs of the
// apps it calls, and the note for the media servers box does not migrate.
func settingsEdits(ctx context.Context, db *sql.DB, in rewrite.Input, rep *rewrite.Report, t tally) ([]edit, error) {
	row, err := readSettings(ctx, db, t)
	if err != nil || row == nil {
		return nil, err
	}
	if err := row.plex(in, rep); err != nil {
		return nil, err
	}
	if err := row.urls(in, rep); err != nil {
		return nil, err
	}
	if len(row.external) > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%s store media servers box does not migrate; they are left as stored",
			strings.Join(row.external, ", ")))
	}
	return row.edits, nil
}

// settingsRow is the app's single settings row, the columns of it this package
// knows about, and the edits decided so far.
type settingsRow struct {
	id       int64
	by       map[string]sql.NullString
	external []string
	edits    []edit
	tally    tally
}

// readSettings is the row, with the columns the file actually has: which ones
// exist depends on the version the source ran, and a rewriter that assumed the
// newest schema would refuse an older install.
func readSettings(ctx context.Context, db *sql.DB, t tally) (*settingsRow, error) {
	columns, err := columnsOf(ctx, db, "settings")
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, nil
	}
	var wanted []string
	for _, c := range append([]string{colPlexHost, colPlexPort, colPlexSSL}, settingsURLs...) {
		if columns[c] {
			wanted = append(wanted, c)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	row := &settingsRow{by: map[string]sql.NullString{}, tally: t}
	for _, c := range settingsExternal {
		if columns[c] {
			row.external = append(row.external, c)
		}
	}

	query := fmt.Sprintf(`SELECT id, %s FROM settings LIMIT 1`, strings.Join(wanted, ", "))
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("maintainerr: read settings: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	values := make([]sql.NullString, len(wanted))
	dest := []any{&row.id}
	for i := range values {
		dest = append(dest, &values[i])
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf("maintainerr: read settings: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, c := range wanted {
		row.by[c] = values[i]
	}
	return row, nil
}

// set is one column's new value. The edit and its count are added only when the
// column does not already hold it: a second pass over a copy the first one wrote
// has nothing to report, which is what makes Write's report equal Check's.
func (r *settingsRow) set(column, value, what string, t tally) {
	if cur, ok := r.by[column]; ok && cur.Valid && cur.String == value {
		return
	}
	r.by[column] = sql.NullString{String: value, Valid: true}
	r.edits = append(r.edits, edit{table: "settings", column: column, id: r.id, value: value})
	t.add("settings."+column, what)
}

// plex is the Plex connection: a host and a port in two columns, which move
// together or not at all, and the TLS flag that stops applying on the box.
func (r *settingsRow) plex(in rewrite.Input, rep *rewrite.Report) error {
	host, ok := r.by[colPlexHost]
	if !ok || strings.TrimSpace(host.String) == "" {
		return nil
	}
	port, _ := strconv.Atoi(strings.TrimSpace(r.by[colPlexPort].String))
	end, rewriteIt, err := ask(in, dbName, "settings."+colPlexHost, rep, host.String, port)
	if err != nil || !rewriteIt {
		return err
	}
	r.set(colPlexHost, end.Host, "host", r.tally)
	if _, ok := r.by[colPlexPort]; ok {
		r.set(colPlexPort, strconv.Itoa(end.Port), "port", r.tally)
	}
	// Everything box runs answers plain HTTP inside the release.
	if ssl, ok := r.by[colPlexSSL]; ok && ssl.String != "0" && ssl.String != "" {
		r.set(colPlexSSL, "0", "tls", r.tally)
	}
	return nil
}

// urls is the Seerr, Tautulli and download-client addresses.
func (r *settingsRow) urls(in rewrite.Input, rep *rewrite.Report) error {
	for _, column := range settingsURLs {
		v, ok := r.by[column]
		if !ok || strings.TrimSpace(v.String) == "" {
			continue
		}
		u, err := parseURL(v.String)
		if err != nil {
			return fmt.Errorf("maintainerr: settings.%s: %w", column, err)
		}
		end, rewriteIt, err := ask(in, dbName, "settings."+column, rep, u.Hostname(), portOf(u))
		if err != nil {
			return err
		}
		if rewriteIt {
			r.set(column, endpointURL(end), "url", r.tally)
		}
	}
	return nil
}

// arrEdits is one URL per Arr server row.
func arrEdits(ctx context.Context, db *sql.DB, in rewrite.Input, rep *rewrite.Report, t tally) ([]edit, error) {
	var edits []edit
	for _, table := range arrTables {
		columns, err := columnsOf(ctx, db, table)
		if err != nil {
			return nil, err
		}
		if !columns["url"] {
			continue
		}
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, url FROM %s WHERE url IS NOT NULL AND url <> ''`, table))
		if err != nil {
			return nil, fmt.Errorf("maintainerr: read %s: %w", table, err)
		}
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("maintainerr: read %s: %w", table, err)
			}
			u, err := parseURL(raw)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("maintainerr: %s.url: %w", table, err)
			}
			end, rewriteIt, err := ask(in, dbName, table+".url", rep, u.Hostname(), portOf(u))
			if err != nil {
				rows.Close()
				return nil, err
			}
			if !rewriteIt {
				continue
			}
			if moved := endpointURL(end); moved != raw {
				edits = append(edits, edit{table: table, column: "url", id: id, value: moved})
				t.add(table+".url", "url")
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("maintainerr: read %s: %w", table, err)
		}
	}
	return edits, nil
}

// ask is the one place this app consults the plan about a stored address. It
// returns the endpoint to point at when the plan rewrites it, and records the
// address as unclassified when the plan does not settle it.
func ask(in rewrite.Input, file, where string, rep *rewrite.Report, host string, port int) (rewrite.Endpoint, bool, error) {
	res := in.Resolve(host, port)
	switch res.Verdict {
	case plan.Rewrite:
		end, ok := in.Targets[res.App]
		if !ok {
			return rewrite.Endpoint{}, false, fmt.Errorf(
				"maintainerr: %s %s: the plan points %s at %q, which has no endpoint on the box",
				file, where, address(host, port), res.App)
		}
		return end, true, nil
	case plan.Leave:
		return rewrite.Endpoint{}, false, nil
	}
	rep.Unclassified = append(rep.Unclassified, rewrite.Address{
		App: in.App, File: file, Where: where, Host: host, Port: port,
	})
	return rewrite.Endpoint{}, false, nil
}

func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// parseURL is a stored URL as the app split it. A value with no scheme is
// refused rather than guessed at: every one of these columns is written from a
// URL field in the UI.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%q is not a URL with a host", raw)
	}
	return u, nil
}

// portOf is the port a URL names, 0 when it names none (the scheme's default,
// which a plan rule with no port still matches).
func portOf(u *url.URL) int {
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return n
}

// endpointURL is one app's address on the box: plain HTTP, the Service's port,
// and the chart's URL base for that app.
func endpointURL(end rewrite.Endpoint) string {
	u := url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", end.Host, end.Port), Path: end.URLBase}
	return u.String()
}

// columnsOf is a table's columns, or nothing when the table does not exist. A
// migrated install can predate a column, and a rewriter that assumed the newest
// schema would refuse to migrate an older one.
func columnsOf(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("maintainerr: read %s schema: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("maintainerr: read %s schema: %w", table, err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// tally counts edits by file, key and kind, which is how a plan shows them.
type tally map[[3]string]int

func (t tally) add(where, what string) { t[[3]string{dbName, where, what}]++ }

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
