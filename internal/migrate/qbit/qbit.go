// Package qbit rewrites what a migrated qBittorrent stored about paths: its
// configuration file, the three JSON files beside it, and the resume data in
// whichever store the source used.
//
// It runs on the copied config before the app's first start because qBittorrent 5
// converts its resume store on startup and carries the paths it held at that
// moment into the new one (research: qbittorrent.md §Q3.5); rewriting after would
// re-encode a database the app already touched and leave a stale BT_backup/ trap.
package qbit

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/fastresume"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
	"github.com/swizzin/s5n/internal/qbtconf"
)

func init() { rewrite.Register("qbittorrent", rewriter{}) }

// The app's files, relative to its config directory. The pinned image points
// HOME, XDG_CONFIG_HOME and XDG_DATA_HOME at /config, so both qBittorrent's
// config directory and its data directory land in /config/qBittorrent
// (qbittorrent@release-5.3.0:src/base/profile_p.cpp:77-105).
const (
	dirRel        = "qBittorrent"
	confRel       = dirRel + "/qBittorrent.conf"
	categoriesRel = dirRel + "/categories.json"
	watchedRel    = dirRel + "/watched_folders.json"
	rulesRel      = dirRel + "/rss/download_rules.json"
	legacyRel     = dirRel + "/BT_backup"
	resumeRel     = dirRel + "/torrents.db"
)

// layout is where this source keeps the app's files.
type layout struct {
	root       string
	conf       string
	categories string
	watched    string
	rules      string
	legacy     string
	resume     string
}

// findLayout settles the layout before anything is read and refuses the older one
// by name: a tree whose conf sits in config/ while its data sits in data/ is not a
// layout box produces, so converting it would be a guess.
func findLayout(dir string) (layout, error) {
	l := layout{
		root:       filepath.Join(dir, dirRel),
		conf:       filepath.Join(dir, dirRel, "qBittorrent.conf"),
		categories: filepath.Join(dir, dirRel, "categories.json"),
		watched:    filepath.Join(dir, dirRel, "watched_folders.json"),
		rules:      filepath.Join(dir, dirRel, "rss", "download_rules.json"),
		legacy:     filepath.Join(dir, dirRel, "BT_backup"),
		resume:     filepath.Join(dir, dirRel, "torrents.db"),
	}
	if exists(l.conf) {
		return l, nil
	}
	for _, old := range []struct{ path, name string }{
		{filepath.Join(dir, "config", "qBittorrent.conf"), "config/qBittorrent.conf"},
		{filepath.Join(dir, "data", "BT_backup"), "data/BT_backup"},
		{filepath.Join(dir, "qBittorrent.conf"), "qBittorrent.conf"},
	} {
		if exists(old.path) {
			return layout{}, fmt.Errorf(
				"qBittorrent's config is in the older layout (%s): box keeps it at %s with the resume store beside it, and migrate does not convert layouts",
				old.name, confRel)
		}
	}
	return layout{}, fmt.Errorf("no %s under %s; the app writes one on first start", confRel, dir)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TorrentPort reads the port the source announced to peers, or says it named
// none: [BitTorrent] Session\Port is live when both spellings are present, the
// older [Preferences] Connection\PortRangeMin otherwise. A refused layout names no
// port: a number read out of a shape box does not produce would be a guess.
func TorrentPort(dir string) (int, bool) {
	lay, err := findLayout(dir)
	if err != nil {
		return 0, false
	}
	raw, err := os.ReadFile(lay.conf)
	if err != nil {
		return 0, false
	}
	c := parseConf(raw)
	for _, k := range []struct{ section, name string }{
		{sectionBitTorrent, keySessionPort},
		{sectionPreferences, keyLegacyPortRangeMin},
	} {
		value, ok := c.get(k.section, k.name)
		if !ok {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port <= 0 || port > 65535 {
			return 0, false
		}
		return port, true
	}
	return 0, false
}

func isDir(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return info.IsDir(), nil
}

type rewriter struct{}

func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	lay, err := findLayout(in.Dir)
	if err != nil {
		return rewrite.Report{}, err
	}
	db, err := openResume(ctx, lay, false)
	if err != nil {
		return rewrite.Report{}, err
	}
	if db != nil {
		defer db.Close()
	}
	a, err := analyse(ctx, in, lay, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	lay, err := findLayout(in.Dir)
	if err != nil {
		return rewrite.Report{}, err
	}
	db, err := openResume(ctx, lay, true)
	if err != nil {
		return rewrite.Report{}, err
	}
	if db != nil {
		defer db.Close()
	}
	a, err := analyse(ctx, in, lay, db)
	if err != nil {
		return rewrite.Report{}, err
	}
	if err := a.apply(ctx); err != nil {
		return rewrite.Report{}, err
	}
	return a.report(), nil
}

// openResume opens the SQLite resume store when the source has one; which store it
// used is read off the directory, never assumed (research: qbittorrent.md §Q3.4).
func openResume(ctx context.Context, lay layout, writable bool) (*sql.DB, error) {
	if !exists(lay.resume) {
		return nil, nil
	}
	if writable {
		return sqlitedb.OpenReadWrite(ctx, lay.resume)
	}
	return sqlitedb.OpenReadOnly(ctx, lay.resume)
}

// outFile is one file the write pass replaces, with the source's mode.
type outFile struct {
	data []byte
	mode fs.FileMode
}

// dbEdit is one row update in the SQLite resume store.
type dbEdit struct {
	column string
	value  any
	id     int64
}

type changeKey struct {
	file  string
	where string
	what  string
}

// analysis is one pass's findings and the writes that make them true.
type analysis struct {
	in       rewrite.Input
	lay      layout
	db       *sql.DB
	changes  map[changeKey]int
	uncover  []plan.Uncovered
	notes    []string
	conf     []byte
	confMode fs.FileMode
	files    map[string]outFile
	dbEdits  []dbEdit
	legacy   bool
	resume   bool
	// stopped counts the torrents this pass wrote in a stopped state: a
	// rehearsal's only app-specific quieting (resumeData, resumeColumns).
	stopped int
}

func analyse(ctx context.Context, in rewrite.Input, lay layout, db *sql.DB) (*analysis, error) {
	a := &analysis{in: in, lay: lay, db: db, changes: make(map[changeKey]int), files: make(map[string]outFile)}
	for _, step := range []func() error{a.confFile, a.categories, a.watchedFolders, a.downloadRules} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	if err := a.resumeData(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *analysis) change(file, where, what string) {
	a.changes[changeKey{file: file, where: where, what: what}]++
}

func (a *analysis) gap(file, key, stored string) {
	a.uncover = append(a.uncover, plan.Uncovered{App: a.in.App, File: file, Key: key, Path: stored})
}

// path maps one stored path through the app's table; one no rule covers is a gap
// only where the app acts on it.
func (a *analysis) path(file, where, stored string, live bool) (string, bool) {
	out, covered := a.in.Plan.Rewrite(a.in.App, stored)
	if !covered {
		if live {
			a.gap(file, where, stored)
		}
		return stored, false
	}
	if out != stored {
		a.change(file, where, "path")
	}
	return out, true
}

func (a *analysis) report() rewrite.Report {
	rep := rewrite.Report{App: a.in.App, Uncovered: a.uncover, Notes: a.notes, Stopped: a.stopped}
	for k, n := range a.changes {
		rep.Changes = append(rep.Changes, rewrite.Change{File: k.file, Where: k.where, What: k.what, Count: n})
	}
	sort.Slice(rep.Changes, func(i, j int) bool {
		x, y := rep.Changes[i], rep.Changes[j]
		switch {
		case x.File != y.File:
			return x.File < y.File
		case x.Where != y.Where:
			return x.Where < y.Where
		}
		return x.What < y.What
	})
	sort.Slice(rep.Uncovered, func(i, j int) bool { return rep.Uncovered[i].String() < rep.Uncovered[j].String() })
	return rep
}

// refuse stops a write pass that found what its Check did not: every Check runs
// before any Write, so a new gap means the staged copy moved under the run.
func (a *analysis) refuse() error {
	if len(a.uncover) > 0 {
		return &plan.UncoveredError{Paths: a.uncover}
	}
	return nil
}

// apply writes what the pass found: the database in one transaction, each file
// through atomicfile, so a crash never leaves half a conf.
func (a *analysis) apply(ctx context.Context) error {
	if err := a.refuse(); err != nil {
		return err
	}
	if len(a.dbEdits) > 0 {
		if err := a.applyResume(ctx); err != nil {
			return err
		}
	}
	if a.conf != nil {
		if err := atomicfile.Write(a.lay.conf, a.conf, a.confMode); err != nil {
			return fmt.Errorf("write %s: %w", confRel, err)
		}
	}
	for name, f := range a.files {
		if err := atomicfile.Write(filepath.Join(a.in.Dir, filepath.FromSlash(name)), f.data, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if a.legacy {
		if _, gaps, err := fastresume.Rewrite(a.lay.legacy, a.in.App, a.in.Plan, a.resumeOptions()...); err != nil {
			return fmt.Errorf("rewrite %s: %w", legacyRel, err)
		} else if len(gaps) > 0 {
			return &plan.UncoveredError{Paths: gaps}
		}
	}
	return nil
}

func (a *analysis) applyResume(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", resumeRel, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to a no-op
	for _, e := range a.dbEdits {
		//nolint:gosec // the column is a constant in this package, never a stored value
		if _, err := tx.ExecContext(ctx, "UPDATE torrents SET "+e.column+" = ? WHERE id = ?", e.value, e.id); err != nil {
			return fmt.Errorf("update torrents.%s row %d in %s: %w", e.column, e.id, resumeRel, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", resumeRel, err)
	}
	return nil
}

// writeFile keeps a JSON file's new bytes for the write pass.
func (a *analysis) writeFile(name string, data []byte, mode fs.FileMode) {
	if data == nil {
		return
	}
	a.files[name] = outFile{data: data, mode: mode}
}

// readJSON reads one of the app's JSON files; a missing one is no error.
func readJSON(path string) (raw []byte, mode fs.FileMode, ok bool, err error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), true, nil
}

// legacyMap handles one of the two QVariant maps qBittorrent 5 migrates on first
// start: with the JSON file beside it the map is never read, and without one it
// is how the app learns the paths -- an escaped QDataStream blob this tool cannot
// rewrite safely, so it refuses with the step that settles it.
func (a *analysis) legacyMap(section, key, jsonFile, jsonRel string) error {
	where := section + " " + key
	if exists(jsonFile) {
		a.notes = append(a.notes, fmt.Sprintf(
			"%s %s: left as it is; qBittorrent reads %s instead", confRel, where, jsonRel))
		return nil
	}
	return fmt.Errorf(
		"%s %s holds a QSettings map this tool does not decode; start the source once so qBittorrent writes %s, then run migrate again",
		confRel, where, jsonRel)
}

// confFile rewrites every path-valued key in qBittorrent.conf, and its WebUI
// port.
func (a *analysis) confFile() error {
	info, err := os.Stat(a.lay.conf)
	if err != nil {
		return fmt.Errorf("read %s: %w", confRel, err)
	}
	raw, err := os.ReadFile(a.lay.conf)
	if err != nil {
		return fmt.Errorf("read %s: %w", confRel, err)
	}
	c := parseConf(raw)
	changed := false
	for _, k := range confKeys {
		keyChanged, err := a.confKey(c, k)
		if err != nil {
			return err
		}
		changed = changed || keyChanged
	}
	// No URL base: the chart's route strips a prefix, so the port is all box sets.
	ep, ok := a.in.Targets[a.in.App]
	if !ok {
		return fmt.Errorf("the plan has no endpoint for %s, so its WebUI port cannot be set", a.in.App)
	}
	want := strconv.Itoa(ep.Port)
	if stored, ok := c.get(sectionWebUI, "Port"); !ok || stored != want {
		c.set(sectionWebUI, "Port", want)
		a.change(confRel, sectionWebUI+" Port", "port")
		changed = true
	}
	// A rehearsal's two settings, read at start: anything added from here lands
	// stopped, and RSS rules grab nothing.
	if a.in.Rehearsal {
		for _, k := range rehearsalConfKeys {
			where := k.section + " " + k.key
			if stored, ok := c.get(k.section, k.key); ok && stored == k.value {
				continue
			}
			c.set(k.section, k.key, k.value)
			a.change(confRel, where, "rehearsal")
			changed = true
		}
	}
	if !changed {
		return nil
	}
	a.conf, a.confMode = c.bytes(), info.Mode().Perm()
	return nil
}

// confKey rewrites one path-valued key and reports whether it changed.
func (a *analysis) confKey(c *conf, k confKey) (bool, error) {
	value, ok := c.get(k.section, k.name)
	if !ok {
		return false, nil
	}
	where := k.section + " " + k.name
	switch k.kind {
	case scalarPath:
		stored := qbtconf.Unquote(value)
		if stored == "" {
			return false, nil
		}
		out, covered := a.path(confRel, where, stored, k.live)
		if !covered || out == stored {
			return false, nil
		}
		c.set(k.section, k.name, quote(out))
		return true, nil
	case pathList:
		return a.confList(c, k, where, value), nil
	default:
		return false, a.legacyMap(k.section, k.name, a.jsonFor(k.name), a.jsonRelFor(k.name))
	}
}

// confList rewrites the elements a rule covers; a history list is cosmetic, so an
// uncovered entry stays rather than stopping the migration.
func (a *analysis) confList(c *conf, k confKey, where, value string) bool {
	items := splitList(value)
	rewritten := make([]string, len(items))
	changed := false
	for i, item := range items {
		out, covered := a.path(confRel, where, item, false)
		if !covered {
			out = item
		}
		if out != item {
			changed = true
		}
		rewritten[i] = out
	}
	if !changed {
		return false
	}
	c.set(k.section, k.name, joinList(rewritten))
	return true
}

// jsonFor is the JSON file that supersedes one legacy map.
func (a *analysis) jsonFor(key string) string {
	if key == categoriesKey {
		return a.lay.categories
	}
	return a.lay.watched
}

func (a *analysis) jsonRelFor(key string) string {
	if key == categoriesKey {
		return categoriesRel
	}
	return watchedRel
}

// resumeData rewrites whichever resume stores the source has. A source can have
// both (a Legacy → SQLite conversion leaves BT_backup behind,
// sessionimpl.cpp:1741-1755), so each present store is rewritten and the operator
// is told when there are two.
func (a *analysis) resumeData(ctx context.Context) error {
	legacy, err := isDir(a.lay.legacy)
	if err != nil {
		return fmt.Errorf("read %s: %w", legacyRel, err)
	}
	a.legacy = legacy
	a.resume = exists(a.lay.resume)
	if legacy {
		rep, gaps, err := fastresume.Scan(a.lay.legacy, a.in.App, a.in.Plan, a.resumeOptions()...)
		if err != nil {
			return fmt.Errorf("read %s: %w", legacyRel, err)
		}
		for _, g := range gaps {
			g.File = legacyRel + "/" + g.File
			a.uncover = append(a.uncover, g)
		}
		for key, n := range rep.Rewrites {
			a.changes[changeKey{file: legacyRel, where: "BT_backup." + key, what: "path"}] += n
		}
		a.stopped += rep.Stopped
	}
	if a.resume {
		if err := a.resumeStore(ctx); err != nil {
			return err
		}
	}
	switch {
	case legacy && a.resume:
		a.notes = append(a.notes, "both resume stores are present (BT_backup and torrents.db); every torrent in both was rewritten")
	case !legacy && !a.resume:
		a.notes = append(a.notes, "the source kept no resume data (no BT_backup and no torrents.db); the first start checks every torrent from scratch")
	}
	return nil
}

// resumeStore rewrites the SQLite store's two path columns and the bencoded
// libtorrent blob beside them (research: qbittorrent.md §Q3.4).
func (a *analysis) resumeStore(ctx context.Context) error {
	findings, err := sqlitedb.IntegrityCheck(ctx, a.db)
	if err != nil {
		return fmt.Errorf("check %s: %w", resumeRel, err)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%s is damaged (%s); copy it again from the stopped source", resumeRel, strings.Join(findings, "; "))
	}
	rows, err := a.db.QueryContext(ctx, "SELECT id, target_save_path, download_path, libtorrent_resume_data FROM torrents")
	if err != nil {
		return fmt.Errorf("read torrents in %s: %w", resumeRel, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var save, download sql.NullString
		var blob []byte
		if err := rows.Scan(&id, &save, &download, &blob); err != nil {
			return fmt.Errorf("read torrents in %s: %w", resumeRel, err)
		}
		if err := a.resumeColumns(id, save, download, blob); err != nil {
			return err
		}
	}
	return rows.Err()
}

// resumeColumns rewrites one row's two path columns and its bencoded blob.
func (a *analysis) resumeColumns(id int64, save, download sql.NullString, blob []byte) error {
	for _, column := range []struct {
		name  string
		value sql.NullString
	}{{"target_save_path", save}, {keyDownloadPath, download}} {
		if !column.value.Valid || column.value.String == "" {
			// An empty save path means automatic torrent management.
			continue
		}
		out, covered := a.path(resumeRel, "torrents."+column.name, column.value.String, true)
		if covered && out != column.value.String {
			a.dbEdits = append(a.dbEdits, dbEdit{column: column.name, value: out, id: id})
		}
	}
	if len(blob) == 0 {
		return nil
	}
	data, rewrites, gaps, stopped, err := fastresume.Blob(resumeRel, blob, a.in.App, a.in.Plan, a.resumeOptions()...)
	if err != nil {
		return fmt.Errorf("%s row %d: %w", resumeRel, id, err)
	}
	if stopped {
		a.stopped++
	}
	for _, g := range gaps {
		g.Key = "libtorrent_resume_data " + g.Key
		a.uncover = append(a.uncover, g)
	}
	for key, n := range rewrites {
		a.changes[changeKey{file: resumeRel, where: "torrents.libtorrent_resume_data." + key, what: "path"}] += n
	}
	if len(gaps) == 0 && !bytes.Equal(data, blob) {
		a.dbEdits = append(a.dbEdits, dbEdit{column: "libtorrent_resume_data", value: data, id: id})
	}
	return nil
}

// resumeOptions stops the torrents in a rehearsal, so either store comes out
// stopped.
func (a *analysis) resumeOptions() []fastresume.Option {
	if a.in.Rehearsal {
		return []fastresume.Option{fastresume.Stop()}
	}
	return nil
}
