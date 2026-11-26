package apply

// carry.go is what a run stages of a config that is more than its state: the tree,
// or the state alone when the config root cannot hold it. The snapshot's copy is
// the read's decision in the plan; this is the other end, which may be a smaller
// filesystem. A run that finds no room carries less and says so, rather than
// failing after the install with an unusable snapshot.

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// Bulk is what a snapshot holds beyond the extract the checks read: Plex's artwork
// and thumbnails. A run stages it once, straight from the snapshot into the staging
// tree, and its numbers come from the verifying pass rather than a walk.
type Bulk interface {
	// HoldsTree reports whether the snapshot's copy of one instance is its whole
	HoldsTree(instance string) bool
	// Measure is what staging one instance writes at the config root, counted as
	// the snapshot was verified.
	Measure(instance string) (Staged, error)
	// Stage writes one instance's whole config into dst, owned by uid:gid.
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
			// A hand-edited plan claiming whole data over a state snapshot: the run
			// stages the state and says what the bytes are.
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
			// One copy: the staging tree is what moves into place. Bytes already
			// extracted beside it are spent, so the remaining room is asked.
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

// carrySize is the whole data's bytes, from the verifying pass when it can say.
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

// carried is what an app's config holds after staging, following the bytes written
// rather than the plan: tree when it lands, state when not.
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
func appText(name, line string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, name+": "))
}

// stageTrimmed stages the state under the app's home, not at the mount's top: an
// migration must not cause.
func stageTrimmed(dst, src string, state plex.Scope, uid, gid int) (Staged, error) {
	names := make([]string, len(state.Files))
	for i, f := range state.Files {
		names[i] = path.Join(homeRelative(src, state), f.Path)
	}
	return copyFiles(dst, src, names, uid, gid)
}

// homeRelative is the app's home inside its config directory, empty for the root.
func homeRelative(dir string, state plex.Scope) string {
	home, err := filepath.Rel(dir, carriedDir(dir, state.Sub))
	if err != nil || home == "." {
		return ""
	}
	return filepath.ToSlash(home)
}

// carriedDir is the app's own data directory inside the snapshot's copy.
func carriedDir(dir, sub string) string {
	if home, err := plex.Home(dir); err == nil {
		return filepath.Join(dir, filepath.FromSlash(home))
	}
	return filepath.Join(dir, filepath.FromSlash(sub))
}
