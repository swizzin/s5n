package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tree is a config root opened as an os.Root, with the path it was opened on.
//
// Everything this package reads, writes, creates, renames, removes, chmods or
// chowns under a config root goes through a tree. The whole of that root belongs
// to the seedbox's user -- .box and the snapshots in it, every app's config
// directory, every file an app wrote -- and box takes snapshots, restores them
// and writes the holds there as root. An Lstat before a write narrows the window
// between the check and the use; the root is what closes it, because it refuses
// any name that resolves outside the tree it opened, however the link that leads
// out got there.
//
// A link that stays inside the tree is the user's own business and keeps
// working: their files, their link. The refusals for the ordinary case (a link a
// glob matches, a link where a restore would write through) stay where they are,
// so the operator reads a sentence about what is wrong rather than a path error.
//
// The directory is kept beside the root for one reason: os.Root reports the name
// inside the tree ("sonarr/app.db"), and the message an operator needs names the
// path on the host.
type tree struct {
	root *os.Root
	dir  string
}

// openTree opens the config root at dir. The directory is resolved the way any
// other path is: box names a config root it was told about, and it is the tree
// below that name this type confines.
func openTree(dir string) (*tree, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &tree{root: root, dir: dir}, nil
}

// openTreeAround opens the tree a take or a restore works inside: the config root
// that holds the app's config directory, and nothing wider. The other path the
// call was given (the snapshot it writes or reads) has to be inside that same
// root, or the call is refused before anything is opened under it.
//
// Every path box constructs satisfies this: a snapshot lives at
// <configRoot>/.box/snapshots/<step>/<app>, beside the app's config directory.
// The refusal is the package's guarantee rather than its callers' discipline,
// because the whole tree belongs to the seedbox's user and box walks it as root:
// a root widened to reach a path outside the config root would be a tree the
// user's links could not leave only by luck.
func openTreeAround(appDir, other string) (*tree, error) {
	dir := filepath.Dir(appDir)
	rel, err := filepath.Rel(dir, other)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("snapshot: %s is not inside the config root %s", other, dir)
	}
	return openTree(dir)
}

// Close releases the root's handle on the directory. Every exported operation
// closes the tree it opened before it returns.
func (t *tree) Close() error {
	return t.root.Close()
}

// rel spells one of box's own paths as a name inside the tree, refusing a path
// that is not under it. Box computes these paths from its own config, so one
// outside the config root is a caller's mistake, and a refusal here reads better
// than the root's own refusal three calls later. The root would refuse it anyway.
func (t *tree) rel(path string) (string, error) {
	rel, err := filepath.Rel(t.dir, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("is not inside %s", t.dir)
	}
	return rel, nil
}

// path is how a name inside the tree is spelled on the host, for the messages
// box prints.
func (t *tree) path(name string) string {
	return filepath.Join(t.dir, filepath.FromSlash(name))
}

// err names the host path in a failed call: os.Root reports the name inside the
// tree ("openat sonarr/app.db"), which says nothing about where the file is on
// the host the operator is looking at.
func (t *tree) err(name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && !filepath.IsAbs(pe.Path) {
		return &fs.PathError{Op: pe.Op, Path: t.path(name), Err: pe.Err}
	}
	return err
}

func (t *tree) stat(name string) (fs.FileInfo, error) {
	info, err := t.root.Stat(filepath.FromSlash(name))
	if err != nil {
		return nil, t.err(name, err)
	}
	return info, nil
}

func (t *tree) lstat(name string) (fs.FileInfo, error) {
	info, err := t.root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, t.err(name, err)
	}
	return info, nil
}

func (t *tree) readFile(name string) ([]byte, error) {
	data, err := t.root.ReadFile(filepath.FromSlash(name))
	if err != nil {
		return nil, t.err(name, err)
	}
	return data, nil
}

func (t *tree) writeFile(name string, data []byte, mode fs.FileMode) error {
	if err := t.root.WriteFile(filepath.FromSlash(name), data, mode); err != nil {
		return t.err(name, err)
	}
	return nil
}

func (t *tree) readDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(t.root.FS(), filepath.ToSlash(name))
	if err != nil {
		return nil, t.err(name, err)
	}
	return entries, nil
}

func (t *tree) mkdir(name string, mode fs.FileMode) error {
	if err := t.root.Mkdir(filepath.FromSlash(name), mode); err != nil {
		return t.err(name, err)
	}
	return nil
}

func (t *tree) chmod(name string, mode fs.FileMode) error {
	if err := t.root.Chmod(filepath.FromSlash(name), mode); err != nil {
		return t.err(name, err)
	}
	return nil
}

func (t *tree) chown(name string, uid, gid int) error {
	if err := t.root.Chown(filepath.FromSlash(name), uid, gid); err != nil {
		return t.err(name, err)
	}
	return nil
}

func (t *tree) chtimes(name string, atime, mtime time.Time) error {
	if err := t.root.Chtimes(filepath.FromSlash(name), atime, mtime); err != nil {
		return t.err(name, err)
	}
	return nil
}

func (t *tree) open(name string) (*os.File, error) {
	f, err := t.root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, t.err(name, err)
	}
	return f, nil
}

// create starts a file that must not be there yet: the caller owns the name, and
// an existing one -- a link planted at it most of all -- is a refusal.
func (t *tree) create(name string, mode fs.FileMode) (*os.File, error) {
	f, err := t.root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return nil, t.err(name, err)
	}
	return f, nil
}

func (t *tree) rename(oldName, newName string) error {
	if err := t.root.Rename(filepath.FromSlash(oldName), filepath.FromSlash(newName)); err != nil {
		return t.err(newName, err)
	}
	return nil
}

func (t *tree) remove(name string) error {
	if err := t.root.Remove(filepath.FromSlash(name)); err != nil {
		return t.err(name, err)
	}
	return nil
}

// removeAll removes a tree, and removes a link rather than the tree it points at:
// the root never follows a name out of itself, so a link planted where box left a
// .partial tree costs the link and nothing else.
func (t *tree) removeAll(name string) error {
	if err := t.root.RemoveAll(filepath.FromSlash(name)); err != nil {
		return t.err(name, err)
	}
	return nil
}
