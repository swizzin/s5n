package arr

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// database is one pass over the app's own rows.
func (a *analysis) database(ctx context.Context, db *sql.DB) error {
	steps := []func(context.Context, *sql.DB) error{
		a.mediaTables,
		a.providers,
	}
	// Prowlarr has no remote path mappings; its clients are the source's Arrs'
	// (TableMapping.cs:41-101 at v2.7.0.5723).
	if a.kind != kindProwlarr {
		steps = append(steps, a.pathMappings)
	}
	steps = append(steps, a.configTable, a.history, a.commands)
	for _, step := range steps {
		if err := step(ctx, db); err != nil {
			return err
		}
	}
	return nil
}

// mediaTables rewrites the columns naming the library. Prowlarr keeps none: its
// tables are settings, indexers and history.
func (a *analysis) mediaTables(ctx context.Context, db *sql.DB) error {
	switch a.kind {
	case kindSonarr:
		if err := a.column(ctx, db, "Series", "Path"); err != nil {
			return err
		}
	case kindRadarr:
		// A collection's own root folder is used when a sync adds a film
		// (MovieCollection.cs:22 at v5.20.1.9773).
		if err := a.column(ctx, db, "Movies", "Path"); err != nil {
			return err
		}
		if err := a.column(ctx, db, "Collections", "RootFolderPath"); err != nil {
			return err
		}
	case kindLidarr:
		// An artist is the library folder, and its files are a column of their own:
		// migration 30 stores the full path, so a file follows its artist only when
		// TrackFiles.Path is rewritten too (v3.1.2.4902).
		if err := a.column(ctx, db, "Artists", "Path"); err != nil {
			return err
		}
		if err := a.column(ctx, db, "TrackFiles", "Path"); err != nil {
			return err
		}
	default:
		return nil
	}
	if err := a.column(ctx, db, "RootFolders", "Path"); err != nil {
		return err
	}
	// The list's root folder is live; its RootFolderPaths in Settings is the remote
	// server's filter and is left (research: §2.2, §5).
	return a.column(ctx, db, "ImportLists", "RootFolderPath")
}

// column rewrites one path column, built only from constants; no stored value is
// ever part of a statement.
func (a *analysis) column(ctx context.Context, db *sql.DB, table, column string) error {
	//nolint:gosec // identifiers are constants in this package
	rows, err := db.QueryContext(ctx, "SELECT Id, "+column+" FROM "+table)
	if err != nil {
		return fmt.Errorf("read %s.%s in %s: %w", table, column, a.file, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stored sql.NullString
		if err := rows.Scan(&id, &stored); err != nil {
			return fmt.Errorf("read %s.%s in %s: %w", table, column, a.file, err)
		}
		if !stored.Valid {
			continue
		}
		out, ok := a.rewritePath(a.app, table+"."+column, stored.String)
		if ok && out != stored.String {
			a.edits = append(a.edits, edit{table: table, column: column, id: id, value: out})
		}
	}
	return rows.Err()
}

// rewritePath maps one stored path; every column it is called for is live, so a
// gap is recorded.
func (a *analysis) rewritePath(app, where, stored string) (string, bool) {
	out, ok := a.in.Plan.Rewrite(app, stored)
	if !ok {
		a.gap(app, a.file, where, stored)
		return stored, false
	}
	if out != stored {
		a.add(a.file, where, "path")
	}
	return out, true
}

// pathMappings rewrites the remote path mappings. A side is a path in its own
// app's filesystem, so each goes through its own table (research: §5.1,
// RemotePathMappingService.cs:20-21 at v4.0.14.2938). After migration both apps see
// /data, so a row whose sides come out equal is deleted: one filesystem needs no
// mappings (docs/box.md, "The data contract").
func (a *analysis) pathMappings(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT Id, Host, RemotePath, LocalPath FROM RemotePathMappings")
	if err != nil {
		return fmt.Errorf("read RemotePathMappings in %s: %w", a.file, err)
	}
	type mapping struct {
		id     int64
		host   string
		remote string
		local  string
	}
	var found []mapping
	for rows.Next() {
		var m mapping
		if err := rows.Scan(&m.id, &m.host, &m.remote, &m.local); err != nil {
			rows.Close()
			return fmt.Errorf("read RemotePathMappings in %s: %w", a.file, err)
		}
		found = append(found, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read RemotePathMappings in %s: %w", a.file, err)
	}

	for _, m := range found {
		client, ok := a.mappingClient(m.host)
		if !ok {
			continue
		}
		// Both sides are rewritten before either is counted: a row about to be
		// deleted would otherwise report writes that never happen.
		remote, remoteOK := a.in.Plan.Rewrite(client, m.remote)
		if !remoteOK {
			a.gap(client, a.file, "RemotePathMappings.RemotePath", m.remote)
			continue
		}
		local, localOK := a.in.Plan.Rewrite(a.app, m.local)
		if !localOK {
			a.gap(a.app, a.file, "RemotePathMappings.LocalPath", m.local)
			continue
		}
		if remote == local {
			a.mappings = append(a.mappings, m.id)
			a.add(a.file, "RemotePathMappings", "dropped")
			a.notes = append(a.notes, fmt.Sprintf(
				"RemotePathMappings row %d (%s): both sides now read %s, so it was deleted; one filesystem needs no mapping",
				m.id, m.host, remote))
			continue
		}
		if remote != m.remote {
			a.add(a.file, "RemotePathMappings.RemotePath", "path")
			a.edits = append(a.edits, edit{table: "RemotePathMappings", column: "RemotePath", id: m.id, value: remote})
		}
		if local != m.local {
			a.add(a.file, "RemotePathMappings.LocalPath", "path")
			a.edits = append(a.edits, edit{table: "RemotePathMappings", column: "LocalPath", id: m.id, value: local})
		}
	}
	return nil
}

// mappingClient settles which app a mapping's host names, picking the remote
// side's table. A client this pass already pointed at the box answers first; then
// in.Resolve, so a mapping left on the box reads back settled. An address neither
// settles is reported.
func (a *analysis) mappingClient(host string) (string, bool) {
	if target, ok := a.clientTargets[strings.ToLower(host)]; ok {
		return target, true
	}
	res := a.in.Resolve(host, 0)
	switch res.Verdict {
	case plan.Rewrite:
		return res.App, true
	case plan.Leave:
		return "", false
	}
	a.unclass = append(a.unclass, rewrite.Address{App: a.app, File: a.file, Where: "RemotePathMappings.Host", Host: host})
	return "", false
}

// configTable rewrites the two Config keys holding a path; other keys are names or
// modes (ConfigService.cs:181-185,296-301 at v4.0.14.2938). A relative value
// follows the config directory.
func (a *analysis) configTable(ctx context.Context, db *sql.DB) error {
	keys := map[string]bool{"backupfolder": true}
	if a.kind != kindProwlarr {
		keys["scriptimportpath"] = true
	}
	rows, err := db.QueryContext(ctx, "SELECT Id, Key, Value FROM Config")
	if err != nil {
		return fmt.Errorf("read Config in %s: %w", a.file, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key, value string
		if err := rows.Scan(&id, &key, &value); err != nil {
			return fmt.Errorf("read Config in %s: %w", a.file, err)
		}
		// Rows are written lowercased (ConfigService.cs:476), so a folded key is it.
		key = strings.ToLower(key)
		if !keys[key] || !path.IsAbs(value) {
			continue
		}
		out, ok := a.rewritePath(a.app, "Config."+key, value)
		if ok && out != value {
			a.edits = append(a.edits, edit{table: "Config", column: "Value", id: id, value: out})
		}
	}
	return rows.Err()
}

// history rewrites the record: never read back to act on a file, so an uncovered
// path is left, while a covered one is rewritten for the UI (docs/box.md, "Every
// rewriter runs twice").
func (a *analysis) history(ctx context.Context, db *sql.DB) error {
	tables := []string{"History"}
	if a.kind != kindProwlarr {
		// Prowlarr has no DownloadHistory and its Data holds search criteria
		// (research: §2.3).
		tables = append(tables, "DownloadHistory")
	}
	for _, table := range tables {
		if err := a.historyTable(ctx, db, table, a.kind != kindProwlarr); err != nil {
			return err
		}
	}
	return nil
}

// historyTable rewrites the JSON path keys in Data and SourceTitle (set to a path
// by a rename or failed import). Only named keys are touched: the rest is free
// text and is never string-replaced.
func (a *analysis) historyTable(ctx context.Context, db *sql.DB, table string, hasTitle bool) error {
	// Prowlarr's History has no SourceTitle (History.cs:9-19 at v2.7.0.5723).
	columns := "Id, Data"
	if hasTitle {
		columns = "Id, Data, SourceTitle"
	}
	//nolint:gosec // the table name is a constant in this package
	rows, err := db.QueryContext(ctx, "SELECT "+columns+" FROM "+table)
	if err != nil {
		return fmt.Errorf("read %s in %s: %w", table, a.file, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var data, title sql.NullString
		scan := []any{&id, &data}
		if hasTitle {
			scan = append(scan, &title)
		}
		if err := rows.Scan(scan...); err != nil {
			return fmt.Errorf("read %s in %s: %w", table, a.file, err)
		}
		blob, changed, err := a.historyData(table, id, data)
		if err != nil {
			return err
		}
		if changed {
			a.edits = append(a.edits, edit{table: table, column: "Data", id: id, value: blob})
		}
		if path, ok := a.historyTitle(table, title); ok {
			a.edits = append(a.edits, edit{table: table, column: "SourceTitle", id: id, value: path})
		}
	}
	return rows.Err()
}

// historyData rewrites one row's path keys; an uncovered path is left and not
// counted.
func (a *analysis) historyData(table string, id int64, data sql.NullString) (string, bool, error) {
	f, err := parseFields(data)
	if err != nil {
		return "", false, fmt.Errorf("%s in %s, row %d: %w", table, a.file, id, err)
	}
	changed := false
	for _, key := range historyPathKeys {
		key, ok := f.find(key)
		if !ok {
			continue
		}
		stored, ok := f.str(key)
		if !ok || stored == "" {
			continue
		}
		out, covered := a.in.Plan.Rewrite(a.app, stored)
		if !covered || out == stored {
			continue
		}
		f[key] = mustJSON(out)
		a.add(a.file, table+".Data."+key, "path")
		changed = true
	}
	if !changed {
		return "", false, nil
	}
	blob, err := f.marshal()
	if err != nil {
		return "", false, fmt.Errorf("%s in %s, row %d: %w", table, a.file, id, err)
	}
	return blob, true, nil
}

// historyTitle rewrites a SourceTitle that is an absolute covered path.
func (a *analysis) historyTitle(table string, title sql.NullString) (string, bool) {
	if !title.Valid || title.String == "" {
		return "", false
	}
	out, covered := a.in.Plan.Rewrite(a.app, title.String)
	if !covered || out == title.String {
		return "", false
	}
	a.add(a.file, table+".SourceTitle", "path")
	return out, true
}

// commands drops queued or running rows: they name moved paths, never run again,
// and a stale scan path would look in a directory that no longer holds anything
// (CommandRepository.cs:25-30 at v4.0.14.2938).
func (a *analysis) commands(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT Id, Status FROM Commands WHERE Status IN (0, 1)")
	if err != nil {
		return fmt.Errorf("read Commands in %s: %w", a.file, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status int
		if err := rows.Scan(&id, &status); err != nil {
			return fmt.Errorf("read Commands in %s: %w", a.file, err)
		}
		a.dropped = append(a.dropped, id)
		a.add(a.file, "Commands", "dropped")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(a.dropped) > 0 {
		a.notes = append(a.notes, fmt.Sprintf(
			"Commands: %s that never finished were dropped; they named paths this migration moves", plural(len(a.dropped), "command")))
	}
	return nil
}

// plural renders "1 command" and "3 commands".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
