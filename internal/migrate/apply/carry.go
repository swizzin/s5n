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
	"fmt"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// Bulk is what a snapshot holds of one app's config beyond the extract apply's
// checks read: the whole data of the one app whose config is a library as well as
// its state (Plex's artwork and thumbnails), which is tens of gigabytes and which
// no check reads.
//
// A migration stages that bulk once, out of the snapshot itself, straight into the
// staging tree that becomes the app's config directory: it is never extracted
// beside the checks, never copied into a second temporary tree, and its numbers
// come from the pass that verified the archive rather than a walk of a tree that
// is not on the host.
type Bulk interface {
	// HoldsTree reports whether the snapshot's copy of one instance is its whole
	// data, and the extract apply reads kept the state alone. A run stages what
	// Stage writes when it is true, and the state under Configs when it is not.
	HoldsTree(instance string) bool
	// Measure is what staging one instance writes at the config root, counted as
	// the snapshot was verified.
	Measure(instance string) (Staged, error)
	// Stage writes the whole of one instance's config into dst, which must not
	// exist, owned by uid:gid, and reports what it wrote.
	Stage(instance, dst string, uid, gid int) (Staged, error)
}

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
		if d.Full && m.env.Bulk != nil && !m.env.Bulk.HoldsTree(name) {
			// A plan that says the whole data over a snapshot that carried the
			// state is a plan edited by hand: what this run stages is the state,
			// and the row says what the bytes are rather than what the plan
			// hoped.
			d.Full = false
		}
		if d.Full {
			size, err := m.carrySize(name)
			if err != nil {
				return err
			}
			free, err := m.freeAt(m.env.ConfigRoot)
			if err != nil {
				return fmt.Errorf("apps.%s: the room its whole data needs at %s: %w", name, m.env.ConfigRoot, err)
			}
			// One copy: the snapshot is written already, and what lands here is
			// the instance's directory. The staging tree it is written into is
			// on the same filesystem and is what moves into place, so the two
			// are one copy, not two. A run that extracted part of the snapshot
			// beside it has already spent those bytes, and this asks what is left.
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

// carrySize is how many bytes one app's whole data is: what the snapshot holds of
// it when the caller can say (the pass that verified the archive counted it), and
// the tree this run has beside its checks otherwise.
func (m *migration) carrySize(name string) (int64, error) {
	if m.env.Bulk != nil {
		staged, err := m.env.Bulk.Measure(name)
		if err != nil {
			return 0, fmt.Errorf("apps.%s: the plan carries its whole data, and this run cannot size the snapshot's copy of it: %w", name, err)
		}
		return staged.Bytes, nil
	}
	dir := filepath.Join(m.env.Configs, name)
	size, err := plex.Size(dir)
	if err != nil {
		return 0, fmt.Errorf("apps.%s: the plan carries its whole data, and this run cannot size %s: %w", name, dir, err)
	}
	return size, nil
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
// What it says follows the bytes this run writes, not the plan alone: decideCarry
// has already been through the snapshot's own copy (Bulk.HoldsTree, which is what
// the archive holds) and the room at the config root, so Full is the tree when the
// tree is what lands there, and the state when it is not.
func (m *migration) carried(name string) (note, phrase string) {
	if CatalogApp(name, m.env.Catalog) != appPlex {
		return "", ""
	}
	if m.carryOf(name).Full {
		tree := plex.Snapshot(true)
		return appText(name, tree.Note), tree.Summary
	}
	state := plex.Snapshot(false)
	return appText(name, state.Note), state.Summary
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
