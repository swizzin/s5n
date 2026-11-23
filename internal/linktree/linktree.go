// Package linktree is the one rule for a symlink inside a config tree that is
// being copied: a copy carries the tree and the links between its files, so a
// link that leaves the tree is refused and one that names the kernel's own
// filesystems is left out.
//
// Both copies box makes use it: migrate's staging copy of an app's config
// (internal/migrate/apply) and a snapshot's config/ tree (internal/snapshot). A
// rule in one and a different one in the other would make the snapshot a
// different tree from the one apply reads.
package linktree

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Result is what a copy does with one link.
type Result struct {
	// Target is the text the copy's link is written with. A link that named an
	// absolute path inside the tree is written relative to the link's own
	// directory, because the tree is about to move and the absolute text would
	// still name where it came from.
	Target string
	// Skip says the link is left out of the copy. It names one of the kernel's
	// filesystems: the images point an app's log at the pod's own stdout, which
	// is plumbing, never a file a config tree keeps, and the app writes the
	// link again when it starts.
	Skip bool
}

// Resolve decides what a copy does with the link at rel, inside the tree rooted
// at root, whose target is target as os.Readlink answered it. An error means the
// link leaves the tree, which a copy cannot carry: after the move it would name
// a path the receiving host does not have.
//
// root, rel and target are filesystem paths. box runs on Linux, so they are also
// the names inside a snapshot's archive.
func Resolve(root, rel, target string) (Result, error) {
	if filepath.IsAbs(target) && kernel(target) {
		return Result{Skip: true}, nil
	}
	if filepath.IsAbs(target) {
		inside, err := filepath.Rel(root, filepath.Clean(target))
		if err != nil || leaves(inside) {
			return Result{}, outside(rel, target)
		}
		back, err := filepath.Rel(filepath.Dir(rel), inside)
		if err != nil {
			return Result{}, fmt.Errorf("link %s: %w", rel, err)
		}
		return Result{Target: back}, nil
	}
	if leaves(filepath.Clean(filepath.Join(filepath.Dir(rel), target))) {
		return Result{}, outside(rel, target)
	}
	return Result{Target: target}, nil
}

// leaves reports whether a path relative to the tree's root names something
// outside it.
func leaves(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// outside is the refusal both copies read.
func outside(rel, target string) error {
	return fmt.Errorf("%s is a link to %s, which is outside the config directory: a copy carries a tree and the links inside it, so a link that leaves it would name a path the copy cannot see", rel, target)
}

// kernel reports whether an absolute path is inside one of the kernel's own
// filesystems: the process's plumbing, never a file a config tree holds.
func kernel(p string) bool {
	clean := filepath.Clean(p)
	for _, root := range []string{"/proc", "/dev", "/sys"} {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
