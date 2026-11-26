// Package maintainerr rewrites the connections Maintainerr stored for the other
// apps. Its state is one SQLite database at the root of /config
// (apps/server/src/app/config/dataDir.ts and typeOrmConfig.ts at v3.32.0), and
// the columns naming a place are connections: Plex's host and port, the Seerr,
// Tautulli and download-client URLs, one URL per Arr. A rule whose address is
// wrong silently never runs. The columns keep TypeORM's property names
// (`plex_hostname`, `url`) and which exist depends on the source version, so
// each read asks the file's own schema.
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

// dbName is Maintainerr's database, at the root of DATA_DIR (dataDir.ts at
// v3.32.0).
const dbName = "maintainerr.sqlite"

// settingsURLs are the columns holding a URL another app answers on.
// `applicationUrl` is absent: people open Maintainerr there, it is not dialled.
var settingsURLs = []string{"seerr_url", "tautulli_url", colDownloadClient}

// colDownloadClient names its app, so an unsettled address can only be the box's
// qBittorrent.
const colDownloadClient = "download_client_url"

// settingsPlex is a host and a port rather than a URL, so it is stored apart.
const (
	colPlexHost = "plex_hostname"
	colPlexPort = "plex_port"
	colPlexSSL  = "plex_ssl"
	// appPlex is the app those two columns name.
	appPlex = "plex"

	// appQBittorrent is the download client's app.
	appQBittorrent = "qbittorrent"
)

// settingsExternal are media servers box does not migrate: leaving them is the
// operator's call, not a rewrite gone wrong, so they are only noted.
var settingsExternal = []string{"jellyfin_url", "emby_url", "ombi_url", "streamystats_url", "tracearr_url"}

// arrTables are the Arr connections: one table per Arr, each row a server.
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

// run is both passes: gather the edits, then (Write only) apply them, so the two
// reports stay identical.
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
	// Check reads a copy: OpenReadOnly cannot write, so a read that would recover
	// a journal fails instead of changing what was checked.
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

// settingsEdits is the one settings row: Plex, the URLs it calls, and the note
// for the media servers box does not migrate.
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

// settingsRow is the app's single settings row and the edits decided so far.
type settingsRow struct {
	id       int64
	by       map[string]sql.NullString
	external []string
	edits    []edit
	tally    tally
}

// readSettings is the row with the columns the file actually has: assuming the
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

// set is one column's new value, added only when it differs: a second pass over a
// written copy has nothing to report.
func (r *settingsRow) set(column, value, what string, t tally) {
	if cur, ok := r.by[column]; ok && cur.Valid && cur.String == value {
		return
	}
	r.by[column] = sql.NullString{String: value, Valid: true}
	r.edits = append(r.edits, edit{table: "settings", column: column, id: r.id, value: value})
	t.add("settings."+column, what)
}

// plex is the Plex connection: host and port move together or not at all, and
// TLS stops applying on the box.
func (r *settingsRow) plex(in rewrite.Input, rep *rewrite.Report) error {
	host, ok := r.by[colPlexHost]
	if !ok || strings.TrimSpace(host.String) == "" {
		return nil
	}
	port, _ := strconv.Atoi(strings.TrimSpace(r.by[colPlexPort].String))
	// These columns are Maintainerr's Plex connection by name, so an unsettled
	// address is proposed for Settle, never rewritten here.
	end, rewriteIt, err := askHost(in, dbName, "settings."+colPlexHost, rep, host.String, port, appPlex)
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

// urls is the Seerr, Tautulli and download-client addresses; a stored URL's path
// says which app it meant on a host of paths, and the box's address replaces
// both.
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
		propose := ""
		if column == colDownloadClient {
			// The download client is the box's qBittorrent by name, when there is
			// one.
			propose = appQBittorrent
		}
		end, rewriteIt, err := ask(in, dbName, "settings."+column, rep, u, propose)
		if err != nil {
			return err
		}
		if rewriteIt {
			r.set(column, end.URL(), "url", r.tally)
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
			end, rewriteIt, err := ask(in, dbName, table+".url", rep, u, "")
			if err != nil {
				rows.Close()
				return nil, err
			}
			if !rewriteIt {
				continue
			}
			if moved := end.URL(); moved != raw {
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

// ask consults the plan about a stored address, returning the endpoint when it
// rewrites and recording it as unclassified otherwise. propose names the app the
// column itself fixes (the Plex connection, the download client): an address
// nothing settles there is proposed for Settle, never rewritten here, because
// Check writes nothing and the plan is the operator's.
func ask(in rewrite.Input, file, where string, rep *rewrite.Report, u *url.URL, propose string) (rewrite.Endpoint, bool, error) {
	return askAddress(in, file, where, rep, in.ResolveURL(u), u.Hostname(), portOf(u), propose)
}

// askHost is ask for the Plex connection's host and port.
func askHost(in rewrite.Input, file, where string, rep *rewrite.Report, host string, port int, propose string) (rewrite.Endpoint, bool, error) {
	return askAddress(in, file, where, rep, in.Resolve(host, port), host, port, propose)
}

// askAddress is ask's body for a resolved address.
func askAddress(in rewrite.Input, file, where string, rep *rewrite.Report, res plan.Resolution, host string, port int, propose string) (rewrite.Endpoint, bool, error) {
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
	if propose != "" {
		// Only when the box runs exactly one: two instances cannot both be meant.
		if name, ok := in.Sole(propose); ok {
			rep.Defaults = append(rep.Defaults, rewrite.Default{
				Host: host, Port: port, App: name,
				Why: "it is Maintainerr's " + appLabel(propose) + " connection, and this box runs one " + appLabel(propose),
			})
		}
	}
	return rewrite.Endpoint{}, false, nil
}

// appLabel is how an app is named in a line an operator reads.
func appLabel(app string) string {
	if app == appPlex {
		return "Plex"
	}
	return app
}

func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// parseURL is a stored URL; one with no scheme is refused, not guessed at.
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

// portOf is the port a URL names or its scheme implies; a rule with no port
// matches any.
func portOf(u *url.URL) int {
	return rewrite.URLPort(u)
}

// columnsOf is a table's columns, or nothing when it does not exist: a migrated
// install can predate a column.
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
