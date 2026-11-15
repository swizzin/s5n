// Package plex rewrites the paths a Plex Media Server install stored: the ones
// in Preferences.xml and the ones in its library database.
//
// Plex is the one app in the catalog whose database migrate may not write with
// a stock SQLite. Its schema creates full-text tables with tokenizers compiled
// into Plex's own libsqlite3 (`tokenize=collating 'root@colStrength=primary;...'`
// and an `icu` tokenizer), so a stock shell can open the file, read it and
// update a plain column, but every write that fires a metadata trigger, every
// FTS maintenance statement and `pragma integrity_check` fail with "unknown
// tokenizer: collating". Plex says the same thing in its own repair article:
// the shipped interpreter is the only one that can touch the database. So every
// read and write here goes through `Plex SQLite` in the pinned image, through
// rewrite.Input.Run, and this package never opens the file itself.
//
// The columns are the four the community's move scripts and Plex's own queries
// agree on: section_locations.root_path, media_parts.file, media_streams.url
// (only the file:// values) and metadata_items.guid (only file://).
// directories.path is deliberately left: it is relative to the section root,
// which is what root_path carries.
//
// The database has to be closed when it is written -- Plex keeps a connection
// pool, -wal and -shm beside it and caches pages -- which is why migrate runs
// before the target's first start. The staged copy's -wal and -shm are part of
// the database and are left where they are; the shell writes through them.
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

const (
	// sqliteBin is Plex's own SQLite shell, beside the server binary in the
	// pinned image (usr/lib/plexmediaserver/Plex SQLite).
	sqliteBin = "/usr/lib/plexmediaserver/Plex SQLite"
	// supportDir is where the image keeps Plex's home: the entrypoint reads
	// Preferences.xml from $PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR, which
	// the image sets to /config/Library/Application Support.
	supportDir = "Library/Application Support/Plex Media Server"
	// prefsName is the settings file Plex rewrites on every upgrade.
	prefsName = "Preferences.xml"
	// dbRel is the library database inside the app-support directory.
	dbRel = "Plug-in Support/Databases/com.plexapp.plugins.library.db"
)

// supportCandidates are the layouts a staged Plex config can have: the image's
// own (Plex's home under Library/Application Support), the data directory
// itself mounted at /config, and a Plex home one level down.
var supportCandidates = []string{supportDir, "", "Plex Media Server"}

type rewriter struct{}

// Check reads Preferences.xml and queries the library database, writing
// nothing: the queries it sends are SELECTs, and the shell is the image's.
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

// supportDirIn is the directory inside the staged config that holds Plex's
// home, as a slash path relative to it.
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

// displayPath is a file's name relative to the staged config, the way a report
// names a file.
func displayPath(support, name string) string {
	return path.Join(filepath.ToSlash(support), name)
}

func displayDBPath(support string) string { return displayPath(support, dbRel) }

// planPrefs is Preferences.xml: every attribute that holds a path, and the one
// that holds a list of them.
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

// planDB decides each row's new value. A gap in a live column is reported; a
// value that is not a path at all (a library:// or metadata:// URI) is not.
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

// rewriteRow is one stored value's rewrite: the absolute path in it, or a gap
// when the value is one the rules cannot map.
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
		// A relative file follows the save path the app is given, but Plex's
		// evidence is that this column is absolute and a scanner-relative value
		// is the pathology: refuse rather than point the app at nothing.
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

// rewriteFileURL rewrites the path inside a file:// value, keeping the URI's
// own shape: a triple slash, or a file://localhost/ form, or a file:// name
// with the absolute path after it. Anything that is not a file:// URL is not a
// filesystem path and is left alone.
func rewriteFileURL(in rewrite.Input, v string) (string, *string, bool) {
	const scheme = "file://"
	if !strings.HasPrefix(v, scheme) {
		return v, nil, false
	}
	rest := v[len(scheme):]
	host := ""
	if !strings.HasPrefix(rest, "/") {
		// file://localhost/path names this host; anything else is another
		// machine's filesystem, which no rule can map.
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

// splitList splits a comma- or semicolon-separated list and keeps the
// separators, so it goes back together exactly as it came apart.
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
