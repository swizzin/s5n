// Package work is where a migrate run keeps the files it has to extract, check
// and rewrite: a private directory beside the snapshot, never in the system's
// temporary directory.
//
// $TMPDIR is not a disk. On a Fedora or an Arch host it is a tmpfs, which is RAM:
// a snapshot's state can be gigabytes (a real source's autobrr config was 8.5
// GiB, and a Plex tree is tens of gigabytes), and asking the kernel for that much
// memory either fails the run or takes the host down. The snapshot's own
// directory is a disk by construction -- the snapshot is already on it -- so the
// work goes beside it and is removed when the run ends.
package work

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// New makes one: a 0700 directory under under, which is the snapshot file's own
// directory, and the function that removes it and everything in it.
//
// under is created 0700 when it is not there: box's own snapshot directory
// (/var/lib/s5n/migrate) does not exist on a host that has never taken one, and
// the first run is the one that makes it.
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
