// Package autobrr rewrites the paths autobrr's filters and actions hold and the
// connections it keeps to the download client and the Arrs.
//
// autobrr keeps its whole state under /config (`--config /config`, and HOME,
// XDG_CONFIG_HOME and XDG_DATA_HOME all point there in its Dockerfile): a
// config.toml and one SQLite database beside it (internal/database/database.go
// at v1.90.0: dataSourceName(cfg.ConfigPath, "autobrr.db")).
//
// The database columns are the app's own: `action.exec_cmd`, `watch_folder`,
// `save_path` and `download_path` (internal/domain/action.go:17-24;
// internal/database/migrations/sqlite/0_base_schema.sql:117-123 and
// 77_action_add_download_path.sql:2), and `client.host`, `client.port` and
// `client.tls` for every download client *and* every Arr, because an Arr is a
// client row with type SONARR or RADARR in this schema. A client autobrr cannot
// reach is an action that silently never runs, which is the failure mode this
// file exists to prevent.
//
// The path columns are macro-expanded per release before they name a file
// (internal/domain/action.go:68,123,142,146), so a value can be
// "/data/downloads/{{ .Category }}": the absolute prefix is rewritten and
// everything from the first macro on is left exactly as it was written.
package autobrr

import (
	"context"
	"database/sql"
	"fmt"
	"net"
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

func init() { rewrite.Register("autobrr", rewriter{}) }

const (
	configName = "config.toml"
	dbName     = "autobrr.db"
	// bindAddr is what a pod needs; autobrr's own default is 127.0.0.1, which
	// inside a pod answers nobody but itself (internal/config/config.go:171).
	bindAddr = "0.0.0.0"
	// tableClient is the one table both a download client and an Arr live in
	// (internal/database/migrations/sqlite/0_base_schema.sql at v1.90.0).
	tableClient = "client"
	keyPort     = "port"
)

// hostPlusPort are the client types autobrr dials at host plus the port column
// (internal/domain/downloader.go:234-242 at v1.90.0, BuildLegacyHost).
var hostPlusPort = map[string]bool{"QBITTORRENT": true, "TRANSMISSION": true}

// typesBoxRuns are the client types this box runs: qBittorrent, and the Arrs
// that are clients of autobrr in this schema (charts/s5n/catalog.yaml). A row of
// any other type (Transmission, rTorrent, Deluge, another *arr) is left exactly
// as it is and called out: rewriting it would point autobrr at an app box does
// not run, and deleting it is the operator's call, never migrate's.
var typesBoxRuns = map[string]bool{"QBITTORRENT": true, "SONARR": true, "RADARR": true}

// actionPaths are the action columns that hold a path, macro-capable
// (internal/domain/action.go:17-24).
var actionPaths = []string{"exec_cmd", "watch_folder", "save_path", "download_path"}

type rewriter struct{}

// Check reads both files and reports what it would change, writing nothing.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	end, ok := in.Targets[in.App]
	if !ok {
		return rep, fmt.Errorf("autobrr: the plan has no endpoint for %q", in.App)
	}
	t := tally{}

	tomlEdits, err := planTOML(in, end, t, &rep)
	if err != nil {
		return rep, err
	}
	dbEdits, err := planDB(ctx, in, t, &rep)
	if err != nil {
		return rep, err
	}
	rep.Changes = t.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("autobrr: the plan does not settle every path and address; nothing was written")
	}
	if !write {
		return rep, nil
	}

	if len(tomlEdits) > 0 {
		path := filepath.Join(in.Dir, configName)
		raw, err := os.ReadFile(path)
		if err != nil {
			return rep, fmt.Errorf("autobrr: %w", err)
		}
		f, err := parseTOML(raw)
		if err != nil {
			return rep, fmt.Errorf("autobrr: %s: %w", configName, err)
		}
		out := f.apply(tomlEdits)
		info, err := os.Stat(path)
		if err != nil {
			return rep, fmt.Errorf("autobrr: %w", err)
		}
		if err := atomicfile.Write(path, out, info.Mode()); err != nil {
			return rep, fmt.Errorf("autobrr: %w", err)
		}
	}

	if len(dbEdits) == 0 {
		return rep, nil
	}
	path := filepath.Join(in.Dir, dbName)
	db, err := sqlitedb.OpenReadWrite(ctx, path)
	if err != nil {
		return rep, fmt.Errorf("autobrr: %w", err)
	}
	defer db.Close()
	for _, e := range dbEdits {
		if err := e.apply(ctx, db); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// planTOML is config.toml: the log file's path and the listen settings.
func planTOML(in rewrite.Input, end rewrite.Endpoint, t tally, rep *rewrite.Report) ([]tomlEdit, error) {
	path := filepath.Join(in.Dir, configName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, configName+" is not in the staged config; nothing to rewrite in it")
			return nil, nil
		}
		return nil, fmt.Errorf("autobrr: %w", err)
	}
	f, err := parseTOML(raw)
	if err != nil {
		return nil, fmt.Errorf("autobrr: %s: %w", configName, err)
	}
	var edits []tomlEdit

	// logPath is a path when it is set; unset means stdout
	// (internal/config/config.go:532-533).
	if v, ok := f.get("logPath"); ok && strings.TrimSpace(v.str) != "" {
		moved, covered := in.Plan.Rewrite(in.App, v.str)
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: "logPath", Path: v.str,
			})
		} else if moved != v.str {
			edits = append(edits, v.setString(moved))
			t.add(configName, "logPath", "path")
		}
	}

	// Listen settings become the chart's. baseUrl keeps its trailing slash:
	// the server mounts its routers at the base URL's string plus the rest of
	// the path (internal/http/server.go:300-302 at v1.90.0), so "/autobrr"
	// would put the API at "/autobrrapi".
	base := end.URLBase
	if base != "" && !strings.HasSuffix(base, "/") {
		base += "/"
	}
	for _, s := range []struct {
		key   string
		value string
		isStr bool
		what  string
	}{
		{"host", bindAddr, true, "bind"},
		{keyPort, strconv.Itoa(end.Port), false, "port"},
		{"baseUrl", base, true, "url base"},
	} {
		v, ok := f.get(s.key)
		switch {
		case !ok:
			// A commented-out setting is not a setting: the chart's value is
			// added so the file agrees with what the release runs.
			edits = append(edits, insertString(s.key, s.value))
			t.add(configName, s.key, s.what)
		case v.str != s.value:
			if s.isStr {
				edits = append(edits, v.setString(s.value))
			} else {
				edits = append(edits, v.setLiteral(s.value))
			}
			t.add(configName, s.key, s.what)
		}
	}
	// Legacy mode rewrites the UI's URLs for a proxy that strips the prefix,
	// and nothing strips here (charts/s5n/templates/workloads.yaml sets it with
	// the base URL). A config that still has it on would build the wrong links
	// once baseUrl is the chart's.
	if v, ok := f.get("baseUrlModeLegacy"); ok && base != "" && v.str == "true" {
		edits = append(edits, v.setLiteral("false"))
		t.add(configName, "baseUrlModeLegacy", "url base mode")
	}
	return edits, nil
}

// dbEdit is one column set back to the value the rewrite chose.
type dbEdit struct {
	table, column string
	id            int64
	value         any
}

func (e dbEdit) apply(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, e.table, e.column), e.value, e.id)
	if err != nil {
		return fmt.Errorf("autobrr: %s.%s: %w", e.table, e.column, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("autobrr: %s.%s: updated %d rows, want 1", e.table, e.column, n)
	}
	return nil
}

// planDB is the database: every client's address and every action's path.
func planDB(ctx context.Context, in rewrite.Input, t tally, rep *rewrite.Report) ([]dbEdit, error) {
	path := filepath.Join(in.Dir, dbName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, dbName+" is not in the staged config; nothing to rewrite in it")
			return nil, nil
		}
		return nil, fmt.Errorf("autobrr: %w", err)
	}
	db, err := sqlitedb.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("autobrr: %w", err)
	}
	defer db.Close()

	var edits []dbEdit
	clientEdits, err := planClients(ctx, db, in, t, rep)
	if err != nil {
		return nil, err
	}
	edits = append(edits, clientEdits...)
	actionEdits, err := planActions(ctx, db, in, t, rep)
	if err != nil {
		return nil, err
	}
	return append(edits, actionEdits...), nil
}

// planClients rewrites the download clients and the Arr connections, which are
// rows of the same table.
//
// The rows are dialled from different columns. qBittorrent and Transmission
// combine host, port and tls into one URL, appending the port column to whatever
// host holds (internal/domain/downloader.go:234-281 at v1.90.0), so their port
// column is part of the address. Every other type is dialled by the host column
// alone: an Arr's config is built from Hostname: cfg.Host
// (internal/downloader/service.go:514-515) and the Arr client parses that as a
// URL and joins /api/v3 onto it (pkg/arr/sonarr/client.go:19-24), so the column
// holds a whole URL and the port column keeps whatever the form left there
// (10000), never read. Such a row is resolved on its URL's host and port,
// rewritten to a URL again, and its port column left exactly as it was.
//
// Two rows are left exactly as they are, and called out rather than refused: a
// client of a type box does not run (there is nothing in the release for it to
// talk to), and a client autobrr has switched off (it is never dialled, so an
// address nothing settles is not a gap).
func planClients(ctx context.Context, db *sql.DB, in rewrite.Input, t tally, rep *rewrite.Report) ([]dbEdit, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, type, host, port, tls, enabled FROM client`)
	if err != nil {
		return nil, fmt.Errorf("autobrr: read client: %w", err)
	}
	defer rows.Close()
	var edits []dbEdit
	for rows.Next() {
		var id int64
		var name, kind sql.NullString
		var host string
		var port, tls, enabled sql.NullInt64
		if err := rows.Scan(&id, &name, &kind, &host, &port, &tls, &enabled); err != nil {
			return nil, fmt.Errorf("autobrr: read client: %w", err)
		}
		// arr is a row dialled by its host URL alone; a bare host, or a
		// client that adds the port column to its host, is host and port.
		arr := strings.Contains(host, "://") && !hostPlusPort[strings.ToUpper(kind.String)]
		clientName, urlPort := splitAddress(host)
		p := urlPort
		if !arr {
			p = int(port.Int64)
			if p == 0 {
				p = urlPort
			}
		}
		label := name.String
		if label == "" {
			label = fmt.Sprintf("client %d", id)
		}
		where := tableClient + ".host"
		res := in.Resolve(clientName, p)
		// Two rows are left exactly as they are, and called out rather than
		// refused: a type box does not run, and a client autobrr has switched
		// off whose address nothing settles. That second one is the ruling's
		// own case: a client nothing dials is not a gap in the plan.
		off := enabled.Valid && enabled.Int64 == 0
		switch {
		case !typesBoxRuns[strings.ToUpper(kind.String)]:
			rep.Left = append(rep.Left, rewrite.Left{
				File: dbName, Where: where, What: address(clientName, p),
				Why: fmt.Sprintf("left as it is: %q is a %s client and this box runs only qBittorrent and the Arrs, so a rewrite would point autobrr at an app that is not there; delete it in autobrr if you do not need it",
					label, kind.String),
			})
			continue
		case res.Verdict == plan.Unclassified && off:
			rep.Left = append(rep.Left, rewrite.Left{
				File: dbName, Where: where, What: address(clientName, p),
				Why: fmt.Sprintf("left as it is: %q is switched off in autobrr, so %s is never dialled and nothing is migrated to it",
					label, address(clientName, p)),
			})
			continue
		}
		switch res.Verdict {
		case plan.Rewrite:
			end, ok := in.Targets[res.App]
			if !ok {
				return nil, fmt.Errorf("autobrr: client %d: the plan points %s at %q, which has no endpoint on the box",
					id, clientName, res.App)
			}
			want := end.Host
			if arr {
				// A rewritten Arr row is still the URL autobrr dials: the
				// box's scheme, Service, port and URL base.
				want = "http://" + net.JoinHostPort(end.Host, strconv.Itoa(end.Port)) + strings.TrimSuffix(end.URLBase, "/")
			}
			if host != want {
				edits = append(edits, dbEdit{tableClient, "host", id, want})
				t.add(dbName, tableClient+".host", "host")
			}
			if !arr && p != end.Port {
				edits = append(edits, dbEdit{tableClient, keyPort, id, end.Port})
				t.add(dbName, tableClient+"."+keyPort, "port")
			}
			// autobrr dials plain HTTP inside the release.
			if tls.Int64 != 0 {
				edits = append(edits, dbEdit{tableClient, "tls", id, 0})
				t.add(dbName, tableClient+".tls", "tls")
			}
		case plan.Leave:
		default:
			rep.Unclassified = append(rep.Unclassified, rewrite.Address{
				App: in.App, File: dbName, Where: where, Host: clientName, Port: p,
			})
		}
	}
	return edits, rows.Err()
}

// planActions rewrites the four macro-capable path columns.
func planActions(ctx context.Context, db *sql.DB, in rewrite.Input, t tally, rep *rewrite.Report) ([]dbEdit, error) {
	var edits []dbEdit
	for _, column := range actionPaths {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(
			`SELECT id, %s FROM action WHERE %s IS NOT NULL AND %s <> ''`, column, column, column))
		if err != nil {
			return nil, fmt.Errorf("autobrr: read action.%s: %w", column, err)
		}
		for rows.Next() {
			var id int64
			var value string
			if err := rows.Scan(&id, &value); err != nil {
				rows.Close()
				return nil, fmt.Errorf("autobrr: read action.%s: %w", column, err)
			}
			moved, gap, covered := rewriteTemplate(in, value)
			if !covered {
				rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
					App: in.App, File: dbName, Key: "action." + column, Path: gap,
				})
				continue
			}
			if moved != value {
				edits = append(edits, dbEdit{"action", column, id, moved})
				t.add(dbName, "action."+column, "path")
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("autobrr: read action.%s: %w", column, err)
		}
	}
	return edits, nil
}

// rewriteTemplate rewrites the absolute prefix of a value that may hold
// autobrr macros. The literal text around a macro is the handler's own
// (internal/domain/action.go:68), so only the part before the first macro is a
// path the plan can map; a value that does not start with a slash is relative
// to the client's save path and stays as it is.
func rewriteTemplate(in rewrite.Input, value string) (moved, gap string, covered bool) {
	if !strings.HasPrefix(value, "/") {
		return value, "", true
	}
	prefix := value
	rest := ""
	if i := strings.Index(value, "{{"); i >= 0 {
		prefix, rest = value[:i], value[i:]
	}
	moved, ok := in.Plan.Rewrite(in.App, prefix)
	if !ok {
		return value, value, false
	}
	return moved + rest, "", true
}

// splitAddress separates a stored host from an optional port: autobrr stores
// the two in their own columns, so a host holding a URL is the exception. A URL
// with no port is the port its scheme implies (rewrite.URLPort).
func splitAddress(host string) (string, int) {
	host = strings.TrimSpace(host)
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			return u.Hostname(), rewrite.URLPort(u)
		}
	}
	if h, p, ok := strings.Cut(host, ":"); ok {
		if n, err := strconv.Atoi(p); err == nil {
			return h, n
		}
	}
	return host, 0
}

// address is a host and port as one line for the operator: host:port, or the
// host alone when autobrr stored no port.
func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// tally counts edits by file, key and kind, which is how a plan shows them.
type tally map[[3]string]int

func (t tally) add(file, where, what string) { t[[3]string{file, where, what}]++ }

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
