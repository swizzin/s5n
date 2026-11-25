package apply

// stage.go is step 2: one app's config directory copied verbatim into the
// staging tree.
//
// The copy is generic on purpose. Apply knows no per-app file lists; an app's
// config directory is whatever the app wrote there, and the rewriter for that
// app decides what inside it matters (internal/migrate/rewrite). A stale layout
// beside the current one, an old backup, a log from last week: they ride along
// inert, because excluding one of them would be this package deciding what an
// app reads.
//
// What the copy will not carry is anything the app's own process could use to
// write outside its config directory: a device, a socket or a fifo is a
// message to another process, and a symlink that leaves the directory would
// after the move name a path on the source host that the pod cannot see. Both
// are refused, by name, before anything is written.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/linktree"
)

// Staged is what one instance's copy cost: how many files, and how many bytes
// of them. The summary prints it because a Plex config tree is tens of
// gigabytes of metadata, and the operator should see that before the copy
// starts rather than after.
type Staged struct {
	App   string
	Files int64
	Bytes int64
}

// copyTree copies src into dst, which must not exist, and returns what it
// copied.
//
// Modes and modification times are preserved, so an app that compares either
// sees the same tree it left; every entry is chowned to uid:gid, because the
// pod's user is not whoever ran box. The directory's own times are set after
// its children, or writing them would move it again.
func copyTree(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, false, true)
}

// copyForCheck copies a config tree for a pass that only reads it (Settle's
// checks), which is taken from a source that may still be running: a socket or
// fifo the app holds open is left out rather than refused, because a reader can
// do nothing with one and a running source always has one (qBittorrent's
// ipc-socket). A snapshot's own copy applies the same rule, and reports what it
// left (internal/archive).
func copyForCheck(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, true, true)
}

// measureTree copies src into dst for a pass that only measures it: the copy is
// thrown away, so nothing is handed to the seedbox's user and no owner is
// needed. It is what the summary before an install counts, and the one
// difference from what that run then stages.
func measureTree(dst, src string) (Staged, error) {
	return copy(dst, src, 0, 0, false, false)
}

// copyFiles copies the named files out of src into dst, which must not exist,
// with the directories above them: one app's state out of a config directory that
// holds its whole library as well (apply's room for a full carry, carry.go).
//
// The rules are copyTree's -- modes and times kept, every entry owned by uid:gid,
// links carried as links -- and a named file that is not there is not a refusal:
// a database's -wal exists only while something has it open.
func copyFiles(dst, src string, files []string, uid, gid int) (Staged, error) {
	c := &copier{dst: dst, src: src, uid: uid, gid: gid, skipKernel: true, own: true}
	return c.files(src, files)
}

// measureFiles is copyFiles for a pass that throws the copy away: the same files,
// counted, with no owner handed to anybody (measureTree's rule).
func measureFiles(dst, src string, files []string) (Staged, error) {
	c := &copier{dst: dst, src: src, skipKernel: true}
	return c.files(src, files)
}

// files is the copy of a named set: everything above it made, every file under
// the same rules as a whole tree, and the directories' own times set last.
func (c *copier) files(src string, files []string) (Staged, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return Staged{}, err
	}
	if !info.IsDir() {
		return Staged{}, fmt.Errorf("%s is not a directory", src)
	}
	dst := c.dst
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return c.st, fmt.Errorf("create %s: %w", dst, err)
	}
	c.dirs = append(c.dirs, dirTime{dst, info})
	made := map[string]bool{}
	for _, rel := range files {
		if err := c.copyNamed(rel, made); err != nil {
			return c.st, err
		}
	}
	if err := c.finishDirs(); err != nil {
		return c.st, err
	}
	return c.st, nil
}

// finishDirs is the last thing a copy does to its directories: own them, give
// them back the mode the widening took, and set the times, because a directory's
// own mtime is the last child put in it.
func (c *copier) finishDirs() error {
	for _, m := range c.dirs {
		if err := c.chown(m.path, false); err != nil {
			return fmt.Errorf("own %s: %w", m.path, err)
		}
		if err := os.Chmod(m.path, m.info.Mode().Perm()); err != nil {
			return fmt.Errorf("chmod %s: %w", m.path, err)
		}
		if err := os.Chtimes(m.path, m.info.ModTime(), m.info.ModTime()); err != nil {
			return fmt.Errorf("touch %s: %w", m.path, err)
		}
	}
	return nil
}

// copyNamed makes the directories above one named file and copies the file. A
// directory above it that is not there means the file is not either.
func (c *copier) copyNamed(rel string, made map[string]bool) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if made[parent] {
			continue
		}
		from := filepath.Join(c.src, filepath.FromSlash(parent))
		info, err := os.Lstat(from)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: %s is not a directory, so nothing can be carried under it", c.src, parent)
		}
		into := filepath.Join(c.dst, filepath.FromSlash(parent))
		if err := os.Mkdir(into, info.Mode().Perm()|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", into, err)
		}
		c.dirs = append(c.dirs, dirTime{into, info})
		made[parent] = true
	}
	from := filepath.Join(c.src, filepath.FromSlash(rel))
	info, err := os.Lstat(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	into := filepath.Join(c.dst, filepath.FromSlash(rel))
	switch {
	case info.Mode().IsRegular():
		n, err := c.copyFile(into, from, info)
		if err != nil {
			return err
		}
		c.st.Files++
		c.st.Bytes += n
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		return c.copyLink(into, from, rel)
	}
	// A directory or a kernel file where a state file was expected: the app's
	// rewriter is what refuses that, in its own words.
	return nil
}

// copy is every copy: the same walk, with the kernel's own files either left out
// or refused, and with or without an owner.
func copy(dst, src string, uid, gid int, skipKernel, own bool) (Staged, error) {
	c := &copier{dst: dst, src: src, uid: uid, gid: gid, skipKernel: skipKernel, own: own}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return Staged{}, fmt.Errorf("create %s: %w", dst, err)
	}
	if err := filepath.WalkDir(src, c.entry); err != nil {
		return c.st, err
	}
	if err := c.finishDirs(); err != nil {
		return c.st, err
	}
	return c.st, nil
}

// chown hands one entry of the copy to the seedbox's user, and does nothing at
// all when this copy has no owner (measureTree). link is a symlink, which is
// owned with Lchown so the copy never follows it.
func (c *copier) chown(path string, link bool) error {
	if !c.own {
		return nil
	}
	if link {
		return os.Lchown(path, c.uid, c.gid)
	}
	return os.Chown(path, c.uid, c.gid)
}

// copier is one walk's state: where the copy goes, who owns it, and the
// directories whose mtime has to wait for their children.
type copier struct {
	dst, src string
	uid, gid int
	st       Staged
	dirs     []dirTime
	// skipKernel leaves a device, socket or fifo out instead of refusing it:
	// only a copy taken from a running source meets one.
	skipKernel bool
	// own is false for a copy nobody keeps: measureTree's.
	own bool
}

// dirTime is one directory of the copy, and the times it has to end up with.
type dirTime struct {
	path string
	info fs.FileInfo
}

// entry copies one entry the walk met.
func (c *copier) entry(path string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(c.src, path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if rel == "." {
		c.dirs = append(c.dirs, dirTime{c.dst, info})
		return nil
	}
	target := filepath.Join(c.dst, rel)
	switch {
	case d.IsDir():
		// The mode is widened while the children are written and restored
		// afterwards: a directory the app made read-only would otherwise
		// refuse the copy of its own contents.
		if err := os.Mkdir(target, info.Mode().Perm()|0o700); err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		c.dirs = append(c.dirs, dirTime{target, info})
		return nil
	case info.Mode().IsRegular():
		n, err := c.copyFile(target, path, info)
		if err != nil {
			return err
		}
		c.st.Files++
		c.st.Bytes += n
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		return c.copyLink(target, path, rel)
	}
	if c.skipKernel && info.Mode()&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeSocket|fs.ModeNamedPipe|fs.ModeIrregular) != 0 {
		return nil
	}
	return fmt.Errorf("%s is a %s: migrate copies files, directories and links inside the config directory, not devices or sockets",
		path, modeName(info.Mode()))
}

// copyFile copies one regular file, preserving its mode and mtime and owning it
// as the release's uid. It returns the bytes written.
func (c *copier) copyFile(dst, src string, info fs.FileInfo) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("copy %s: %w", src, err)
	}
	if err := c.chown(dst, false); err != nil {
		return n, fmt.Errorf("own %s: %w", dst, err)
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return n, fmt.Errorf("touch %s: %w", dst, err)
	}
	return n, nil
}

// copyLink copies one symlink, and refuses one that resolves outside the config
// directory. The rule itself is internal/linktree's, because a snapshot's
// config/ tree is copied under the same one: what a staging copy would carry and
// a snapshot would not (or the other way round) would make the two trees differ.
//
// A link is copied as a link: the app reads it the same way, and following it
// would turn one file into two copies that then drift. An absolute link that
// points inside the directory is rewritten to the relative form, because the
// tree it names is about to move and the absolute text would still name the
// source host's path after the move.
//
// One link is not a path at all and is left out of the copy: the images point
// their log at the container's own stdout, so a qBittorrent config directory
// holds qBittorrent/logs/qbittorrent.log as a link to /proc/self/fd/1. /proc,
// /dev and /sys are the pod's own kernel filesystems, never somewhere a config
// tree keeps a file, and the app writes the link again on start. Refusing it
// made every real qBittorrent install uncopyable (hack/e2e-box.sh, migrate).
func (c *copier) copyLink(dst, src, rel string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	res, err := linktree.Resolve(c.src, rel, target)
	if err != nil {
		return err
	}
	if res.Skip {
		return nil
	}
	if err := os.Symlink(res.Target, dst); err != nil {
		return fmt.Errorf("link %s: %w", dst, err)
	}
	if err := c.chown(dst, true); err != nil {
		return fmt.Errorf("own %s: %w", dst, err)
	}
	return nil
}

// modeName names a file's type the way a refusal reads it.
func modeName(m fs.FileMode) string {
	switch {
	case m&fs.ModeDevice != 0 && m&fs.ModeCharDevice != 0:
		return "character device"
	case m&fs.ModeDevice != 0:
		return "block device"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	}
	return "special file"
}

// emptyDir reports whether path is a directory with nothing in it. A path that
// is not a directory is not empty: the caller refuses it rather than deleting
// something it did not create.
func emptyDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

// emptyTree reports whether path holds nothing but directories, recursively: no
// file, symlink or special file anywhere under it. That is a rename target this
// box made itself -- the wiring Job creates /data/{downloads,media}/<category>
// for every Arr the release runs (internal/wiring/dirs.go) -- and a target with
// nothing in it is one the rename may remove. A path that is not there is not
// empty, like emptyDir: the caller then has nothing to remove and renames into
// the name.
func emptyTree(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return false, nil
		}
		ok, err := emptyTree(filepath.Join(path, e.Name()))
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// removeEmptyTree removes path and every empty directory under it, and never a
// file: each removal is os.Remove, which fails on a directory that stopped being
// empty after emptyTree proved it, so a file that appears in between is an error
// rather than something deleted. Only an emptyTree caller may pass its answer
// here.
func removeEmptyTree(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return fmt.Errorf("%s holds %s, which is not a directory", filepath.Join(path, e.Name()), e.Name())
		}
		if err := removeEmptyTree(filepath.Join(path, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// isDir reports whether path is a directory, without an error: callers are
// asking a yes/no question about a path the operator named.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// exists reports whether anything is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
