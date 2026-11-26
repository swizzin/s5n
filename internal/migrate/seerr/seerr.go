// Package seerr rewrites the root folders and app connections Seerr stored. Its
// state is settings.json and db/db.sqlite3 (server/lib/settings/index.ts:396-398,
// server/datasource.ts:107-122 at v3.5.1). A root folder is a path in the Arr's
// filesystem, never one Seerr opens: a request sends it as rootFolderPath
// (server/api/servarr/radarr.ts:166,234), so each entry's connection is resolved
// first and its folders move by that Arr's table. Connections are hostname, port,
// useSsl and baseUrl (plus plex's ip and the tautulli block), all plain HTTP with
// the chart's base. applicationUrl and externalUrl are where people open the
// apps, so they are left. override_rule.rootFolder must move (OverrideRule.ts:36,
// read by MediaRequest.ts:283-298); media_request.rootFolder is history
// (MediaRequest.ts:554).
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
	// dbName is the database under the staged /config.
	dbName = "db/db.sqlite3"
	// colRootFolder is both root-folder tables' column (OverrideRule.ts:36,
	// MediaRequest.ts:554).
	colRootFolder = "rootFolder"
)

type rewriter struct{}

// Check reports what it would change, writing nothing.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// decision is what both passes work out, so Write cannot exceed its Check.
type decision struct {
	settings *jsonFile
	edits    []edit
	dbEdits  []sqlEdit
	tally    tally
	// arrs is every configured Arr by id and the connection it resolved to, which
	// turns a database folder into a path in that Arr's filesystem.
	arrs map[service]plan.Resolution
}

// service is one Arr entry as the database names it: its kind and its id
// (settings/index.ts:78,98 at v3.5.1; a rule's radarrServiceId, a request's
// serverId).
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

// planSettings is each Arr's connection and root folders, then Plex and Tautulli.
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
			// The connection first: it decides whose path table the folders use.
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

// remember files one Arr by the id the database names it, so a row can find the
// Arr whose filesystem its rootFolder belongs to.
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

// arrRootFolder moves one root folder by that Arr's table. Seerr hands it to the
// Arr as rootFolderPath (radarr.ts:166,234 at v3.5.1). A leave, or an unclassified
// connection already reported, keeps it; settling the connection settles the
// folder next run.
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

// planArrConnection rewrites an Arr entry and says what it resolved to. `baseUrl`
// is optional in Seerr and makes an Arr served under /<name> reachable, so it is
// added when the chart has one.
func planArrConnection(in rewrite.Input, f *jsonFile, base string, d *decision, rep *rewrite.Report) (plan.Resolution, bool, error) {
	return planAddress(in, f, base, "hostname", "baseUrl", d, rep)
}

// planAddress resolves one stored connection through the plan, writing the box's
// host, port, plain HTTP and URL base. It returns what it resolved to, so a caller
// with an Arr's root folders can ask that app's table next.
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
		// Plain HTTP inside the release.
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

// setBaseURL gives a connection the chart's base, adding the optional key Seerr
// leaves out.
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

// planDB is the two root-folder columns. A row names its Arr by id, not address
// (overrideRules.ts:39-57, MediaRequest.ts:548 at v3.5.1), and a root folder is a
// path in that Arr's filesystem, so it moves by that Arr's table, never Seerr's.
// One rule holds one rootFolder for both Arrs it may name (OverrideRule.ts:14-19),
// so it is written only when every named Arr agrees, and left and called out
// otherwise. A request is history: rewritten when covered, left otherwise.
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
	// A service id column is read only when the schema has it: a schema without
	// one cannot say whose folder it is, and guessing is not an option.
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
			// Already reported; settling the connection settles the folder next run.
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
		// A folder no Arr's table covers is a gap, reported once per Arr asked.
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
			Why: "the Arrs it names disagree about it",
			Detail: fmt.Sprintf("the Arrs this override rule names do not agree about %s (%s)",
				folder, strings.Join(parts, "; ")),
		})
	}
}

// requests rewrites each completed request's folder through the Arr it went to.
// A request is history, so one no rule covers is left and counted once, not a gap.
func (d *decision) requests(ctx context.Context, db *sql.DB, in rewrite.Input, rep *rewrite.Report) error {
	const table = "media_request"
	for _, c := range []string{"type", "serverId", colRootFolder} {
		ok, err := hasColumn(ctx, db, table, c)
		if err != nil {
			return err
		}
		if !ok {
			// A schema without these cannot be mapped, and the column is history:
			// leaving it loses nothing.
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
		// One count line, not one per row: leaving history is the decision.
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
