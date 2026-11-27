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
	if int(st.Uid) != uid {
		// Root's directory is only trusted when the user could not have led box to
		// it: a link that user swapped in above it names any root-owned directory.
		if err := rootsAlone(dir); err != nil {
			anchor.Close()
			return nil, err
		}
	}
	return anchor, nil
}

// rootsAlone checks that only root can change what dir names: every directory on
// the way to it, as written and as its links resolve, is root's and writable by
// nobody else (a sticky one, like /tmp, only by an entry's own owner, which the next
// step checks). A check by path is sound here because nothing it passes can change
// without root.
func rootsAlone(dir string) error {
	resolved, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return named(dir, err)
	}
	for _, path := range [][]string{ancestors(filepath.Dir(dir)), ancestors(resolved)} {
		for _, at := range path {
			info, err := os.Lstat(at)
			if err != nil {
				return named(dir, err)
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				// Its directory, checked just before, is what keeps it fixed.
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			writable := info.Mode().Perm()&0o022 != 0 && info.Mode()&fs.ModeSticky == 0
			if !ok || st.Uid != 0 || writable {
				return fmt.Errorf("%s is root's, and box reaches it through %s, which someone other than root can change: a link there could have named any directory of root's", dir, at)
			}
		}
	}
	return nil
}

// ancestors is dir and every directory above it, from / down.
func ancestors(dir string) []string {
	var out []string
	for at := filepath.Clean(dir); ; at = filepath.Dir(at) {
		out = append([]string{at}, out...)
		if at == filepath.Dir(at) {
			return out
		}
	}
}

// ownsAnchor is the rule for the directory root's work is anchored on: the seedbox's
// user's, so a swapped parent can only have led box into that user's own files, or
// root's, which OpenOwned then holds to rootsAlone.
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
