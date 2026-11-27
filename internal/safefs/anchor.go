package safefs

// anchor.go is the directory a run is opened on: the config root, the data root, or
// the directory holding a tree box reads. Every name used under it is resolved from
// the one handle, so a component swapped for a link cannot steer work out of it.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Anchor is a directory held open for as long as box works in it, with the host path
// it was opened on: the root confines every name, the path is what a refusal prints.
type Anchor struct {
	root *os.Root
	dir  string
}

// OpenAnchor opens dir as an Anchor. The last name is read without following it and
// must still be the directory that was opened, so a link planted at the name, or a
// directory swapped for one, is refused rather than adopted: os.OpenRoot follows the
// path it is handed.
func OpenAnchor(dir string) (*Anchor, error) {
	dir = filepath.Clean(dir)
	base := filepath.Base(dir)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Lstat(base)
	if err != nil {
		return nil, named(dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, named(dir, errors.New("it is a symbolic link, and box works in the directory it opened, not the one a link names"))
	}
	if !info.IsDir() {
		return nil, named(dir, syscall.ENOTDIR)
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		return nil, named(dir, err)
	}
	at, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, named(dir, err)
	}
	if !os.SameFile(info, at) {
		root.Close()
		return nil, named(dir, errors.New("it was replaced while box opened it"))
	}
	return &Anchor{root: root, dir: dir}, nil
}

// OpenOwned opens dir as an Anchor for a tree the seedbox's user owns, and refuses
// one that is not a directory owned by uid or by root, which is what box init leaves
// the config root and the data root as. os.OpenRoot follows the parent path, so a
// parent swapped for a link can still only lead to a directory that user owns.
func OpenOwned(dir string, uid int) (*Anchor, error) {
	anchor, err := OpenAnchor(dir)
	if err != nil {
		return nil, err
	}
	info, err := anchor.root.Stat(".")
	if err != nil {
		anchor.Close()
		return nil, named(dir, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !ownsAnchor(st, uid) {
		anchor.Close()
		owner := "another user"
		if ok {
			owner = fmt.Sprintf("uid %d", st.Uid)
		}
		return nil, fmt.Errorf("%s is owned by %s, and box works only in a root it can trust: one the seedbox's user %d or root owns", dir, owner, uid)
	}
	return anchor, nil
}

// ownsAnchor is the rule for the directory root's work is anchored on. It is the
// seedbox's user's or root's, so the run's work stays inside a tree that user could
// already write, whatever a swapped parent led box to.
func ownsAnchor(st *syscall.Stat_t, uid int) bool { return int(st.Uid) == uid || st.Uid == 0 }

// Root is the handle every name under the anchor is resolved against.
func (a *Anchor) Root() *os.Root { return a.root }

// Dir is the host path the anchor was opened on.
func (a *Anchor) Dir() string { return a.dir }

// Path is one name inside the anchor spelled on the host.
func (a *Anchor) Path(name string) string {
	if name == "." || name == "" {
		return a.dir
	}
	return filepath.Join(a.dir, filepath.FromSlash(name))
}

// Close releases the handle.
func (a *Anchor) Close() error {
	if a == nil || a.root == nil {
		return nil
	}
	return a.root.Close()
}

// Err names the host path in a failed call on the anchor: os.Root reports the name it
// was given, which says nothing about where that is.
func (a *Anchor) Err(name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && !filepath.IsAbs(pe.Path) {
		return &fs.PathError{Op: pe.Op, Path: a.Path(pe.Path), Err: pe.Err}
	}
	return err
}

// named spells a failed call on dir as a path error, with the reason the call gave.
func named(dir string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &fs.PathError{Op: "open", Path: dir, Err: pe.Err}
	}
	return &fs.PathError{Op: "open", Path: dir, Err: err}
}
