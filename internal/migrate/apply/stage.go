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
	"sort"
	"strings"
	"syscall"

	"github.com/swizzin/s5n/internal/linktree"
	"github.com/swizzin/s5n/internal/safefs"
)

// beforeHand and beforeDirs are test seams: the first runs as one copied file has
// been written and is about to get its owner and mode, the second once the walk is
// done and the directories are given theirs. Both are the moments the copy's tree
// can change under it.
var (
	beforeHand = func(file string) error { return nil }
	beforeDirs = func(dir string) error { return nil }
)

// Staged is what a copy cost, printed before it starts: a Plex tree is tens of
// gigabytes.
type Staged struct {
	App   string
	Files int64
	Bytes int64
}

// copyTree copies src into dst, which must not exist: modes and mtimes kept, every
// file and directory owned by uid:gid, and kernel files refused.
func copyTree(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, false, true)
}

// copyForCheck copies a tree for a read-only pass, from a source that may still be
// running: a device, socket or fifo is left out rather than refused.
func copyForCheck(dst, src string, uid, gid int) (Staged, error) {
	return copy(dst, src, uid, gid, true, true)
}

// measureTree copies for measurement only: no owner is needed, and it is what a
// pass before an install does to count a tree it does not keep.
func measureTree(dst, src string) (Staged, error) {
	return copy(dst, src, 0, 0, false, false)
}

// copyFiles copies the named files with their directories, under copyTree's rules;
// a missing one is not a refusal, since a -wal exists only while the database has
// it open.
func copyFiles(dst, src string, files []string, uid, gid int) (Staged, error) {
	c, err := newCopier(dst, src, uid, gid, true, true)
	if err != nil {
		return Staged{}, err
	}
	defer c.close()
	return c.files(src, files)
}

// measureFiles is copyFiles for a pass that throws the copy away: the same files,
// counted.
func measureFiles(dst, src string, files []string) (Staged, error) {
	c, err := newCopier(dst, src, 0, 0, true, false)
	if err != nil {
		return Staged{}, err
	}
	defer c.close()
	return c.files(src, files)
}

// newCopier opens both trees. The one written stays box's own, 0700, until the last
// entry is in it, and every read and write below goes through its os.Root: the
// source is the seedbox's user's, and the copy's own tree hangs off a config root
// it can write, so a path resolved twice is a link followed once.
func newCopier(dst, src string, uid, gid int, skipKernel, own bool) (*copier, error) {
	from, err := safefs.Open(src)
	if err != nil {
		return nil, err
	}
	to, err := safefs.Make(dst, 0o700)
	if err != nil {
		from.Close()
		return nil, fmt.Errorf("create %s: %w", dst, err)
	}
	return &copier{src: from, dst: to, uid: uid, gid: gid, skipKernel: skipKernel, own: own}, nil
}

// files is the copy of a named set: everything above it made, every file under
// the same rules as a whole tree, and the directories' own times set last.
func (c *copier) files(src string, files []string) (Staged, error) {
	info, err := c.src.Root().Stat(".")
	if err != nil {
		return c.st, c.src.Err(".", err)
	}
	if !info.IsDir() {
		return c.st, fmt.Errorf("%s is not a directory", src)
	}
	c.held = append(c.held, step{".", info, false})
	made := map[string]bool{}
	for _, rel := range files {
		if err := c.copyNamed(rel, made); err != nil {
			return c.st, err
		}
	}
	if err := c.handOff(); err != nil {
		return c.st, err
	}
	return c.st, nil
}

// copy is the walk, with kernel files left out or refused.
func copy(dst, src string, uid, gid int, skipKernel, own bool) (Staged, error) {
	c, err := newCopier(dst, src, uid, gid, skipKernel, own)
	if err != nil {
		return Staged{}, err
	}
	defer c.close()
	if err := fs.WalkDir(c.src.Root().FS(), ".", c.entry); err != nil {
		return c.st, err
	}
	if err := c.handOff(); err != nil {
		return c.st, err
	}
	return c.st, nil
}

// copier is one copy: the tree it reads, the tree it writes, and the entries whose
// owner, mode and times wait for the end.
type copier struct {
	src, dst *safefs.Tree
	uid, gid int
	st       Staged
	// held is every directory and link waiting for the end, by name inside the
	// copy.
	held []step
	// skipKernel leaves a device, socket or fifo out; only a running source has one.
	skipKernel bool
	// own is false for a copy nobody keeps: measureTree's.
	own bool
}

// step is one entry of the copy whose owner, mode and times are given once nothing
// more is written: a directory's all three, a link's owner alone.
type step struct {
	rel  string
	info fs.FileInfo
	link bool
}

// close releases both trees.
func (c *copier) close() {
	c.src.Close()
	c.dst.Close()
}

// copyNamed makes the directories above one named file and copies it.
func (c *copier) copyNamed(rel string, made map[string]bool) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if made[parent] {
			continue
		}
		info, err := c.src.Root().Lstat(parent)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return c.src.Err(parent, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: %s is not a directory, so nothing can be carried under it", c.src.Dir(), parent)
		}
		if err := c.dst.Root().Mkdir(parent, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", c.dst.Path(parent), c.dst.Err(parent, err))
		}
		c.held = append(c.held, step{parent, info, false})
		made[parent] = true
	}
	info, err := c.src.Root().Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return c.src.Err(rel, err)
	}
	switch {
	case info.Mode().IsRegular():
		n, err := c.copyFile(rel, rel, info)
		if err != nil {
			return err
		}
		c.st.Files++
		c.st.Bytes += n
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		return c.copyLink(rel, rel)
	}
	// A directory or a kernel file where a state file was expected: the app's
	// rewriter is what refuses that, in its own words.
	return nil
}

// entry copies one entry the walk met.
func (c *copier) entry(rel string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	info, err := d.Info()
	if err != nil {
		return c.src.Err(rel, err)
	}
	if rel == "." {
		c.held = append(c.held, step{".", info, false})
		return nil
	}
	switch {
	case d.IsDir():
		// 0700 while the children are written, and the source's mode once they
		// are: a directory the app made read-only would otherwise refuse the copy
		// of its own contents.
		if err := c.dst.Root().Mkdir(rel, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", c.dst.Path(rel), c.dst.Err(rel, err))
		}
		c.held = append(c.held, step{rel, info, false})
		return nil
	case info.Mode().IsRegular():
		n, err := c.copyFile(rel, rel, info)
		if err != nil {
			return err
		}
		c.st.Files++
		c.st.Bytes += n
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		return c.copyLink(rel, rel)
	}
	if c.skipKernel && info.Mode()&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeSocket|fs.ModeNamedPipe|fs.ModeIrregular) != 0 {
		return nil
	}
	return fmt.Errorf("%s is a %s: migrate copies files, directories and links inside the config directory, not devices or sockets",
		c.src.Path(rel), modeName(info.Mode()))
}

// copyFile copies one regular file, preserving its mode and mtime and owning it as
// the release's uid. It returns the bytes written.
//
// info is the Lstat the caller decided on, so the open must reach that same file:
// O_NOFOLLOW refuses a link swapped in since, O_NONBLOCK keeps a fifo from holding
// the open, and the descriptor's own stat must still be info's regular file. The
// copy is owned and given its mode through that same descriptor, before it is
// closed, so a destination swapped for a link after it was created is not followed.
func (c *copier) copyFile(dst, src string, info fs.FileInfo) (int64, error) {
	in, err := c.src.Root().OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, c.src.Err(src, err)
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return 0, fmt.Errorf("%s changed while it was being copied: migrate copies the file it checked, so run apply again", c.src.Path(src))
	}
	out, err := c.dst.Root().OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", c.dst.Path(dst), c.dst.Err(dst, err))
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = beforeHand(c.dst.Path(dst))
	}
	if err == nil && c.own {
		if cerr := out.Chown(c.uid, c.gid); cerr != nil {
			err = fmt.Errorf("own %s: %w", c.dst.Path(dst), cerr)
		}
	}
	if err == nil {
		if cerr := out.Chmod(info.Mode().Perm()); cerr != nil {
			err = fmt.Errorf("chmod %s: %w", c.dst.Path(dst), cerr)
		}
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("copy %s: %w", c.src.Path(src), err)
	}
	if err := c.dst.Root().Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return n, fmt.Errorf("touch %s: %w", c.dst.Path(dst), c.dst.Err(dst, err))
	}
	return n, nil
}

// copyLink copies a symlink under internal/linktree's rule, so staging and a
// snapshot agree. Following a link would make two copies that drift; an absolute
// inside link is written relative because the tree moves. The log link to
// /proc/self/fd/1 is left out: a kernel filesystem is never a config file, and
// refusing it made every real qBittorrent uncopyable.
func (c *copier) copyLink(dst, src string) error {
	target, err := c.src.Root().Readlink(src)
	if err != nil {
		return c.src.Err(src, err)
	}
	res, err := linktree.Resolve(c.src.Dir(), filepath.FromSlash(src), target)
	if err != nil {
		return err
	}
	if res.Skip {
		return nil
	}
	if err := c.dst.Root().Symlink(res.Target, dst); err != nil {
		return fmt.Errorf("link %s: %w", c.dst.Path(dst), c.dst.Err(dst, err))
	}
	c.held = append(c.held, step{dst, nil, true})
	return nil
}

// handOff is the last thing a copy does: it gives every directory and link to the
// seedbox's user, deepest first, and the copy's own directory last, with the mode
// and times the source had. Until then the tree is box's own and 0700, so the user
// cannot reach in to swap what is being written, and no chown or chmod is done
// through a parent it already owns.
func (c *copier) handOff() error {
	if err := beforeDirs(c.dst.Dir()); err != nil {
		return err
	}
	order := make([]step, 0, len(c.held))
	order = append(order, c.held...)
	sort.SliceStable(order, func(i, j int) bool {
		ti, tj := order[i].rel == ".", order[j].rel == "."
		if ti != tj {
			return tj
		}
		di, dj := strings.Count(order[i].rel, "/"), strings.Count(order[j].rel, "/")
		if di != dj {
			return di > dj
		}
		return order[i].rel < order[j].rel
	})
	for _, s := range order {
		if err := c.give(s); err != nil {
			return err
		}
	}
	return nil
}

// give gives one entry to the owner, sets its mode and time, and never follows a
// link: a directory that is one by now is not the directory that was copied.
func (c *copier) give(s step) error {
	if s.rel == "." {
		mode := s.info.Mode().Perm()
		if c.own {
			if err := c.dst.Give(c.uid, c.gid, mode); err != nil {
				return fmt.Errorf("own %s: %w", c.dst.Dir(), err)
			}
		} else if err := c.dst.Root().Chmod(".", mode); err != nil {
			return fmt.Errorf("chmod %s: %w", c.dst.Dir(), c.dst.Err(".", err))
		}
		if err := c.dst.Root().Chtimes(".", s.info.ModTime(), s.info.ModTime()); err != nil {
			return fmt.Errorf("touch %s: %w", c.dst.Dir(), c.dst.Err(".", err))
		}
		return nil
	}
	root := c.dst.Root()
	if c.own {
		if err := root.Lchown(s.rel, c.uid, c.gid); err != nil {
			return fmt.Errorf("own %s: %w", c.dst.Path(s.rel), c.dst.Err(s.rel, err))
		}
	}
	if s.link {
		return nil
	}
	if err := root.Chmod(s.rel, s.info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", c.dst.Path(s.rel), c.dst.Err(s.rel, err))
	}
	if err := root.Chtimes(s.rel, s.info.ModTime(), s.info.ModTime()); err != nil {
		return fmt.Errorf("touch %s: %w", c.dst.Path(s.rel), c.dst.Err(s.rel, err))
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
// being empty, and that is the answer rather than a reason to force it.
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
// asking whether there is a config there at all.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// exists reports whether anything is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
