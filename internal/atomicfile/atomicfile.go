// Package atomicfile writes a file so that a crash mid-write leaves the old one
// whole. WriteIn and CopyIn do it through an os.Root, so a link planted in a
// tree the caller does not own cannot steer the write out of it.
package atomicfile

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// rename is os.Rename, replaced by a test proving a failed rename leaves the
// target and no temporary file behind.
var rename = os.Rename

// Write replaces path with data, atomically. The parent directory must exist.
func Write(path string, data []byte, mode fs.FileMode) error {
	return Copy(path, bytes.NewReader(data), mode)
}

// Copy is Write for a stream, so a multi-gigabyte restore is never held in
// memory.
func Copy(path string, src io.Reader, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, mode.Perm()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	// The rename is only durable once the directory holding the name is.
	if d, err := os.Open(dir); err == nil {
		// A failed directory fsync is deliberately not reported: the bytes are
		// already in place.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// WriteIn is Write inside root. Every step goes through the os.Root, because a
// target in a tree someone else can rewrite (a seedbox's config root) could have
// a link swapped in after the caller's check, and a path-based write would
// follow it out.
func WriteIn(root *os.Root, name string, data []byte, mode fs.FileMode) error {
	return CopyIn(root, name, bytes.NewReader(data), mode)
}

// CopyIn is WriteIn for a stream.
func CopyIn(root *os.Root, name string, src io.Reader, mode fs.FileMode) error {
	dir, base := filepath.Dir(name), filepath.Base(name)
	tmp, err := tempName(dir, base)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	// O_EXCL refuses a file planted at the random name rather than following it.
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	// A failed write leaves nothing behind; the random name means a leftover is
	// never picked up again.
	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := root.Chmod(tmp, mode.Perm()); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := root.Rename(tmp, name); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	committed = true
	// The rename is what makes the new bytes visible; it is only durable once
	// the directory that holds the name is.
	if d, err := root.Open(dir); err == nil {
		// A failed directory fsync after a successful rename is deliberately not
		// reported: the bytes are already in place under the new name.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// tempName is random so two runs writing one path cannot meet mid-write.
func tempName(dir, base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(b[:])), nil
}

// File is a file being written whole by the caller. Commit fsyncs, chmods and
// renames it into place; a File that is Discarded, or whose commit fails, leaves
// no name behind.
type File struct {
	f      *os.File
	path   string
	tmp    string
	mode   fs.FileMode
	closed bool
}

// Create starts a file at path, which must not exist: box never overwrites a
// snapshot that may be the only copy. The parent directory must exist.
func Create(path string, mode fs.FileMode) (*File, error) {
	switch _, err := os.Lstat(path); {
	case err == nil:
		return nil, fmt.Errorf("%s already exists: box does not overwrite one", path)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &File{f: f, path: path, tmp: f.Name(), mode: mode.Perm()}, nil
}

// Write sends p to the temporary file.
func (f *File) Write(p []byte) (int, error) {
	if f.closed {
		return 0, errors.New("write " + f.path + ": the file is finished")
	}
	return f.f.Write(p)
}

// Commit makes the file appear at path, whole: fsync, chmod, rename, and the
// directory synced so the rename outlives a crash.
func (f *File) Commit() error {
	if f.closed {
		return errors.New("write " + f.path + ": the file is finished")
	}
	fail := func(err error) error {
		f.Discard()
		return err
	}
	if err := f.f.Sync(); err != nil {
		return fail(fmt.Errorf("write %s: %w", f.path, err))
	}
	if err := f.f.Close(); err != nil {
		return fail(fmt.Errorf("write %s: %w", f.path, err))
	}
	if err := os.Chmod(f.tmp, f.mode); err != nil {
		return fail(fmt.Errorf("write %s: %w", f.path, err))
	}
	if err := rename(f.tmp, f.path); err != nil {
		return fail(fmt.Errorf("write %s: %w", f.path, err))
	}
	f.closed = true
	if d, err := os.Open(filepath.Dir(f.path)); err == nil {
		// A failed directory fsync after a successful rename is deliberately not
		// reported: the bytes are already in place under the new name.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Discard throws the bytes away; it is safe after Commit and safe twice.
func (f *File) Discard() {
	if f.closed {
		return
	}
	f.closed = true
	f.f.Close()
	os.Remove(f.tmp)
}
