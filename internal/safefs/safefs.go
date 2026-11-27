// Package safefs works inside a directory the seedbox's user can write.
//
// box runs as root and that user owns the directories its apps run in, so between
// box's check and its use a component of a path can be swapped for a symlink: a
// chown by path gives the link's target away, a chmod takes its mode, an O_EXCL
// create writes anywhere root can. An Anchor is a directory held open and a Tree is
// one directory inside it, and every name is resolved against a handle, so nothing
// under either can be steered out however the tree changes.
package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Tree is a directory opened as an os.Root inside an anchor, with the host path it is
// spelled as: the root confines the work, the path is what a refusal prints. A tree
// Make made is also box's to remove again; a tree Open opened is somebody else's.
type Tree struct {
	root   *os.Root
	anchor *Anchor
	// name is the tree inside the anchor, dir the host path it is named by.
	name string
	dir  string
	made bool
	// own is the anchor this tree opened for itself, which it closes with the tree.
	own *Anchor
}

// Open opens name inside anchor as a Tree. A name that leaves the anchor is refused
// by the root, whatever the path from the anchor to it does.
func Open(a *Anchor, name string) (*Tree, error) {
	name = filepath.Clean(name)
	root, err := a.root.OpenRoot(name)
	if err != nil {
		return nil, a.Err(name, err)
	}
	return &Tree{root: root, anchor: a, name: name, dir: a.Path(name)}, nil
}

// OpenAt opens the host path dir as a Tree through an anchor on the directory that
// holds it, and closes that anchor with the tree. It is for a tree whose parent is
// box's own; one under a root the seedbox's user writes is named in that root's
// Anchor instead (Open), which the caller opens once.
func OpenAt(dir string) (*Tree, error) {
	at, err := OpenAnchor(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		return nil, err
	}
	tree, err := Open(at, filepath.Base(filepath.Clean(dir)))
	if err != nil {
		at.Close()
		return nil, err
	}
	tree.own = at
	return tree, nil
}

// Make creates name inside anchor, which must not exist, with mode. The directory
// above it has to exist: what is made here is the tree, never a chain of parents a
// swap could have redirected.
func Make(a *Anchor, name string, mode fs.FileMode) (*Tree, error) {
	name = filepath.Clean(name)
	if !fs.ValidPath(name) || name == "." {
		return nil, fmt.Errorf("%q is not a name inside %s", name, a.dir)
	}
	parent := a.root
	if up := filepath.Dir(name); up != "." {
		handle, err := a.root.OpenRoot(up)
		if err != nil {
			return nil, a.Err(up, err)
		}
		defer handle.Close()
		parent = handle
	}
	base := filepath.Base(name)
	if err := parent.Mkdir(base, mode); err != nil {
		return nil, a.Err(name, err)
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		return nil, a.Err(name, err)
	}
	// The process umask is not the caller's: a tree waiting to be filled is box's
	// own, and what it is finally is set once it is whole.
	if err := root.Chmod(".", mode); err != nil {
		root.Close()
		return nil, a.Err(name, err)
	}
	return &Tree{root: root, anchor: a, name: name, dir: a.Path(name), made: true}, nil
}

// Close releases the handle on the tree, and the anchor it opened for itself.
func (t *Tree) Close() error {
	if t == nil {
		return nil
	}
	err := error(nil)
	if t.root != nil {
		err = t.root.Close()
	}
	if t.own != nil {
		if aerr := t.own.Close(); err == nil {
			err = aerr
		}
	}
	return err
}

// Remove deletes the tree and everything in it, resolved from the anchor: a removal
// by host path deletes whatever the name means by then, which for a tree swapped for
// a link is somewhere else entirely. It refuses a tree Make did not make, which is a
// seedbox user's config directory.
func (t *Tree) Remove() error {
	if !t.made {
		return fmt.Errorf("%s was not made by box, so box does not remove it", t.dir)
	}
	if err := t.anchor.root.RemoveAll(t.name); err != nil {
		return t.anchor.Err(t.name, err)
	}
	return nil
}

// Root is the handle every name inside the tree is resolved against.
func (t *Tree) Root() *os.Root { return t.root }

// Dir is the host path the tree was opened on.
func (t *Tree) Dir() string { return t.dir }

// Path is one name inside the tree spelled on the host.
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

// Give gives the directory the tree was opened on to uid:gid, resolved from the
// anchor without following a link at any component, and sets its mode. A tree swapped
// since box made it is refused rather than owned: it is not the directory box filled.
func (t *Tree) Give(uid, gid int, mode fs.FileMode) error {
	f, err := t.anchor.root.OpenFile(t.name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return t.anchor.Err(t.name, err)
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
