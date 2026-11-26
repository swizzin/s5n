// Package autobrr rewrites the paths its actions hold and the connections it
// keeps to the download client and the Arrs. Its state is config.toml and
// autobrr.db under /config (internal/database/database.go at v1.90.0). The
// database columns are the app's own: the action path columns
// (internal/domain/action.go:17-24) and client.host, port and tls for every
// download client and every Arr, which is a client row here
// (internal/domain/downloader.go at v1.91.1). A client autobrr cannot reach is an
// action that silently never runs. The path columns are macro-expanded per
// release (action.go:68), so only the absolute prefix before the first `{{` is
// rewritten.
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
	// bindAddr is what a pod needs; autobrr's 127.0.0.1 default answers the pod
	// itself (internal/config/config.go:171).
	bindAddr = "0.0.0.0"
	// tableClient holds both a download client and an Arr (0_base_schema.sql at
	// v1.90.0).
	tableClient = "client"
	keyPort     = "port"

	// tableUsers is autobrr's account table (0_base_schema.sql:1-9 at v1.90.0),
	// exactly one row; keyUsername is its name column.
	tableUsers  = "users"
	keyUsername = "username"
)

// hostPlusPort are the types dialled at host plus the port column
// (downloader.go:234-242 at v1.90.0, BuildLegacyHost).
var hostPlusPort = map[string]bool{"QBITTORRENT": true, "TRANSMISSION": true}

// typesBoxRuns are the types this box runs (charts/s5n/catalog.yaml). Any other
// row is left and called out: rewriting it would point autobrr at an app box does
// not run, and deleting it is the operator's call.
var typesBoxRuns = map[string]bool{"QBITTORRENT": true, "SONARR": true, "RADARR": true, "LIDARR": true}

// actionPaths are the macro-capable path columns (action.go:17-24).
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

	// logPath is a path only when set; unset means stdout (config.go:532-533).
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

	// baseUrl keeps its trailing slash: routers are mounted at the base's string
	// plus the rest (internal/http/server.go:300-302 at v1.90.0), so "/autobrr"
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
			// A commented-out setting is not one: the chart's value is added.
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
	// Legacy mode rewrites URLs for a proxy that strips the prefix, and nothing
	// strips here; with the chart's baseUrl it would build the wrong links.
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

// planDB is every client's address, every action's path, and the one account.
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
	edits = append(edits, actionEdits...)
	accountEdits, err := planAccounts(ctx, db, rep)
	if err != nil {
		return nil, err
	}
	return append(edits, accountEdits...), nil
}

// planAccounts makes the source's one account box's own. autobrr allows exactly
// one (internal/user/service.go:51 at v1.90.0), and the chart's init signs in as
// s5n.wiringAccount: a row with another name leaves its change-password looking
// for a name the table lacks, and the pod never starts. The id is kept; nothing
// in the schema refers to it (api_key and sessions are keyed by their own values).
// Two rows is not a source this migration understands, so the run stops naming
// the count.
func planAccounts(ctx context.Context, db *sql.DB, rep *rewrite.Report) ([]dbEdit, error) {
	empty, err := tableEmpty(ctx, db, tableUsers)
	if err != nil {
		return nil, err
	}
	if empty {
		// No table or no row: the init creates the account on the first start.
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, username FROM `+tableUsers+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("autobrr: read %s: %w", tableUsers, err)
	}
	defer rows.Close()
	type account struct {
		id   int64
		name string
	}
	var found []account
	for rows.Next() {
		var a account
		if err := rows.Scan(&a.id, &a.name); err != nil {
			return nil, fmt.Errorf("autobrr: read %s: %w", tableUsers, err)
		}
		found = append(found, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("autobrr: read %s: %w", tableUsers, err)
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("autobrr: %s %s holds %d accounts, and autobrr allows one: box owns the account %q, and which of these a source meant is not something this migration can decide",
			dbName, tableUsers, len(found), rewrite.WiringAccount)
	}
	if found[0].name == rewrite.WiringAccount {
		return nil, nil
	}
	rep.Accounts = append(rep.Accounts, rewrite.Account{From: found[0].name, To: rewrite.WiringAccount})
	return []dbEdit{{tableUsers, keyUsername, found[0].id, rewrite.WiringAccount}}, nil
}

// tableEmpty reports whether a table exists and holds no row: migrations run at
// the app's next start, so an older source simply has no table yet.
func tableEmpty(ctx context.Context, db *sql.DB, table string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
	if err != nil {
		return false, fmt.Errorf("autobrr: read %s schema: %w", table, err)
	}
	exists := rows.Next()
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("autobrr: read %s schema: %w", table, err)
	}
	rows.Close()
	if !exists {
		return true, nil
	}
	rows, err = db.QueryContext(ctx, `SELECT 1 FROM `+table+` LIMIT 1`)
	if err != nil {
		return false, fmt.Errorf("autobrr: read %s: %w", table, err)
	}
	defer rows.Close()
	found := rows.Next()
	return !found, rows.Err()
}

// planClients rewrites the download clients and the Arr rows. qBittorrent and
// Transmission are dialled at host plus the port column
// (downloader.go:234-281), while an Arr row is dialled by its host column alone as
// a whole URL (internal/downloader/service.go:514-515, pkg/arr/sonarr/client.go:
// 19-24), its port column left at whatever the form wrote. Two rows are left and
// called out rather than refused: a type box does not run, and a switched-off
// client nothing dials, whose unsettled address is not a gap.
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
		// An Arr is dialled by its host URL alone; anything else is host and port.
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
		// An Arr's URL path settles it on a host of paths; a client stores host
		// and port, which is all its rule needs.
		res := in.Resolve(clientName, p)
		if arr {
			if u, err := url.Parse(strings.TrimSpace(host)); err == nil {
				res = in.ResolveURL(u)
			}
		}
		// A type box does not run, and a switched-off client nothing dials: left
		// and called out, because neither is a gap in the plan.
		off := enabled.Valid && enabled.Int64 == 0
		switch {
		case !typesBoxRuns[strings.ToUpper(kind.String)]:
			rep.Left = append(rep.Left, rewrite.Left{
				File: dbName, Where: where, What: address(clientName, p),
				Why: fmt.Sprintf("%s client: this box runs qBittorrent and the Arrs", kind.String),
				Detail: fmt.Sprintf("%q is a %s client and this box runs only qBittorrent and the Arrs, so a rewrite would point autobrr at an app that is not there; delete it in autobrr if you do not need it",
					label, kind.String),
			})
			continue
		case res.Verdict == plan.Unclassified && off:
			rep.Left = append(rep.Left, rewrite.Left{
				File: dbName, Where: where, What: address(clientName, p),
				Why: "switched off, so nothing dials it",
				Detail: fmt.Sprintf("%q is switched off, so %s is never dialled and nothing is migrated to it",
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
				// Still the URL autobrr dials: box's scheme, Service, port and base.
				want = end.URL()
			}
			if host != want {
				edits = append(edits, dbEdit{tableClient, "host", id, want})
				t.add(dbName, tableClient+".host", "host")
			}
			if !arr && p != end.Port {
				edits = append(edits, dbEdit{tableClient, keyPort, id, end.Port})
				t.add(dbName, tableClient+"."+keyPort, "port")
			}
			// Plain HTTP inside the release.
			if tls.Int64 != 0 {
				edits = append(edits, dbEdit{tableClient, "tls", id, 0})
				t.add(dbName, tableClient+".tls", "tls")
			}
		case plan.Leave:
		default:
			rep.Unclassified = append(rep.Unclassified, rewrite.Address{
				App: in.App, File: dbName, Where: where, Host: clientName, Port: p,
			})
			// The type says what the address can only mean; with two instances
			// nothing is proposed.
			app, ok := clientApp(strings.ToUpper(kind.String))
			if _, sole := in.Sole(app); !ok || !sole {
				continue
			}
			rep.Defaults = append(rep.Defaults, rewrite.Default{
				Host: clientName, Port: p, App: app,
				Why: fmt.Sprintf("it is a %s client, and this box runs one %s", kind.String, app),
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

// rewriteTemplate rewrites the absolute prefix before the first macro: the text
// around a macro is the handler's own (action.go:68), and a value not starting
// with a slash is relative to the save path.
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

// The catalog names a client row's type maps to.
const (
	appQBittorrent = "qbittorrent"
	appSonarr      = "sonarr"
	appRadarr      = "radarr"
	appLidarr      = "lidarr"
	appProwlarr    = "prowlarr"
)

// clientApp is the app a type names; a type box does not run names none, which is
// the caller's Left case.
func clientApp(kind string) (string, bool) {
	switch kind {
	case "QBITTORRENT", "QBITTORRENTEE":
		return appQBittorrent, true
	case "SONARR":
		return appSonarr, true
	case "RADARR":
		return appRadarr, true
	case "LIDARR":
		return appLidarr, true
	case "PROWLARR":
		return appProwlarr, true
	}
	return "", false
}

// splitAddress separates a stored host from an optional port; a host holding a URL
// is the exception, and its port is the scheme's (rewrite.URLPort).
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

// address is host:port, or the host alone when no port was stored.
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
