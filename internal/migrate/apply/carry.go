package apply

// carry.go is what one run stages out of an app whose config is more than its
// state: the tree the snapshot carries, or -- when the plan says the tree and the
// config path cannot hold it -- the state alone, with the numbers.
//
// The snapshot's own copy is the read's decision (cmd/box, `--plex-minimal`), and
// it is in the plan. This is the other end: the config root the copy lands on,
// which on a host that is both ends of the migration is the same machine and may
// be a smaller filesystem. A run that finds no room there carries less than the
// plan says and says so, because the alternative is failing after the box is
// installed with a snapshot that cannot be used at all.

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// carryDecision is one app's carry for this run: whether its whole data is
// staged, and the sentence saying why it is not when the plan said it would be.
type carryDecision struct {
	Full bool
	// Short is the line with the numbers, empty when the run carries what the
	// plan says.
	Short string
}

// decideCarry settles every trimmed app's carry for this run from the plan and
// the room at the config root. It reads; it writes nothing.
func (m *migration) decideCarry() error {
	for _, name := range planApps(m.p) {
		if CatalogApp(name, m.env.Catalog) != appPlex {
			continue
		}
		app := m.p.Apps[name]
		d := carryDecision{Full: app.Carry == plan.CarryFull}
		if d.Full {
			dir := filepath.Join(m.env.Configs, name)
			size, err := plex.Size(dir)
			if err != nil {
				return fmt.Errorf("apps.%s: the plan carries its whole data, and this run cannot size %s: %w", name, dir, err)
			}
			free, err := m.freeAt(m.env.ConfigRoot)
			if err != nil {
				return fmt.Errorf("apps.%s: the room its whole data needs at %s: %w", name, m.env.ConfigRoot, err)
			}
			// One copy: the snapshot is written already, and what lands here is
			// the instance's directory. The staging tree it is copied into is
			// on the same filesystem and is what moves into place, so the two
			// are one copy, not two.
			if v := space.Choose(size, []space.Place{{Path: m.env.ConfigRoot, Free: free}}); !v.Fits() {
				d.Full = false
				d.Short = fmt.Sprintf("%s: its whole data needs %s and %s is free at %s; carrying its state instead, so Plex builds its artwork, thumbnails and detection again",
					name, space.Human(v.Need), space.Human(v.Short.Free), v.Short.Path)
			}
		}
		if m.carry == nil {
			m.carry = map[string]carryDecision{}
		}
		m.carry[name] = d
	}
	return nil
}

// carryOf is one app's carry, defaulting to the plan's answer for an app
// decideCarry did not look at (every app whose config is its state).
func (m *migration) carryOf(name string) carryDecision {
	if d, ok := m.carry[name]; ok {
		return d
	}
	return carryDecision{Full: m.p.Apps[name].Carry == plan.CarryFull}
}

// freeAt is how much room a filesystem has, through the caller's answer when it
// gave one.
func (m *migration) freeAt(path string) (int64, error) {
	disk := m.env.Disk
	if disk == nil {
		disk = space.Real{}
	}
	return disk.Free(path)
}

// carried is what one app's config holds once this run has staged it: the note a
// report and a confirm say, and the phrase the app's row carries. Both are empty
// for every app whose config is its state.
//
// What it says is decided from the bytes staged, not from the plan alone: a
// snapshot that carried a state is never described as a tree, and one that
// carried a tree is never described as a subset.
func (m *migration) carried(name string) (note, phrase string) {
	if CatalogApp(name, m.env.Catalog) != appPlex {
		return "", ""
	}
	tree, state := plex.Snapshot(true), plex.Snapshot(false)
	dir := m.staging(name)
	if !isDir(dir) {
		// A pass that measured the tree instead of copying it has no staged
		// directory: the snapshot's own is what it counted.
		dir = filepath.Join(m.env.Configs, name)
	}
	if trimmed(carriedDir(dir, tree.Sub), state.Files) {
		return appText(name, state.Note), state.Summary
	}
	return appText(name, tree.Note), tree.Summary
}

// appText is one line of a report with the instance it belongs to taken off:
// the confirm names the app in its own column.
func appText(name, line string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, name+": "))
}

// stageTrimmed stages one app's state out of a config directory that holds its
// whole data: the files the plan's minimal carry names, with the directories
// above them, under the same rules as a whole-tree copy (modes and times kept,
// links carried or skipped by internal/linktree).
func stageTrimmed(dst, src string, state plex.Scope, uid, gid int) (Staged, error) {
	names := make([]string, len(state.Files))
	for i, f := range state.Files {
		names[i] = f.Path
	}
	return copyFiles(dst, carriedDir(src, state.Sub), names, uid, gid)
}

// carriedDir is where one app's own data is inside the config directory the
// snapshot carries: Plex's home, or the directory itself when the source kept its
// files at the top of the mount.
func carriedDir(dir, sub string) string {
	if home, err := plex.Home(dir); err == nil {
		return filepath.Join(dir, filepath.FromSlash(home))
	}
	return filepath.Join(dir, filepath.FromSlash(sub))
}

// errNotCarried stops trimmed's walk at the first entry of a config directory
// that a carried set does not name.
var errNotCarried = errors.New("not part of the snapshot")

// trimmed reports whether dir holds the files a snapshot carries for one app,
// with the directories above them, and nothing else. It stops at the first entry
// outside the set, which is what keeps a whole Plex tree cheap to refuse.
func trimmed(dir string, files []plex.File) bool {
	if dir == "" {
		return false
	}
	carried := make(map[string]bool, len(files))
	for _, f := range files {
		carried[f.Path] = true
		for i := strings.LastIndex(f.Path, "/"); i > 0; i = strings.LastIndex(f.Path[:i], "/") {
			carried[f.Path[:i]] = true
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !carried[filepath.ToSlash(rel)] {
			return errNotCarried
		}
		return nil
	})
	return err == nil
}
