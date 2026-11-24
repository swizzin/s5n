// Package space is the two numbers a migration's one large decision is made
// from: how many bytes a directory takes, and how much room a filesystem has
// left. `box migrate` copies a source's configs into a snapshot and then into a
// seedbox, and Plex's config directory is a whole library: on a real box that is
// tens of gigabytes, so the copy is only made where it fits.
//
// The walk is a du: one stat per entry, the entry's own size for a regular file,
// and nothing read. The free space is statfs's unprivileged count (Bavail), the
// same number df reports as available, because root's reserve is not room a
// migration may count on.
package space

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// marginFloor is the least room a copy leaves free beside itself: a filesystem
// with no bytes left cannot be written to at all, and the app's own directories
// above the tree take a little.
const marginFloor = 256 << 20

// Size is the bytes of the regular files under dir, skipping any directory whose
// slash path relative to dir is in skip and everything beneath it. It is a
// walk with one stat per entry and no reads, so it is the quick answer a
// decision before a copy can afford.
//
// A symlink counts as nothing: the archive and the staging copy carry it as a
// link, so its target's bytes are not written again.
func Size(dir string, skip []string) (int64, error) {
	_, bytes, err := Tally(dir, skip)
	return bytes, err
}

// Tally is Size with the count of files too: the two numbers a copy of the same
// tree would report, from one walk instead of the copy.
func Tally(dir string, skip []string) (files, bytes int64, err error) {
	c := &counter{dir: dir, skip: skipSet(skip)}
	if err := filepath.WalkDir(dir, c.entry); err != nil {
		return 0, 0, err
	}
	return c.files, c.bytes, nil
}

// counter is one Tally walk: what it counts and what it does not walk into.
type counter struct {
	dir   string
	skip  map[string]bool
	files int64
	bytes int64
}

// entry is one entry the walk met. A name that is gone by the time the walk asks
// what it is is not counted and is not a failure: a migration is read off a
// source that is still running, and a running app rewrites its settings through a
// temporary file and a rename -- Plex's Preferences.xml.tmp.7133... exists in a
// directory listing and is gone a moment later. The bytes are no longer there to
// copy, so there is nothing to size. The tree itself, and a directory the walk
// cannot read, are still refusals.
func (c *counter) entry(p string, d fs.DirEntry, err error) error {
	if err != nil {
		if p != c.dir && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	rel, err := filepath.Rel(c.dir, p)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return nil
	}
	if c.skip[rel] {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if !d.Type().IsRegular() {
		return nil
	}
	// The entry's own stat, taken once by the walk: a Plex tree is a million
	// small files, and a second syscall each is what makes this slow enough to
	// notice.
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	c.files++
	c.bytes += info.Size()
	return nil
}

// skipSet is the paths a Size walk does not go into, cleaned so a caller's
// spelling of one matches the walk's.
func skipSet(skip []string) map[string]bool {
	if len(skip) == 0 {
		return nil
	}
	out := make(map[string]bool, len(skip))
	for _, s := range skip {
		if s = path.Clean(filepath.ToSlash(s)); s != "." && s != "" {
			out[s] = true
		}
	}
	return out
}

// Free is the bytes a filesystem has left for an unprivileged writer at path.
// A path that is not there yet is the path it would be created at, so the walk
// goes up to the nearest directory that exists: an app's config directory is
// created by the install, and the question is asked before it is.
func Free(path string) (int64, error) {
	at, err := existing(path)
	if err != nil {
		return 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(at, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// existing is the nearest directory at or above path.
func existing(path string) (string, error) {
	for {
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				return filepath.Dir(path), nil
			}
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(strings.TrimRight(path, "/"))
		if parent == path || parent == "." || parent == "" {
			return "", errors.New("no directory above " + path + " to ask for free space")
		}
		path = parent
	}
}

// Same reports whether two paths are on one filesystem, asked the way Free asks
// it: a path that is not there yet is its nearest existing directory's. A run
// that writes the snapshot and the seedbox's copy on one filesystem needs the
// room for both.
func Same(a, b string) (bool, error) {
	da, err := device(a)
	if err != nil {
		return false, err
	}
	db, err := device(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// device is the filesystem a path is on.
func device(path string) (uint64, error) {
	at, err := existing(path)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(at)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device for " + at)
	}
	return uint64(st.Dev), nil
}

// Margin is the room a copy leaves spare beside the tree it writes: a tenth of
// the tree, and never less than 256 MiB.
func Margin(size int64) int64 {
	if tenth := size / 10; tenth > marginFloor {
		return tenth
	}
	return marginFloor
}

// Place is one filesystem a run writes a tree to, and how many copies of it land
// there: a host that is both ends of a migration writes the snapshot and the
// seedbox's copy on one filesystem, so the room has to be there twice.
type Place struct {
	Path   string
	Free   int64
	Copies int
}

// Disk is where a run asks about room: how many bytes a filesystem has left, and
// whether two paths are on one. Real is the host box runs on; a test hands over
// its own answers so a short disk, or two of them, is a case it can build.
type Disk interface {
	Free(path string) (int64, error)
	Same(a, b string) (bool, error)
}

// Real is the filesystems of the host box runs on.
type Real struct{}

// Free is the unprivileged room at path.
func (Real) Free(path string) (int64, error) { return Free(path) }

// Same is whether two paths are on one filesystem.
func (Real) Same(a, b string) (bool, error) { return Same(a, b) }

// Verdict is what a run decided about a tree: the bytes it needs, margin
// included, and the first place that cannot hold them, if any.
type Verdict struct {
	Need  int64
	Short *Place
}

// Fits reports whether every place can hold the copies it is asked for.
func (v Verdict) Fits() bool { return v.Short == nil }

// Choose is the decision: a tree of size bytes fits when every place has room
// for the copies of it that land there. A Short place names where it does not,
// with the numbers a report says.
func Choose(size int64, places []Place) Verdict {
	need := size + Margin(size)
	v := Verdict{Need: need}
	for i := range places {
		p := places[i]
		copies := int64(p.Copies)
		if copies < 1 {
			copies = 1
		}
		if p.Free < need*copies {
			v.Short = &p
			return v
		}
	}
	return v
}

// Human is a byte count the way box's lines say it: "512 bytes", "494.0 MiB".
func Human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	v, exp := float64(n)/unit, 0
	for v >= unit && exp < 3 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", v, []string{"KiB", "MiB", "GiB", "TiB"}[exp])
}
