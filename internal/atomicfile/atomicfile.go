// Package atomicfile writes a file so that a crash mid-write leaves the old
// one whole.
//
// Box writes four kinds of file it cannot afford to half-write: a seedbox's
// grant and values (where a lost edit is a lost grant), the cluster decision it
// re-reads on every later run, and the two app configs the image seeds
// credentials into. The robust version was written for the first case and the
// other three grew their own: a fixed temporary name, which two runs at once
// would interleave on, and no fsync, which lets the rename reach the directory
// before the bytes are durable. This is that write once.
package atomicfile

import (
	"fmt"
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
	if _, err := f.Write(data); err != nil {
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
		d.Sync()
		d.Close()
	}
	return nil
}
