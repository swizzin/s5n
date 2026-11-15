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
// against them. The connections are `hostname`, `port`, `useSsl` and `baseUrl`
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
}

func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	d := decision{tally: tally{}}
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

// planSettings is settings.json: the root folders, then every connection.
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
			for _, name := range arr.dirs {
				n, ok := f.find(base + "/" + name)
				if !ok || n.kind != 's' || strings.TrimSpace(n.str) == "" {
					continue
				}
				moved, covered := in.Plan.Rewrite(in.App, n.str)
				if !covered {
					rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
						App: in.App, File: settingsName, Key: base + "/" + name, Path: n.str,
					})
					continue
				}
				if moved != n.str {
					d.edits = append(d.edits, f.setString(n, moved, "path"))
					d.tally.add(settingsName, base+"/"+name, "path")
				}
			}
			if err := planArrConnection(in, f, base, d, rep); err != nil {
				return err
			}
		}
	}

	// Plex: an address and a port in two fields.
	if err := planAddress(in, f, "/plex", "ip", "", d, rep); err != nil {
		return err
	}
	// Tautulli, when it is configured at all.
	if _, ok := f.find("/tautulli"); ok {
		if err := planAddress(in, f, "/tautulli", "hostname", "urlBase", d, rep); err != nil {
			return err
		}
	}
	return nil
}

// planArrConnection rewrites one Arr entry's address, port, TLS flag and URL
// base. `baseUrl` is optional in Seerr and is what makes an Arr's own prefix
// (the chart serves each one under /<name>) reachable, so it is added when the
// chart has one and the file does not.
func planArrConnection(in rewrite.Input, f *jsonFile, base string, d *decision, rep *rewrite.Report) error {
	return planAddress(in, f, base, "hostname", "baseUrl", d, rep)
}

// planAddress is the one place a stored connection is resolved: the host field
// names the host, the port field the port, and the plan settles which app it
// was. A rewritten connection takes the box's host, port, plain HTTP and the
// chart's URL base.
func planAddress(in rewrite.Input, f *jsonFile, base, hostKey, baseKey string, d *decision, rep *rewrite.Report) error {
	hostNode, ok := f.find(base + "/" + hostKey)
	if !ok || hostNode.kind != 's' || strings.TrimSpace(hostNode.str) == "" {
		return nil
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
			return fmt.Errorf("seerr: %s: the plan points %s at %q, which has no endpoint on the box",
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
	return nil
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

// planDB is the two root-folder columns. A path no rule covers stops the
// migration for the override rule, because a future request would be sent to a
// directory the Arr does not have; the same gap on a completed request is
// history and is left alone.
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

	for _, c := range []struct {
		table string
		must  bool
	}{{"override_rule", true}, {"media_request", false}} {
		ok, err := hasColumn(ctx, db, c.table, "rootFolder")
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, rootFolder FROM %s WHERE rootFolder IS NOT NULL AND rootFolder <> ''`, c.table))
		if err != nil {
			return fmt.Errorf("seerr: read %s.rootFolder: %w", c.table, err)
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
				return fmt.Errorf("seerr: read %s.rootFolder: %w", c.table, err)
			}
			found = append(found, r)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("seerr: read %s.rootFolder: %w", c.table, err)
		}
		for _, r := range found {
			moved, covered := in.Plan.Rewrite(in.App, r.value)
			if !covered {
				if c.must {
					rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
						App: in.App, File: dbName, Key: c.table + ".rootFolder", Path: r.value,
					})
				}
				continue
			}
			if moved == r.value {
				continue
			}
			d.dbEdits = append(d.dbEdits, dbEdit{
				table: c.table, column: "rootFolder", id: r.id, value: moved,
			})
			d.tally.add(dbName, c.table+".rootFolder", "path")
		}
	}
	return nil
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
