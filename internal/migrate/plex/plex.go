// Package plex rewrites the paths a Plex install stored in Preferences.xml and its
// library database. Plex's schema uses tokenizers compiled into its own libsqlite3
// (`tokenize=collating ...`, an `icu` tokenizer), so stock SQLite fails on any
// write that fires a metadata trigger, on FTS maintenance and on
// integrity_check, as Plex's own repair article says. Every read and write goes
// through `Plex SQLite` in the pinned image (rewrite.Input.Run); this package never
// opens the file. The columns are section_locations.root_path, media_parts.file,
// media_streams.url and metadata_items.guid (file:// only); directories.path is
// relative to root_path. The database must be closed to be written, which is why
// migrate runs before the target's first start, and its -wal and -shm travel with
// it.
package plex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

func init() { rewrite.Register("plex", rewriter{}) }

// NeedsRunner: Plex's database is written only through the `Plex SQLite` its
// own image ships (see Input.Run), never through stock SQLite.
func (rewriter) NeedsRunner() bool { return true }

const (
	// sqliteBin is Plex's own SQLite shell, beside the server binary in the
	// pinned image (usr/lib/plexmediaserver/Plex SQLite).
	sqliteBin = "/usr/lib/plexmediaserver/Plex SQLite"
	// supportDir is where the image keeps Plex's home: the entrypoint reads
	// $PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR, set to /config/Library/Application
	// Support.
	supportDir = "Library/Application Support/Plex Media Server"
	// prefsName is the settings file Plex rewrites on every upgrade.
	prefsName = "Preferences.xml"
	// dbRel is the library database inside the app-support directory.
	dbRel = "Plug-in Support/Databases/com.plexapp.plugins.library.db"
)

// supportCandidates are the layouts a staged config can have: the image's, the
// data directory at /config, and a Plex home one level down.
var supportCandidates = []string{supportDir, "", "Plex Media Server"}

type rewriter struct{}

// Check reads Preferences.xml and queries the database with SELECTs, writing
// nothing.
func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(ctx, in, true)
}

// decision is everything both passes work out.
type decision struct {
	prefs    *prefsFile
	prefEdit []prefEdit
	dbMoved  []dbRow
	tally    tally
}

func run(ctx context.Context, in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	d := decision{tally: tally{}}
	support, err := supportDirIn(in.Dir)
	if err != nil {
		return rep, err
	}
	// Preferences.xml.
	prefsPath := filepath.Join(in.Dir, support, prefsName)
	raw, err := os.ReadFile(prefsPath)
	switch {
	case err == nil:
		f, err := parsePrefs(raw)
		if err != nil {
			return rep, fmt.Errorf("plex: %s: %w", displayPath(support, prefsName), err)
		}
		d.prefs = f
		d.prefEdit = planPrefs(in, f, displayPath(support, prefsName), d.tally, &rep)
	case os.IsNotExist(err):
		rep.Notes = append(rep.Notes, displayPath(support, prefsName)+" is not in the staged config; nothing to rewrite in it")
	default:
		return rep, fmt.Errorf("plex: %w", err)
	}

	// The library database, through Plex's own SQLite.
	dbPath := filepath.Join(in.Dir, support, dbRel)
	if _, err := os.Stat(dbPath); err == nil {
		if in.Run == nil {
			return rep, errors.New("plex: the library database is written by Plex's own `Plex SQLite`, " +
				"which only the app's image has; this run has no way to execute it")
		}
		rows, missing, err := readDB(ctx, in, displayDBPath(support))
		if err != nil {
			return rep, err
		}
		if len(missing) > 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s has no %s table; there is nothing of that kind to rewrite",
				displayDBPath(support), strings.Join(missing, ", ")))
		}
		d.dbMoved = planDB(in, rows, displayDBPath(support), d.tally, &rep)
	} else if !os.IsNotExist(err) {
		return rep, fmt.Errorf("plex: %w", err)
	} else {
		rep.Notes = append(rep.Notes, displayDBPath(support)+" is not in the staged config; nothing to rewrite in it")
	}
	rep.Changes = d.tally.changes()
	if write && rep.Blocked() {
		return rep, fmt.Errorf("plex: the plan does not settle every path; nothing was written")
	}
	if !write {
		return rep, nil
	}

	if len(d.prefEdit) > 0 {
		info, err := os.Stat(prefsPath)
		if err != nil {
			return rep, fmt.Errorf("plex: %w", err)
		}
		if err := atomicfile.Write(prefsPath, d.prefs.apply(d.prefEdit), info.Mode()); err != nil {
			return rep, fmt.Errorf("plex: %w", err)
		}
	}
	if len(d.dbMoved) == 0 {
		return rep, nil
	}
	if err := writeDB(ctx, in, d.dbMoved); err != nil {
		return rep, err
	}
	return rep, nil
}

// supportDirIn is the directory holding Plex's home, a slash path relative to the
// config root.
func supportDirIn(dir string) (string, error) {
	var found []string
	for _, rel := range supportCandidates {
		if looksLikePlexHome(filepath.Join(dir, filepath.FromSlash(rel))) {
			found = append(found, rel)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("plex: %s holds neither %s nor %s; a Plex config directory keeps %s at its root or under %s",
			dir, prefsName, dbRel, prefsName, supportDir)
	default:
		return "", fmt.Errorf("plex: %s holds more than one Plex home (%s); name one by hand",
			dir, strings.Join(found, ", "))
	}
}

func looksLikePlexHome(dir string) bool {
	for _, name := range []string{prefsName, dbRel} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			return true
		}
	}
	return false
}

// displayPath is a file's name relative to the staged config.
func displayPath(support, name string) string {
	return path.Join(filepath.ToSlash(support), name)
}

func displayDBPath(support string) string { return displayPath(support, dbRel) }

// planPrefs is every path attribute and the one holding a list.
func planPrefs(in rewrite.Input, f *prefsFile, file string, t tally, rep *rewrite.Report) []prefEdit {
	var edits []prefEdit
	for _, name := range prefsPaths {
		a, ok := f.get(name)
		if !ok || strings.TrimSpace(a.value) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, a.value)
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: file, Key: name, Path: a.value,
			})
			continue
		}
		if moved != a.value {
			edits = append(edits, a.replace(moved, name, "path"))
			t.add(file, name, "path")
		}
	}
	if a, ok := f.get(prefsList); ok && strings.TrimSpace(a.value) != "" {
		items, seps := splitList(a.value)
		changed := false
		for i, item := range items {
			head, core, tail := trimSpace(item)
			if core == "" {
				continue
			}
			moved, covered := in.Plan.Rewrite(in.App, core)
			if !covered {
				rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
					App: in.App, File: file, Key: prefsList, Path: core,
				})
				continue
			}
			if moved != core {
				items[i] = head + moved + tail
				changed = true
			}
		}
		if changed {
			edits = append(edits, a.replace(joinList(items, seps), prefsList, "path"))
			t.add(file, prefsList, "path")
		}
	}
	return edits
}

// dbRow is one value a library table holds.
type dbRow struct {
	table, column string
	id            int64
	value         string
}

// planDB decides each row's value; a library:// or metadata:// URI is not a path
// and is left alone.
func planDB(in rewrite.Input, rows []dbRow, file string, t tally, rep *rewrite.Report) []dbRow {
	var moved []dbRow
	for _, r := range rows {
		value, gap, changed := rewriteRow(in, r)
		if gap != nil {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: file, Key: r.table + "." + r.column, Path: *gap,
			})
			continue
		}
		if !changed {
			continue
		}
		t.add(file, r.table+"."+r.column, "path")
		moved = append(moved, dbRow{table: r.table, column: r.column, id: r.id, value: value})
	}
	return moved
}

// rewriteRow is one value's rewrite, or a gap when no rule can map it.
func rewriteRow(in rewrite.Input, r dbRow) (value string, gap *string, changed bool) {
	v := r.value
	if v == "" {
		return v, nil, false
	}
	switch {
	case r.table == "section_locations" && r.column == "root_path":
		if !path.IsAbs(v) {
			return v, &v, false
		}
		moved, covered := in.Plan.Rewrite(in.App, v)
		if !covered {
			return v, &v, false
		}
		return moved, nil, moved != v

	case r.table == "media_parts" && r.column == "file":
		// Plex's evidence is that this column is absolute, so a relative value is
		// the pathology: a gap, not a guessed root.
		if !path.IsAbs(v) {
			return v, &v, false
		}
		moved, covered := in.Plan.Rewrite(in.App, v)
		if !covered {
			return v, &v, false
		}
		return moved, nil, moved != v

	case r.column == "url" || r.column == "guid":
		return rewriteFileURL(in, v)
	}
	return v, nil, false
}

// rewriteFileURL rewrites the path inside a file:// value, keeping the URI's shape
// (triple slash, localhost form); anything else is not a filesystem path.
func rewriteFileURL(in rewrite.Input, v string) (string, *string, bool) {
	const scheme = "file://"
	if !strings.HasPrefix(v, scheme) {
		return v, nil, false
	}
	rest := v[len(scheme):]
	host := ""
	if !strings.HasPrefix(rest, "/") {
		// localhost names this host; anything else is another machine's filesystem.
		switch {
		case strings.HasPrefix(rest, "localhost/"):
			host, rest = "localhost", rest[len("localhost"):]
		default:
			return v, &v, false
		}
	}
	if !path.IsAbs(rest) {
		return v, &v, false
	}
	moved, covered := in.Plan.Rewrite(in.App, rest)
	if !covered {
		return v, &v, false
	}
	if moved == rest {
		return v, nil, false
	}
	return scheme + host + moved, nil, true
}

// splitList keeps the separators, so the list goes back exactly as it came apart.
func splitList(s string) (items, seps []string) {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' || s[i] == ';' {
			items = append(items, s[start:i])
			seps = append(seps, string(s[i]))
			start = i + 1
		}
	}
	return append(items, s[start:]), seps
}

func joinList(items, seps []string) string {
	var b strings.Builder
	for i, item := range items {
		if i > 0 && i-1 < len(seps) {
			b.WriteString(seps[i-1])
		}
		b.WriteString(item)
	}
	return b.String()
}

// trimSpace separates an item's surrounding whitespace from its path.
func trimSpace(s string) (head, core, tail string) {
	core = strings.TrimSpace(s)
	if core == "" {
		return s, "", ""
	}
	start := strings.Index(s, core)
	return s[:start], core, s[start+len(core):]
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
