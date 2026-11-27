// Package safefs works inside a directory the seedbox's user can write.
//
// box runs as root and that user owns the directories its apps run in, so between
// box's check and its use a component of a path can be swapped for a symlink: a
// chown by path gives the link's target away, a chmod takes its mode, an O_EXCL
// create writes anywhere root can. A Tree is one directory held open, and every
// name is resolved against it, so nothing under it can be steered out however the
// tree changes.
package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Tree is a directory opened as an os.Root, with the host path it was opened on:
// the root confines the work, the path is what a refusal prints. A tree Make made
// also holds the directory it made it in, which is what removes it again; a tree
// Open opened is somebody else's and is never removed.
type Tree struct {
	root   *os.Root
	parent *os.Root
	base   string
	dir    string
}

// Open opens dir as a Tree, through the directory that holds it: os.OpenRoot
// follows the path it is handed, so a link planted at dir would become the tree's
// own root instead of a link out of it.
func Open(dir string) (*Tree, error) {
	dir = filepath.Clean(dir)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	root, err := parent.OpenRoot(filepath.Base(dir))
	parent.Close()
	if err != nil {
		return nil, err
	}
	return &Tree{root: root, dir: dir}, nil
}

// Make creates dir, which must not exist, with mode, through a handle on the
// directory that holds it, and returns it as a Tree. The directory above dir has
// to exist: what is made here is the tree, never a chain of parents a swap could
// have redirected.
func Make(dir string, mode fs.FileMode) (*Tree, error) {
	dir = filepath.Clean(dir)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	base := filepath.Base(dir)
	if err := parent.Mkdir(base, mode); err != nil {
		parent.Close()
		return nil, err
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		parent.Close()
		return nil, err
	}
	// The process umask is not the caller's: a tree waiting to be filled is box's
	// own, and what it is finally is set once it is whole.
	if err := root.Chmod(".", mode); err != nil {
		root.Close()
		parent.Close()
		return nil, err
	}
	return &Tree{root: root, parent: parent, base: base, dir: dir}, nil
}

// Close releases the handles on the directory, and on the one it was made in.
func (t *Tree) Close() error {
	err := t.root.Close()
	if t.parent != nil {
		if perr := t.parent.Close(); err == nil {
			err = perr
		}
	}
	return err
}

// Remove deletes the tree and everything in it, through the handle on the directory
// it was made in: a removal by path deletes whatever the name means by then, which
// for a tree swapped for a link is somewhere else entirely. It refuses a tree Make
// did not make, which is a seedbox user's config directory.
func (t *Tree) Remove() error {
	if t.parent == nil {
		return fmt.Errorf("%s was not made by box, so box does not remove it", t.dir)
	}
	return t.parent.RemoveAll(t.base)
}

// Root is the handle every name inside the tree is resolved against.
func (t *Tree) Root() *os.Root { return t.root }

// Dir is the host path the tree was opened on.
func (t *Tree) Dir() string { return t.dir }

// Path is one name inside the tree spelled on the host: os.Root reports the name
// it was given, which says nothing about where that is.
func (t *Tree) Path(name string) string {
	if name == "." {
		return t.dir
	}
	return filepath.Join(t.dir, filepath.FromSlash(name))
}

// Err names the host path in a failed call on the tree.
func (t *Tree) Err(name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && !filepath.IsAbs(pe.Path) {
		return &fs.PathError{Op: pe.Op, Path: t.Path(pe.Path), Err: pe.Err}
	}
	return err
}

// Give gives the directory the tree was opened on to uid:gid, through a handle
// opened without following a link, and sets its mode. The path is resolved again
// here, so a tree swapped for a link since box made it is refused rather than
// owned: it is not the directory box filled.
func (t *Tree) Give(uid, gid int, mode fs.FileMode) error {
	f, err := os.OpenFile(t.dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	at, err := f.Stat()
	if err != nil {
		return err
	}
	made, err := t.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(at, made) {
		return fmt.Errorf("%s was replaced while box was writing in it, so it is not the tree box wrote", t.dir)
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	return f.Chmod(mode)
}
