// Package atomicfile writes a file so that a crash mid-write leaves the old
// one whole.
//
// Box writes five kinds of file it cannot afford to half-write: a seedbox's
// grant and values (where a lost edit is a lost grant), the cluster decision it
// re-reads on every later run, the two app configs the image seeds credentials
// into, and an app's database a failed update is putting back. The robust
// version was written for the first case and the next three grew their own: a
// fixed temporary name, which two runs at once would interleave on, and no
// fsync, which lets the rename reach the directory before the bytes are
// durable. This is that write once, and Copy is the same write for a file too
// big to hold in memory. Create is the same write held open while the caller
// produces the bytes: a snapshot's archive is a tar + gzip stream, not a buffer.
//
// WriteIn and CopyIn are the same write performed through an os.Root, for a
// target inside a tree someone else can rewrite: the root is what keeps a link
// they plant from steering the temporary file, the chmod or the rename out of it.
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

// rename is os.Rename. A test replaces it to prove that a failed rename leaves
// the target exactly as it was and no temporary file behind.
var rename = os.Rename

// Write replaces path with data: a temporary file beside it, fsynced, chmodded
// to mode, renamed over it, and the directory fsynced so the rename itself
// outlives a crash. The parent directory must exist; the callers that create it
// do so with the mode they want, which differs (0755 for the record, 0750 for
// the app configs).
//
// The temporary name is random rather than fixed, so two box runs writing one
// path cannot meet in the middle of a write.
func Write(path string, data []byte, mode fs.FileMode) error {
	return Copy(path, bytes.NewReader(data), mode)
}

// Copy is Write for content that arrives as a stream. A snapshot restore puts
// an Arr's database back without loading it: an install small enough to run in
// a gigabyte is exactly where reading a multi-gigabyte file into memory would
// fail the rollback that was supposed to save it.
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
	// The rename is what makes the new bytes visible; it is only durable once
	// the directory that holds the name is.
	if d, err := os.Open(dir); err == nil {
		// A failed directory fsync after a successful rename is deliberately not
		// reported: the bytes are already in place under the new name.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// WriteIn is Write inside a tree. It is the same write -- a temporary file
// beside the target, fsynced, chmodded, renamed over it, and the directory
// fsynced -- with every step performed through an os.Root opened on the tree the
// file belongs to.
//
// A caller whose target sits in a directory someone else can rewrite (a
// seedbox's config root belongs to its user) cannot use the path-based Write: the
// temporary file, the chmod and the rename all name a path, and a symlink
// swapped in after the caller's own check would put any of them outside the
// tree. The root refuses every name that resolves outside it, so the write lands
// where the caller said or not at all.
func WriteIn(root *os.Root, name string, data []byte, mode fs.FileMode) error {
	return CopyIn(root, name, bytes.NewReader(data), mode)
}

// CopyIn is WriteIn for content that arrives as a stream: a snapshot restore
// puts an Arr's database back without loading it.
func CopyIn(root *os.Root, name string, src io.Reader, mode fs.FileMode) error {
	dir, base := filepath.Dir(name), filepath.Base(name)
	tmp, err := tempName(dir, base)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	// O_EXCL is what makes the name this run's: an existing file at the random
	// name (a link planted there, most of all) is refused rather than followed.
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	// A write that fails leaves nothing of the file it was making. A failed
	// remove is not the error worth reporting -- the write's own is, and the
	// name is random, so nothing picks a leftover up again.
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

// tempName is the random name the new bytes are written under before the
// rename. It is random rather than fixed for the same reason os.CreateTemp's is:
// two runs writing one path -- root's box and a seedbox's, most of all -- must
// not meet in the middle of a write.
func tempName(dir, base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(b[:])), nil
}

// File is a file being written whole, for content that is produced rather than
// handed over: a snapshot's tar+gzip stream is written by two writers the caller
// owns, so the write is held open until Commit. It is Copy with the copy's two
// halves in the caller's hands.
//
// The bytes go to a temporary file beside path, as in Copy; Commit fsyncs it,
// chmods it to mode and renames it into place, and a File that is Discarded --
// or whose commit fails -- leaves no name behind.
type File struct {
	f      *os.File
	path   string
	tmp    string
	mode   fs.FileMode
	closed bool
}

// Create starts a file at path, which must not exist yet: a name the caller means
// to keep -- a snapshot, most of all -- is never overwritten, because the copy
// under it may be the only one there is. The parent directory must exist.
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

// Write is the content on its way to the temporary file.
func (f *File) Write(p []byte) (int, error) {
	if f.closed {
		return 0, errors.New("write " + f.path + ": the file is finished")
	}
	return f.f.Write(p)
}

// Commit makes the file appear at its path, whole: the bytes are synced, the
// name is the caller's mode, and the directory is synced so the rename outlives
// a crash.
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

// Discard throws the written bytes away and leaves nothing at path. It is safe
// after Commit and safe twice; a caller that abandons a File without discarding
// leaves only the temporary file, whose random name nothing picks up again.
func (f *File) Discard() {
	if f.closed {
		return
	}
	f.closed = true
	f.f.Close()
	os.Remove(f.tmp)
}
