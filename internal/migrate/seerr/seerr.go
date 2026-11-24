// Package seerr rewrites the root folders and the app connections Seerr stored.
//
// Seerr keeps its state in one directory: settings.json and, beside it,
// db/db.sqlite3 (server/lib/settings/index.ts:396-398 and
// server/datasource.ts:107-122 at v3.5.1 -- SQLite unless DB_* env points it at
// Postgres, which a box never does).
//
// In settings.json the paths are the Arr root folders a request is added to:
// `radarr[].activeDirectory`, `sonarr[].activeDirectory` and
// `sonarr[].activeAnimeDirectory` (server/lib/settings/index.ts:78, :98). They
// are not decorative: a request sends one to the Arr as `rootFolderPath`
// (server/api/servarr/radarr.ts:166,234) and the scanners compare library paths
// against them. A root folder is therefore a path in the Arr's filesystem, never
// one Seerr opens, so this pass resolves each entry's connection first and maps
// its root folders through that Arr's path table. The connections are
// `hostname`, `port`, `useSsl` and `baseUrl`
// on every Arr entry, and `plex.ip`/`port`/`useSsl` and the `tautulli` block
// (:31-56, :67-78, :130-145); box serves them all on plain HTTP with its own
// URL base, so a rewritten entry takes the chart's.
//
// `main.applicationUrl`, and the `externalUrl` fields beside the connections,
// are the addresses people open those apps at, not ones Seerr dials. They belong
// to the box's edge and are left as they are.
//
// In the database two columns hold a root folder: `override_rule.rootFolder`,
// which is read for every future request and so must move
// (server/entity/OverrideRule.ts:36, used at server/entity/MediaRequest.ts:283-298),
// and `media_request.rootFolder`, which records where a past request went and
// is never read again (MediaRequest.ts:554).
package seerr

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func init() { rewrite.Register("seerr", rewriter{}) }

const (
	settingsName = "settings.json"
	// dbName is the database, relative to the staged /config.
	dbName = "db/db.sqlite3"
	// colRootFolder is the column both root-folder tables keep the folder in
	// (server/entity/OverrideRule.ts:36, MediaRequest.ts:554).
	colRootFolder = "rootFolder"
)

type rewriter struct{}

// Check reads the settings file and the database and reports what it would
// change, writing nothing.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// decision is everything both passes work out, so Write cannot make a change
// the Check before it did not report.
type decision struct {
	settings *jsonFile
	edits    []edit
	dbEdits  []sqlEdit
	tally    tally
	// arrs is every Arr entry the file configures, by the id Seerr gives it and
	// the connection that entry resolved to. The database names an Arr by that
	// id, so this is what turns a root folder in it into a path in that Arr's
	// filesystem.
	arrs map[service]plan.Resolution
}

// service is one Arr entry in settings.json as the database names it: which
// kind of Arr (sonarr or radarr) and the id the entry carries
// (server/lib/settings/index.ts:78,98 at v3.5.1; override_rule's
// radarrServiceId/sonarrServiceId and media_request's serverId are that id).
type service struct {
	kind string
	id   int
}

func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	d := decision{tally: tally{}, arrs: map[service]plan.Resolution{}}
	settingsPath := filepath.Join(in.Dir, settingsName)
	raw, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		f, err := parseJSON(raw)
		if err != nil {
			return rep, fmt.Errorf("seerr: %s: %w", settingsName, err)
		}
		d.settings = f
		if err := planSettings(in, f, &d, &rep); err != nil {
			return rep, err
		}
	case os.IsNotExist(err):
		rep.Notes = append(rep.Notes, settingsName+" is not in the staged config; nothing to rewrite in it")
	default:
		return rep, fmt.Errorf("seerr: %w", err)
	}
	if err := planDB(ctx, in, &d, &rep); err != nil {
		return rep, err
	}
	rep.Changes = d.tally.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("seerr: the plan does not settle every path and address; nothing was written")
	}
	if !write {
		return rep, nil
	}

	if len(d.edits) > 0 {
		info, err := os.Stat(settingsPath)
		if err != nil {
			return rep, fmt.Errorf("seerr: %w", err)
		}
		if err := atomicfile.Write(settingsPath, d.settings.apply(d.edits), info.Mode()); err != nil {
			return rep, fmt.Errorf("seerr: %w", err)
		}
	}
	if len(d.dbEdits) == 0 {
		return rep, nil
	}
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(in.Dir, filepath.FromSlash(dbName)))
	if err != nil {
		return rep, fmt.Errorf("seerr: %w", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("seerr: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range d.dbEdits {
		if err := e.apply(ctx, tx); err != nil {
			return rep, err
		}
	}
	if err := tx.Commit(); err != nil {
		return rep, fmt.Errorf("seerr: %w", err)
	}
	return rep, nil
}

// planSettings is settings.json: each Arr entry's connection and root folders,
// then the other two connections.
func planSettings(in rewrite.Input, f *jsonFile, d *decision, rep *rewrite.Report) error {
	for _, arr := range []struct {
		key  string
		dirs []string
	}{
		{"sonarr", []string{"activeDirectory", "activeAnimeDirectory"}},
		{"radarr", []string{"activeDirectory"}},
	} {
		for i := 0; ; i++ {
			base := "/" + arr.key + "/" + strconv.Itoa(i)
			if _, ok := f.find(base); !ok {
				break
			}
			// The connection first: which Arr the entry is decides whose path
			// table its root folders move by.
			res, found, err := planArrConnection(in, f, base, d, rep)
			if err != nil {
				return err
			}
			if found {
				d.remember(f, arr.key, base, res)
			}
			for _, name := range arr.dirs {
				where := base + "/" + name
				n, ok := f.find(where)
				if !ok || n.kind != 's' || strings.TrimSpace(n.str) == "" {
					continue
				}
				d.arrRootFolder(in, f, where, n, res, rep)
			}
		}
	}

	// Plex: an address and a port in two fields.
	if _, _, err := planAddress(in, f, "/plex", "ip", "", d, rep); err != nil {
		return err
	}
	// Tautulli, when it is configured at all.
	if _, ok := f.find("/tautulli"); ok {
		if _, _, err := planAddress(in, f, "/tautulli", "hostname", "urlBase", d, rep); err != nil {
			return err
		}
	}
	return nil
}

// remember files one Arr entry under the id the database names it by, so a row
// in override_rule or media_request can find the Arr whose filesystem its
// rootFolder belongs to.
func (d *decision) remember(f *jsonFile, kind, base string, res plan.Resolution) {
	n, ok := f.find(base + "/id")
	if !ok || n.kind != 'n' {
		return
	}
	id, err := strconv.Atoi(n.raw)
	if err != nil {
		return
	}
	d.arrs[service{kind: kind, id: id}] = res
}

// arrRootFolder moves one Arr root folder by that Arr's path table. Seerr never
// opens the directory: it hands it to the Arr as rootFolderPath
// (server/api/servarr/radarr.ts:166,234 at v3.5.1), which the Arr resolves in its
// own filesystem. A leave keeps it as it is (that Arr is outside the box), and
// an unclassified connection keeps it too: the connection is already reported,
// and settling it settles its root folder on the next run.
func (d *decision) arrRootFolder(in rewrite.Input, f *jsonFile, where string, n *jsonNode, res plan.Resolution, rep *rewrite.Report) {
	if res.Verdict != plan.Rewrite {
		return
	}
	moved, covered := in.Plan.Rewrite(res.App, n.str)
	if !covered {
		rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
			App: res.App, File: settingsName, Key: where, Path: n.str,
		})
		return
	}
	if moved == n.str {
		return
	}
	d.edits = append(d.edits, f.setString(n, moved, "path"))
	d.tally.add(settingsName, where, "path")
}

// planArrConnection rewrites one Arr entry's address, port, TLS flag and URL
// base, and says what its connection resolved to: only a rewritten entry names
// an Arr whose path table its root folders move by. `baseUrl` is optional in
// Seerr and is what makes an Arr's own prefix (the chart serves each one under
// /<name>) reachable, so it is added when the chart has one and the file does
// not. found is false for an entry that stores no address at all.
func planArrConnection(in rewrite.Input, f *jsonFile, base string, d *decision, rep *rewrite.Report) (plan.Resolution, bool, error) {
	return planAddress(in, f, base, "hostname", "baseUrl", d, rep)
}

// planAddress is the one place a stored connection is resolved: the host field
// names the host, the port field the port, and the plan settles which app it
// was. A rewritten connection takes the box's host, port, plain HTTP and the
// chart's URL base. It returns what it resolved to, so a caller with something
// else that belongs to the app it named (an Arr's root folders) can ask that
// app's table next.
func planAddress(in rewrite.Input, f *jsonFile, base, hostKey, baseKey string, d *decision, rep *rewrite.Report) (plan.Resolution, bool, error) {
	hostNode, ok := f.find(base + "/" + hostKey)
	if !ok || hostNode.kind != 's' || strings.TrimSpace(hostNode.str) == "" {
		return plan.Resolution{}, false, nil
	}
	portNode, havePort := f.find(base + "/port")
	port := 0
	if havePort && portNode.kind == 'n' {
		port, _ = strconv.Atoi(portNode.raw)
	}
	sslNode, haveSSL := f.find(base + "/useSsl")

	res := in.Resolve(hostNode.str, port)
	switch res.Verdict {
	case plan.Rewrite:
		end, ok := in.Targets[res.App]
		if !ok {
			return res, true, fmt.Errorf("seerr: %s: the plan points %s at %q, which has no endpoint on the box",
				settingsName, address(hostNode.str, port), res.App)
		}
		if hostNode.str != end.Host {
			d.edits = append(d.edits, f.setString(hostNode, end.Host, "host"))
			d.tally.add(settingsName, base+"/"+hostKey, "host")
		}
		if havePort && portNode.raw != strconv.Itoa(end.Port) {
			d.edits = append(d.edits, f.setRaw(portNode, strconv.Itoa(end.Port), "port"))
			d.tally.add(settingsName, base+"/port", "port")
		}
		// Everything box runs answers plain HTTP inside the release.
		if haveSSL && sslNode.raw != "false" {
			d.edits = append(d.edits, f.setRaw(sslNode, "false", "tls"))
			d.tally.add(settingsName, base+"/useSsl", "tls")
		}
		d.setBaseURL(f, base, baseKey, end)
	case plan.Leave:
	default:
		rep.Unclassified = append(rep.Unclassified, rewrite.Address{
			App: in.App, File: settingsName, Where: base + "/" + hostKey,
			Host: hostNode.str, Port: port,
		})
	}
	return res, true, nil
}

// setBaseURL gives one connection the chart's URL base, adding the key when the
// file does not have it: Seerr's baseUrl is optional and an Arr served under
// /sonarr is unreachable without it.
func (d *decision) setBaseURL(f *jsonFile, base, baseKey string, end rewrite.Endpoint) {
	if baseKey == "" {
		return
	}
	where := base + "/" + baseKey
	if n, ok := f.find(where); ok {
		if n.str == end.URLBase {
			return
		}
		d.edits = append(d.edits, f.setString(n, end.URLBase, "url base"))
		d.tally.add(settingsName, where, "url base")
		return
	}
	if end.URLBase == "" {
		return
	}
	obj, ok := f.find(base)
	if !ok {
		return
	}
	enc, _ := json.Marshal(end.URLBase)
	if e, ok := f.addMember(obj, baseKey, string(enc), "url base"); ok {
		d.edits = append(d.edits, e)
		d.tally.add(settingsName, where, "url base")
	}
}

// sqlEdit is one statement the write pass runs inside the transaction.
type sqlEdit interface {
	apply(ctx context.Context, tx *sql.Tx) error
}

// dbEdit is one root-folder column set to the path the rewrite chose.
type dbEdit struct {
	table, column string
	id            int64
	value         string
}

func (e dbEdit) apply(ctx context.Context, tx *sql.Tx) error {
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, e.table, e.column), e.value, e.id)
	if err != nil {
		return fmt.Errorf("seerr: %s.%s: %w", e.table, e.column, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("seerr: %s.%s: updated %d rows, want 1", e.table, e.column, n)
	}
	return nil
}

// planDB is the two root-folder columns.
//
// These rows name their Arr by id, not by address: Seerr selects an override
// rule by radarrServiceId or sonarrServiceId (server/lib/overrideRules.ts:39-57
// at v3.5.1: the `id` of a `radarr[]` or `sonarr[]` entry), and a request
// records the entry it went to in `serverId` (server/entity/MediaRequest.ts:548).
// A root folder is a path in *that Arr's* filesystem, so it moves by that Arr's
// path table and never by Seerr's, which holds paths Seerr itself opens.
//
// One rule holds a single rootFolder for both Arrs it may name
// (server/entity/OverrideRule.ts:14-19,36), so it is written only when every Arr
// the rule names gives the same path; otherwise the column cannot be right for
// both and the rule is left as it is, called out. A request records where it was
// sent and is never read again, so it is rewritten when its Arr's table covers it
// and is otherwise history.
func planDB(ctx context.Context, in rewrite.Input, d *decision, rep *rewrite.Report) error {
	path := filepath.Join(in.Dir, filepath.FromSlash(dbName))
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, dbName+" is not in the staged config; nothing to rewrite in it")
			return nil
		}
		return fmt.Errorf("seerr: %w", err)
	}
	db, err := sqlitedb.OpenReadOnly(ctx, path)
	if err != nil {
		return fmt.Errorf("seerr: %w", err)
	}
	defer db.Close()

	if err := d.overrideRules(ctx, db, in, rep); err != nil {
		return err
	}
	return d.requests(ctx, db, in, rep)
}

// overrideRules moves each override rule's folder through the Arrs the rule
// names.
func (d *decision) overrideRules(ctx context.Context, db *sql.DB, in rewrite.Input, rep *rewrite.Report) error {
	const table = "override_rule"
	ok, err := hasColumn(ctx, db, table, colRootFolder)
	if err != nil || !ok {
		return err
	}
	// Each service id is read only when the schema has it. A schema without
	// them cannot say which filesystem the folder belongs to, and that is a
	// fact about the row, not a reason to guess.
	radarrCol, err := hasColumn(ctx, db, table, "radarrServiceId")
	if err != nil {
		return err
	}
	sonarrCol, err := hasColumn(ctx, db, table, "sonarrServiceId")
	if err != nil {
		return err
	}
	columns := "id, rootFolder"
	if radarrCol {
		columns += ", radarrServiceId"
	}
	if sonarrCol {
		columns += ", sonarrServiceId"
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE rootFolder IS NOT NULL AND rootFolder <> ''`, columns, table))
	if err != nil {
		return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
	}
	type row struct {
		id     int64
		folder string
		radarr sql.NullInt64
		sonarr sql.NullInt64
	}
	var found []row
	for rows.Next() {
		var r row
		switch {
		case radarrCol && sonarrCol:
			err = rows.Scan(&r.id, &r.folder, &r.radarr, &r.sonarr)
		case radarrCol:
			err = rows.Scan(&r.id, &r.folder, &r.radarr)
		case sonarrCol:
			err = rows.Scan(&r.id, &r.folder, &r.sonarr)
		default:
			err = rows.Scan(&r.id, &r.folder)
		}
		if err != nil {
			rows.Close()
			return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
		}
		found = append(found, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
	}
	for _, r := range found {
		var named []service
		if r.radarr.Valid {
			named = append(named, service{kind: "radarr", id: int(r.radarr.Int64)})
		}
		if r.sonarr.Valid {
			named = append(named, service{kind: "sonarr", id: int(r.sonarr.Int64)})
		}
		d.overrideRule(in, r.id, r.folder, named, rep)
	}
	return nil
}

// overrideRule settles one rule's folder through every Arr the rule names. It
// writes the path only when the Arrs agree; otherwise the row is left exactly as
// it is and the report says what each Arr makes of the folder.
func (d *decision) overrideRule(in rewrite.Input, id int64, folder string, named []service, rep *rewrite.Report) {
	const where = "override_rule.rootFolder"
	if len(named) == 0 {
		d.left(rep, rewrite.Left{
			Where: where, What: folder,
			Why:    "it names no Arr",
			Detail: fmt.Sprintf("this override rule names no Arr, so box cannot tell whose folder %s is", folder),
		})
		return
	}
	var parts []string
	paths := map[string]bool{}
	var gaps []plan.Uncovered
	unsettled := false
	for _, svc := range named {
		res, configured := d.arrs[svc]
		switch {
		case !configured:
			parts = append(parts, fmt.Sprintf("%s #%d is not configured", svc.kind, svc.id))
		case res.Verdict == plan.Unclassified:
			// The connection is already reported, and settling it settles
			// this folder on the next run.
			unsettled = true
		case res.Verdict != plan.Rewrite:
			parts = append(parts, svc.kind+" is not on this box")
		default:
			moved, covered := in.Plan.Rewrite(res.App, folder)
			if !covered {
				gaps = append(gaps, plan.Uncovered{App: res.App, File: dbName, Key: where, Path: folder})
				continue
			}
			paths[moved] = true
			parts = append(parts, svc.kind+" gives "+moved)
		}
	}
	if unsettled {
		return
	}
	switch {
	case len(gaps) > 0:
		// A folder no rule in one Arr's table covers is a gap in the plan, not
		// a decision: it is reported once per Arr whose table was asked.
		rep.Uncovered = append(rep.Uncovered, gaps...)
	case len(paths) == 1 && len(parts) == len(named):
		moved := ""
		for p := range paths {
			moved = p
		}
		if moved == folder {
			return
		}
		d.dbEdits = append(d.dbEdits, dbEdit{table: "override_rule", column: colRootFolder, id: id, value: moved})
		d.tally.add(dbName, where, "path")
	default:
		d.left(rep, rewrite.Left{
			Where: where, What: folder,
			Why:    "the Arrs it names disagree about it",
			Detail: fmt.Sprintf("the Arrs this override rule names do not agree about %s (%s)",
				folder, strings.Join(parts, "; ")),
		})
	}
}

// requests rewrites the root folder each completed request records, through the
// Arr it was sent to. A request is history: Seerr records where it went and
// never reads the folder again, so one no rule covers is left exactly as it is,
// counted once in the report rather than named a gap.
func (d *decision) requests(ctx context.Context, db *sql.DB, in rewrite.Input, rep *rewrite.Report) error {
	const table = "media_request"
	for _, c := range []string{"type", "serverId", colRootFolder} {
		ok, err := hasColumn(ctx, db, table, c)
		if err != nil {
			return err
		}
		if !ok {
			// A schema that does not record where a request went cannot be
			// mapped, and this column is history: nothing is lost by leaving
			// it.
			return nil
		}
	}
	history := 0
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, type, serverId, rootFolder FROM %s WHERE rootFolder IS NOT NULL AND rootFolder <> ''`, table))
	if err != nil {
		return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
	}
	type row struct {
		id     int64
		kind   sql.NullString
		server sql.NullInt64
		folder string
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.server, &r.folder); err != nil {
			rows.Close()
			return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
		}
		found = append(found, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("seerr: read %s.rootFolder: %w", table, err)
	}
	for _, r := range found {
		var kind string
		switch r.kind.String {
		case "movie":
			kind = "radarr"
		case "tv":
			kind = "sonarr"
		default:
			continue
		}
		if !r.server.Valid {
			continue
		}
		res, configured := d.arrs[service{kind: kind, id: int(r.server.Int64)}]
		if !configured || res.Verdict != plan.Rewrite {
			continue
		}
		moved, covered := in.Plan.Rewrite(res.App, r.folder)
		if !covered {
			history++
			continue
		}
		if moved == r.folder {
			continue
		}
		d.dbEdits = append(d.dbEdits, dbEdit{table: table, column: colRootFolder, id: r.id, value: moved})
		d.tally.add(dbName, table+".rootFolder", "path")
	}
	if history > 0 {
		// One count line, not one per row: these are folders Seerr recorded and
		// never reads again, so leaving them is the decision rather than a gap
		// in the plan.
		rep.Left = append(rep.Left, rewrite.Left{
			File: dbName, Where: table + "." + colRootFolder, What: pastRequests(history),
			Why:    "history: seerr never reads it again",
			Detail: "their folders are history: seerr recorded where each request went and never reads the folder again, so the paths stay as they are",
		})
	}
	return nil
}

// pastRequests is the count of past requests as a line an operator reads.
func pastRequests(n int) string {
	if n == 1 {
		return "1 past request"
	}
	return fmt.Sprintf("%d past requests", n)
}

// left records one row this pass leaves exactly as it is.
func (d *decision) left(rep *rewrite.Report, l rewrite.Left) {
	l.File = dbName
	rep.Left = append(rep.Left, l)
}

// hasColumn reports whether a table exists with a column of that name.
func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("seerr: read %s schema: %w", table, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
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
