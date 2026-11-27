package apply

// stage.go is step 2: an app's config directory copied verbatim into staging. The
// copy is generic -- stale layouts and backups ride along inert, since excluding
// one would be deciding what the app reads. A device, socket, fifo or escaping
// symlink is refused by name before anything is written, since each could reach
// outside the config the pod can see.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/swizzin/s5n/internal/linktree"
)

// Staged is what a copy cost, printed before it starts: a Plex tree is tens of
// gigabytes.
type Staged struct {
	App   string
	Files int64
	Bytes int64
}

// copyTree copies src into dst, which must not exist: modes and mtimes kept, every
func copyTree(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, false, true)
}

// copyForCheck copies a tree for a read-only pass, from a source that may still be
func copyForCheck(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, true, true)
}

// measureTree copies for measurement only: no owner is needed, and it is what the
func measureTree(dst, src string) (Staged, error) {
	return copy(dst, src, 0, 0, false, false)
}

// copyFiles copies the named files with their directories, under copyTree's rules;
// a missing one is not a refusal, since a -wal exists only while the database is
func copyFiles(dst, src string, files []string, uid, gid int) (Staged, error) {
	c := &copier{dst: dst, src: src, uid: uid, gid: gid, skipKernel: true, own: true}
	return c.files(src, files)
}

// measureFiles is copyFiles for a pass that throws the copy away: the same files,
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

// copyNamed makes the directories above one named file and copies it.
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

// copy is the walk, with kernel files left out or refused.
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

// chown hands one entry to the seedbox's user; print is how a symlink is owned
// (Lchown) so the copy never follows it.
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
	// skipKernel leaves a device, socket or fifo out; only a running source has one.
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
//
// info is the Lstat the caller decided on, so the open must reach that same file:
// O_NOFOLLOW refuses a link swapped in since, O_NONBLOCK keeps a fifo from holding
// the open, and the descriptor's own stat must still be info's regular file.
func (c *copier) copyFile(dst, src string, info fs.FileInfo) (int64, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return 0, fmt.Errorf("%s changed while it was being copied: migrate copies the file it checked, so run apply again", src)
	}
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

// copyLink copies a symlink under internal/linktree's rule, so staging and a
// snapshot agree. Following a link would make two copies that drift; an absolute
// inside link is written relative because the tree moves. The log link to
// /proc/self/fd/1 is left out: a kernel filesystem is never a config file, and
// refusing it made every real qBittorrent uncopyable.
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

// emptyTree reports whether path holds only directories: a box-made rename target
// (internal/wiring/dirs.go) may be removed, while an absent one is not empty.
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

// removeEmptyTree removes empty directories only: os.Remove fails if one stopped
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
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// exists reports whether anything is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
