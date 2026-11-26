// Package filebrowser rewrites the source paths FileBrowser Quantum stored, so the
// one source the chart gives it is the one its users, shares and rules point at.
// Its config.yaml is the chart's (charts/s5n/templates/workloads.yaml); its
// database is state. A source path is absolute in one place, a source's own
// `path`: user scopes, the path-keyed permission and pinned maps, declined
// defaults and a sidebar link's sourceName store it, while shares, access_rules
// and quotas store the source's name with a path relative to it
// (internal/database/sqldb at v2.1.0-stable). The chart's one source is /data,
// named "data" (pkg/settings/config.go, setupSources). A path the plan cannot
// place blocks the run unless the config no longer names it: a database-only path
// is stale, left as it is and called out.
package filebrowser

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

func init() { rewrite.Register("filebrowser", rewriter{}) }

const (
	// dbName is the database at the config root (FILEBROWSER_DATABASE_PATH, the
	// chart's server.database.path).
	dbName = "database.sqlite"
	// configName is the chart's settings file, which a source also kept sources in.
	configName = "config.yaml"
	// targetPath is the chart's one source, the data root; it names no source, so
	// the name follows the path.
	targetPath = "/data"
)

// targetName is the last part of the path, what setupSources names a sourceless
// source.
const targetName = "data"

type rewriter struct{}

// Check reads the database and reports every stored source reference it would
// rewrite.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// source is one source: the directory and its name, derived when unnamed.
type source struct {
	path string
	name string
}

// run is both passes, so the two reports stay identical.
func run(ctx context.Context, in rewrite.Input, write bool) (rep rewrite.Report, err error) {
	rep.App = in.App
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	dbPath := filepath.Join(in.Dir, dbName)
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			rep.Notes = append(rep.Notes, dbName+" is not in the staged config; nothing to rewrite in it")
			return rep, nil
		}
		return rep, fmt.Errorf("filebrowser: %w", statErr)
	}
	db, err := open(ctx, dbPath, write)
	if err != nil {
		return rep, err
	}
	defer db.Close()

	users, err := readUsers(ctx, db)
	if err != nil {
		return rep, err
	}
	tables, err := readTables(ctx, db)
	if err != nil {
		return rep, err
	}

	// The config says which sources it had and their names; one that never wrote
	// it defaulted to the working directory, leaving the database the only record.
	configured, err := configSources(filepath.Join(in.Dir, configName))
	if err != nil {
		return rep, err
	}
	if configured == nil {
		rep.Notes = append(rep.Notes,
			fmt.Sprintf("%s is not in the staged config, so the sources the database stored are the ones rewritten", configName))
	}
	m := newMapping(configured, users)

	// Settle every stored path first: a configured one the plan cannot place is a
	// gap for the operator.
	for _, p := range m.storedPaths() {
		if m.settle(in, p) {
			continue
		}
		if !m.configured[p] {
			m.stale = append(m.stale, p)
			continue
		}
		rep.Uncovered = append(rep.Uncovered, plan.Uncovered{App: in.App, File: configName, Key: "server.sources[].path", Path: p})
	}
	if len(m.stale) > 0 {
		sort.Strings(m.stale)
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%s: %s no longer configures %s, so the rows that name it are left as they are",
			dbName, plural(len(m.stale), "a stored source path", "stored source paths"), strings.Join(m.stale, ", ")))
	}

	t := newTally()
	userWrites := make([]userWrite, 0, len(users))
	for _, u := range users {
		data, changed := m.rewriteUser(u.data, t)
		if changed {
			userWrites = append(userWrites, userWrite{id: u.id, data: data})
		}
	}
	tableWrites := m.rewriteTables(tables, t)
	// The app signs users in through the box's login (catalog.yaml: `oidc:
	// true`) and resolves an identity by username, refusing a row of another login
	// method (internal/web/auth.go, getOrCreateAuthenticatedUser, at
	// v2.1.0-stable). An account the source's own form made is therefore not the
	// one the person signs in as, and which is the owner's is the operator's
	// answer: leave it and say so.
	rep.Left = append(rep.Left, leftLocalAccounts(users)...)

	rep.Changes = t.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("filebrowser: the plan does not settle every source path; nothing was written")
	}
	if !write {
		return rep, nil
	}
	if err := apply(ctx, db, userWrites, tableWrites); err != nil {
		return rep, err
	}
	return rep, nil
}

func open(ctx context.Context, path string, write bool) (*sql.DB, error) {
	if write {
		return sqlitedb.OpenReadWrite(ctx, path)
	}
	return sqlitedb.OpenReadOnly(ctx, path)
}

// mapping is what a stored value is rewritten to, by source path.
type mapping struct {
	// rel is each source's position under /data: "" for the root, "/media" for a
	// library tree.
	rel map[string]string
	// name is each source's name, by its path.
	name map[string]string
	// configured is every source path config.yaml named.
	configured map[string]bool
	// stored is every path the database refers to, which is what has to settle.
	stored map[string]bool
	// stale is every stored path the config no longer names.
	stale []string
}

// newMapping settles every source: the config's, and the database's alone.
func newMapping(configured []source, users []userRow) *mapping {
	m := &mapping{
		rel:        map[string]string{},
		name:       map[string]string{},
		configured: map[string]bool{},
		stored:     map[string]bool{},
	}
	for _, c := range configured {
		if c.path != "" {
			m.configured[c.path] = true
		}
	}
	add := func(s source) {
		if s.path == "" {
			return
		}
		if s.name == "" {
			s.name = path.Base(s.path)
		}
		if _, ok := m.name[s.path]; !ok {
			m.name[s.path] = s.name
		}
	}
	for _, s := range configured {
		add(s)
	}
	for _, p := range storedSourcePaths(users) {
		add(source{path: p})
		m.stored[p] = true
	}
	// The box's source always exists; a scope already on it is the no-op case.
	add(source{path: targetPath, name: targetName})
	m.rel[targetPath] = ""
	m.name[targetPath] = targetName
	if configured == nil {
		// No config file: each database path is a source it had, not a stale row.
		for p := range m.stored {
			m.configured[p] = true
		}
	}
	return m
}

// settle records where one source sits under the data root, through the plan.
func (m *mapping) settle(in rewrite.Input, p string) bool {
	if _, ok := m.rel[p]; ok {
		return true
	}
	if in.Plan == nil {
		return false
	}
	to, ok := in.Plan.Rewrite(in.App, p)
	if !ok {
		return false
	}
	rel, ok := underData(to)
	if !ok {
		return false
	}
	m.rel[p] = rel
	return true
}

// underData returns where a mapped path sits under the box's data root.
func underData(p string) (string, bool) {
	if p == targetPath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(p, targetPath+"/"); ok {
		return "/" + rest, true
	}
	return "", false
}

// storedPaths is every stored source path except the box's, the ones to settle.
func (m *mapping) storedPaths() []string {
	out := make([]string, 0, len(m.stored))
	for p := range m.stored {
		if p == targetPath {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

//
// The database side.
//

// userRow is one users row: its id and its JSON document.
type userRow struct {
	id   string
	data []byte
}

// namedAccount is a document's username and the login method that made it.
func namedAccount(raw []byte) (name, method string) {
	doc, err := decodeUser(raw)
	if err != nil {
		return "", ""
	}
	name, _ = doc["username"].(string)
	method, _ = doc["loginMethod"].(string)
	return name, method
}

// leftLocalAccounts is one Left per account the source's own login made, which
// the box's login will not resolve. A row with no method predates the field, and
// an oidc one is what the box finds: neither is left.
func leftLocalAccounts(users []userRow) []rewrite.Left {
	var out []rewrite.Left
	for _, u := range users {
		name, method := namedAccount(u.data)
		if method == "" || method == "oidc" {
			continue
		}
		label := fmt.Sprintf("%q (%s login)", name, method)
		out = append(out, rewrite.Left{
			File:  dbName,
			Where: "users.loginMethod",
			What:  label,
			Why:   "the box signs this app in through its login, not this app's",
			Detail: fmt.Sprintf(
				"%s belongs to the source's own login, and the box's app is a client of the box's provider: it matches an incoming identity to a stored account by name and refuses one made by another login method, so this account is not the one the person signs in as, and its scopes, shares and access rules stay with it",
				label),
		})
	}
	return out
}

// dbTables is the rows that name a source by its name and a path relative to it.
type dbTables struct {
	shares      []namedRow
	accessRules []namedRow
	quotas      []namedRow
}

// namedRow is one row: an id, a source name, and a path relative to it.
type namedRow struct {
	id     any
	source string
	path   string
}

func readUsers(ctx context.Context, db *sql.DB) ([]userRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT user_id, user_data FROM users`)
	if err != nil {
		return nil, fmt.Errorf("filebrowser: read users: %w", err)
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var u userRow
		var id string
		if err := rows.Scan(&id, &u.data); err != nil {
			return nil, fmt.Errorf("filebrowser: read users: %w", err)
		}
		u.id = id
		out = append(out, u)
	}
	return out, rows.Err()
}

// readTables reads the tables naming a source; each is optional, since the
// source's version decides what exists.
func readTables(ctx context.Context, db *sql.DB) (dbTables, error) {
	var out dbTables
	tables := []struct {
		name   string
		key    string
		string bool // the key is a TEXT column, not an INTEGER one
		dest   *[]namedRow
	}{
		{"shares", "hash", true, &out.shares},
		{"access_rules", "id", false, &out.accessRules},
		{"quotas", "id", true, &out.quotas},
	}
	for _, t := range tables {
		ok, err := hasTable(ctx, db, t.name)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT %s, source, path FROM %s`, t.key, t.name))
		if err != nil {
			return out, fmt.Errorf("filebrowser: read %s: %w", t.name, err)
		}
		for rows.Next() {
			var r namedRow
			if t.string {
				var id string
				if err := rows.Scan(&id, &r.source, &r.path); err != nil {
					rows.Close()
					return out, fmt.Errorf("filebrowser: read %s: %w", t.name, err)
				}
				r.id = id
			} else {
				var id int64
				if err := rows.Scan(&id, &r.source, &r.path); err != nil {
					rows.Close()
					return out, fmt.Errorf("filebrowser: read %s: %w", t.name, err)
				}
				r.id = id
			}
			*t.dest = append(*t.dest, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, fmt.Errorf("filebrowser: read %s: %w", t.name, err)
		}
		rows.Close()
	}
	return out, nil
}

func hasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("filebrowser: read the schema: %w", err)
	}
	return n > 0, nil
}

// userPathMaps are the document's fields whose keys are a source's path.
var userPathMaps = []string{"backendSourcePermissions", "sourcePermissions", "pinnedItems"}

// storedSourcePaths is every path-shaped reference: a scope, the permission maps'
// keys, a pinned item's source, a sidebar link's source, a declined default.
func storedSourcePaths(users []userRow) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, u := range users {
		doc, err := decodeUser(u.data)
		if err != nil {
			// A document this pass cannot read has no path to rewrite.
			continue
		}
		eachSourcePath(doc, add)
	}
	return out
}

// eachSourcePath calls add for every source path one user document holds.
func eachSourcePath(doc map[string]any, add func(string)) {
	for _, key := range userPathMaps {
		if obj, ok := doc[key].(map[string]any); ok {
			for p := range obj {
				add(p)
			}
		}
	}
	for _, p := range stringsOf(doc["declinedDefaultSources"]) {
		add(p)
	}
	for _, obj := range objects(doc["backendScopes"]) {
		if p, ok := obj["path"].(string); ok {
			add(p)
		}
	}
	for _, obj := range sidebarLinks(doc) {
		if p, ok := obj["sourceName"].(string); ok {
			add(p)
		}
	}
}

// sidebarLinks is where a document keeps its links: inside the settings overlay
// (internal/database/sqldb/users.go, NonAdminEditable.SidebarLinks at
// v2.1.0-stable), and at the top level in an older document.
func sidebarLinks(doc map[string]any) []map[string]any {
	links := objects(doc["sidebarLinks"])
	if settings, ok := doc["settings"].(map[string]any); ok {
		links = append(links, objects(settings["sidebarLinks"])...)
	}
	return links
}

// objects is a document field as the JSON objects it holds, skipping anything
// of another shape.
func objects(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

// stringsOf is a document field as the JSON strings it holds.
func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

//
// The rewrite itself.
//

// userWrite is one users row the pass changed.
type userWrite struct {
	id   string
	data []byte
}

// tableWrite is one named row the pass changed.
type tableWrite struct {
	table  string
	key    string
	id     any
	source string
	path   string
}

// rewriteUser rewrites every source-path reference in one document.
func (m *mapping) rewriteUser(raw []byte, t *tally) ([]byte, bool) {
	doc, err := decodeUser(raw)
	if err != nil {
		// A document this pass cannot read would be a guess at an unknown shape.
		return nil, false
	}
	changed := m.rewriteScopes(doc, t)
	changed = m.rewritePathMaps(doc, t) || changed
	changed = m.rewriteDeclined(doc, t) || changed
	changed = m.rewriteSidebar(doc, t) || changed
	if !changed {
		return nil, false
	}
	data, err := encodeUser(doc)
	if err != nil {
		return nil, false
	}
	return data, true
}

// rewriteScopes moves each scope onto the box's source, keeping its directory as
// a prefix.
func (m *mapping) rewriteScopes(doc map[string]any, t *tally) bool {
	changed := false
	for _, scope := range objects(doc["backendScopes"]) {
		p, _ := scope["path"].(string)
		rel, ok := m.moved(p)
		if !ok {
			continue
		}
		scope["path"] = targetPath
		scope["scope"] = joinScope(rel, stringValue(scope["scope"]))
		t.add("users.backendScopes.path", "path")
		changed = true
	}
	return changed
}

// rewritePathMaps moves the path-keyed permission and pinned maps onto the box's
// source; one already there is no change.
func (m *mapping) rewritePathMaps(doc map[string]any, t *tally) bool {
	changed := false
	for _, key := range userPathMaps {
		obj, ok := doc[key].(map[string]any)
		if !ok {
			continue
		}
		for p := range obj {
			if _, ok := m.moved(p); !ok {
				continue
			}
			obj[targetPath] = obj[p]
			delete(obj, p)
			t.add("users."+key, "source path")
			changed = true
		}
	}
	return changed
}

// rewriteDeclined moves a declined source onto the box's, which needs no decline.
func (m *mapping) rewriteDeclined(doc map[string]any, t *tally) bool {
	list, ok := doc["declinedDefaultSources"].([]any)
	if !ok {
		return false
	}
	changed := false
	for i, item := range list {
		p, _ := item.(string)
		if _, ok := m.moved(p); !ok {
			continue
		}
		list[i] = targetPath
		t.add("users.declinedDefaultSources", "source path")
		changed = true
	}
	return changed
}

// rewriteSidebar moves a path-named link, its relative target with it.
func (m *mapping) rewriteSidebar(doc map[string]any, t *tally) bool {
	changed := false
	for _, link := range sidebarLinks(doc) {
		p, _ := link["sourceName"].(string)
		rel, ok := m.moved(p)
		if !ok {
			continue
		}
		link["sourceName"] = targetPath
		link["target"] = joinScope(rel, stringValue(link["target"]))
		t.add("users.sidebarLinks", "source path")
		changed = true
	}
	return changed
}

// moved is a stored path's position under /data; false for the box's own source,
// a stale row, or one with no settle -- none of which is an edit.
func (m *mapping) moved(p string) (string, bool) {
	if p == targetPath {
		return "", false
	}
	rel, ok := m.rel[p]
	return rel, ok
}

// rewriteTables rewrites rows naming a source; a name the config no longer uses is
// a leftover, left and noted.
func (m *mapping) rewriteTables(tables dbTables, t *tally) []tableWrite {
	out := make([]tableWrite, 0)
	// The name is what matches; paths are walked in order so two sources that
	// ended with one name settle the same way twice.
	paths := make([]string, 0, len(m.name))
	for p := range m.name {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	byName := map[string]string{}
	for _, p := range paths {
		rel, ok := m.rel[p]
		if !ok || m.name[p] == targetName {
			continue
		}
		if _, dup := byName[m.name[p]]; dup {
			continue
		}
		byName[m.name[p]] = rel
	}
	rewrite := func(table string, rows []namedRow) {
		for _, r := range rows {
			rel, ok := byName[r.source]
			if !ok {
				continue
			}
			out = append(out, tableWrite{
				table: table, key: keyColumn(table), id: r.id,
				source: targetName, path: joinScope(rel, r.path),
			})
			t.add(table+".source", "source name")
			if rel != "" {
				t.add(table+".path", "path")
			}
		}
	}
	rewrite("shares", tables.shares)
	rewrite("access_rules", tables.accessRules)
	rewrite("quotas", tables.quotas)
	return out
}

func keyColumn(table string) string {
	if table == "shares" {
		return "hash"
	}
	return "id"
}

func apply(ctx context.Context, db *sql.DB, users []userWrite, tables []tableWrite) error {
	if len(users) == 0 && len(tables) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("filebrowser: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, u := range users {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET user_data = ? WHERE user_id = ?`, u.data, u.id); err != nil {
			return fmt.Errorf("filebrowser: users.user_data: %w", err)
		}
	}
	for _, w := range tables {
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET source = ?, path = ? WHERE %s = ?`, w.table, w.key),
			w.source, w.path, w.id)
		if err != nil {
			return fmt.Errorf("filebrowser: %s: %w", w.table, err)
		}
		if n, err := res.RowsAffected(); err == nil && n != 1 {
			return fmt.Errorf("filebrowser: %s: updated %d rows, want 1", w.table, n)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("filebrowser: %w", err)
	}
	return nil
}

//
// Small helpers.
//

// decodeUser keeps numbers as text: a user id is a uint64, and a float64 would
// lose its low digits.
func decodeUser(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	return doc, nil
}

func encodeUser(doc map[string]any) ([]byte, error) {
	return json.Marshal(doc)
}

// stringValue reads a JSON string, "" for anything else.
func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// joinScope puts a scope inside a directory's new position; an empty prefix is the
// data root.
func joinScope(rel, scope string) string {
	if rel == "" {
		if scope == "" {
			return ""
		}
		return "/" + strings.TrimPrefix(scope, "/")
	}
	clean := path.Join(rel, "/"+strings.TrimPrefix(scope, "/"))
	if clean == "." {
		return rel
	}
	if !strings.HasPrefix(clean, "/") {
		return "/" + clean
	}
	return clean
}

// tally counts changes by file and place.
type tally struct {
	order  []string
	counts map[string]*rewrite.Change
}

func newTally() *tally { return &tally{counts: map[string]*rewrite.Change{}} }

func (t *tally) add(where, what string) {
	key := where
	if c, ok := t.counts[key]; ok {
		c.Count++
		return
	}
	t.counts[key] = &rewrite.Change{File: dbName, Where: where, What: what, Count: 1}
	t.order = append(t.order, key)
}

func (t *tally) changes() []rewrite.Change {
	sort.Strings(t.order)
	out := make([]rewrite.Change, 0, len(t.order))
	for _, key := range t.order {
		out = append(out, *t.counts[key])
	}
	return out
}

// configSources reads `server.sources` from config.yaml, a scan rather than a YAML
// decode: the keys are flat scalars in a list, and this package may not import
// the YAML library.
func configSources(name string) ([]source, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("filebrowser: %w", err)
	}
	lines, start := configLines(string(raw))
	if start < 0 {
		return nil, nil
	}
	var out []source
	for _, item := range listItems(lines[start+1:], lines[start].indent) {
		if item["path"] == "" {
			continue
		}
		out = append(out, source{path: item["path"], name: item["name"]})
	}
	return out, nil
}

// line is one meaningful config line, with the indentation that places it.
type line struct {
	indent int
	text   string
}

// configLines is a document's meaningful lines and its `server.sources:` index, or
// -1 when it configures no source (the app defaults to its working directory).
func configLines(doc string) ([]line, int) {
	var out []line
	start := -1
	serverIndent := -1
	for _, raw := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		text := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		if start < 0 {
			switch {
			case serverIndent < 0 && trimmed == "server:":
				serverIndent = indent
			case serverIndent >= 0 && indent <= serverIndent:
				// The `server` block ended before a sources list was seen.
				return out, -1
			case trimmed == "sources:":
				start = len(out)
			}
		}
		out = append(out, line{indent: indent, text: trimmed})
	}
	return out, start
}

// listItems is the `- key: value` items under one block. An item's nested block
// (a source's `config:`) is indented past the item and skipped.
func listItems(lines []line, parentIndent int) []map[string]string {
	var out []map[string]string
	var cur map[string]string
	itemIndent := -1
	for _, l := range lines {
		if l.indent <= parentIndent {
			break
		}
		if strings.HasPrefix(l.text, "- ") || l.text == "-" {
			cur = map[string]string{}
			out = append(out, cur)
			itemIndent = l.indent
			l.text = strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
			if l.text == "" {
				continue
			}
		}
		if cur == nil || l.indent > itemIndent+2 {
			continue
		}
		key, value, ok := strings.Cut(l.text, ":")
		if !ok {
			continue
		}
		if v := unquote(value); v != "" {
			cur[strings.TrimSpace(key)] = v
		}
	}
	return out
}

// unquote strips a YAML scalar's quotes and any trailing comment.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`)
}

// plural renders "1 path" and "2 paths".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
