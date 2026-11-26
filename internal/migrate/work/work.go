// Package work holds a migrate run's private working copies: beside the
// snapshot, never in $TMPDIR, which is a tmpfs on some hosts and a source's
// state can be gigabytes.
package work

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// New makes a 0700 directory under under, the snapshot file's own directory,
// which it creates 0700 when it is not there, and the function that removes it.
func New(under string) (string, func(), error) {
	none := func() {}
	if under == "" {
		return "", none, errors.New("no directory for a working copy: a migrate run keeps what it extracts beside its snapshot")
	}
	if info, err := os.Stat(under); err == nil {
		if !info.IsDir() {
			return "", none, fmt.Errorf("a private directory beside %s: it is not a directory", under)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(under, 0o700); err != nil {
			return "", none, fmt.Errorf("a private directory beside %s: %w", under, err)
		}
	} else {
		return "", none, fmt.Errorf("a private directory beside %s: %w", under, err)
	}
	dir, err := os.MkdirTemp(under, "box-work-")
	if err != nil {
		return "", none, fmt.Errorf("a private directory in %s: %w", under, err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}
