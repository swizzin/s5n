// Package space is the two numbers a migration's one large decision is made from:
// how many bytes a directory takes and how much room a filesystem has left,
// because Plex's config is a whole library. The walk is a du (one stat per entry,
// nothing read); free space is statfs's unprivileged Bavail, what df calls
// available, since root's reserve is not room a migration may count on.
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

// marginFloor is the least room a copy leaves free beside itself: an empty
// filesystem cannot be written to at all.
const marginFloor = 256 << 20

// Size is the bytes of the regular files under dir, skipping any directory in
// skip (slash path relative to dir) and everything beneath it. A symlink counts as
// nothing: it is carried as a link, so its target's bytes are not written.
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

// entry is one entry the walk met. A name gone by the time the walk asks what it
// is counts nothing and is not a failure: the source is still running, and an app
// rewriting its settings through a temporary file makes one. The tree itself, and
// a directory the walk cannot read, are still refusals.
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
	// The walk's own stat: a second syscall per file is what makes a million-file
	// tree slow enough to notice.
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

// skipSet is the paths a Size walk does not enter, cleaned to the walk's
// spelling.
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

// Free is the bytes a filesystem has left for an unprivileged writer at path; a
// path not there yet is answered by its nearest existing directory, because an
// install creates the config directory after the question is asked.
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

// Same reports whether two paths are on one filesystem, a missing one answered by
// its nearest existing directory: one filesystem holding both copies needs room
// for both.
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

// Margin is the room a copy leaves spare: a tenth of the tree, at least 256 MiB.
func Margin(size int64) int64 {
	if tenth := size / 10; tenth > marginFloor {
		return tenth
	}
	return marginFloor
}

// Place is one filesystem a run writes a tree to, and how many copies land there:
// a host that is both ends writes both on one filesystem.
type Place struct {
	Path   string
	Free   int64
	Copies int
}

// Disk is where a run asks about room. Real is the host box runs on; a test hands
// over its own answers to build a short disk.
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

// Choose fits a tree of size bytes when every place has room for the copies that
// land there; a Short place names where it does not.
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
