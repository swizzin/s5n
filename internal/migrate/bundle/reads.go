package bundle

// reads.go lists the snapshots a directory holds with the plan each carries, for
// one question: was this source read before? A cutover compares its read against
// the newest earlier read of the same source, because an app deleted since is
// invisible now (docs/box.md, "migrate"). Only plan.yaml is read, so a directory
// of final snapshots is answered without extracting any.

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

// Reads lists every snapshot, newest first, with its plan. A file that is not a
// snapshot is left alone and an unreadable one stepped over, as Newest does. A
// missing dir is no snapshots, which is not an error.
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
