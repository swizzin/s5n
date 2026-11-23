package bundle

// reads.go lists the snapshots a directory holds with the plan each one carries.
//
// It exists for one question: was this source read before? A cutover compares its
// own read against the newest earlier read of the same source, because an app the
// operator deleted before the earlier read is invisible now, and only a read taken
// while the source was whole can say it was ever there (docs/box.md, "migrate").
//
// Only plan.yaml is read out of each file (archive.ReadEntry), so a directory of
// final snapshots -- each one a config tree's worth of bytes -- is answered
// without extracting any of them.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/swizzin/s5n/internal/archive"
)

// Read is one snapshot in a directory, as choosing an earlier read needs it.
type Read struct {
	// Path is the snapshot file.
	Path string
	// Mode is what the source was when it was read.
	Mode archive.Mode
	// Taken is the time the name carries, which is when the source was read.
	Taken time.Time
	// Plan is plan.yaml's bytes, exactly as the snapshot holds them.
	Plan []byte
}

// Reads lists every snapshot in dir, newest first by the time its name carries,
// with the plan each one holds.
//
// A file that is not a snapshot is left alone, and one that cannot be read is
// stepped over rather than being the directory's answer: a broken copy stays
// where it is for the operator to look at, as Newest already does. A missing dir
// is no snapshots at all -- the first run on a host has none, and that is not an
// error.
func Reads(dir string) ([]Read, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isSnapshotName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := make([]Read, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		m, err := archive.Read(path)
		if err != nil {
			continue
		}
		raw, err := archive.ReadEntry(path, archive.PlanName)
		if err != nil || len(raw) == 0 {
			continue
		}
		at, _ := nameStamp(name)
		out = append(out, Read{Path: path, Mode: m.Mode, Taken: at, Plan: raw})
	}
	return out, nil
}
