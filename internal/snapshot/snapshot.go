// Package snapshot copies the app state an image upgrade can rewrite, so a
// failed update can put it back.
//
// An unattended update runs over forward-only migrations: a new Arr migrates
// its SQLite database, an app rewrites its settings file into a new shape, and
// neither knows how to go back. So a step copies the files the chart says an
// upgrade can change (a catalog entry's `snapshot:` globs) before the release
// moves, and rollback puts them back. It is not backup: one update step, on the
// same filesystem as the config claim, and nothing here ages a snapshot out on
// its own schedule.
//
// A snapshot is a directory, not a set of files. Take copies into dest
// +".partial" and renames the whole tree into place, so a crash halfway through
// leaves nothing a Restore would trust with a live database. Restore goes the
// other way, one file at a time through internal/atomicfile, because it writes
// into a live config directory: a crash mid-restore leaves each file either old
// or new, never truncated.
//
// The manifest is what makes a restore safe to attempt. Restore verifies every
// snapshot file against it, before it touches the config directory, because a
// snapshot that rotted on disk is not something to copy over a working
// database.
//
// All of this runs as root over a config root the seedbox's user owns, so every
// file it reads, writes, creates, renames, removes or chowns under one is reached
// through an os.Root opened on that root (tree.go). The checks below refuse a
// link with a sentence an operator can act on; the root is what holds when the
// link arrives between the check and the use.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// ManifestName is the file inside a snapshot that records what it holds.
const ManifestName = "manifest.json"

// File is one regular file in a snapshot.
type File struct {
	// Path is relative to the app's config directory, slash-separated, and is
	// the manifest's key: it is what Restore writes back and what the globs are
	// matched against.
	Path string `json:"path"`
	// Size and SHA256 are the verification Restore does before it writes
	// anything: a snapshot file that changed on disk is a refusal, not a copy.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Mode holds the permission bits, and UID/GID their owner, as they were
	// when the copy was taken: a restored file must look like the one the app
	// wrote, not like the root process that restored it.
	Mode uint32 `json:"mode"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// ModTime is restored too, because an app that compares a settings file
	// with the database reads a bumped mtime as a change it has not seen.
	ModTime time.Time `json:"mtime"`
}

// Manifest is what one app's snapshot holds.
type Manifest struct {
	// App is the config directory's name, which is the instance name: a
	// snapshot found without its path still says whose state it is.
	App string `json:"app"`
	// Taken is when the copy was made, in UTC.
	Taken time.Time `json:"taken"`
	// Image is what the app ran when the copy was taken, and MovedTo is what
	// the step moved it to. They are what a rollback needs and nothing else
	// keeps: once the release has moved, the box no longer says which image
	// wrote these files. A snapshot taken before box recorded them has both
	// zero, and a caller decides what that means.
	Image   ImageRef `json:"image"`
	MovedTo ImageRef `json:"movedTo"`
	// Globs are the globs the copy was taken with. A restore of this copy
	// uses exactly these: it removes every match the copy lacks, and a
	// catalog that names more files since would have it delete files the
	// snapshot never held. Empty in a snapshot taken before box recorded
	// them, and a caller decides what that means.
	Globs []string `json:"globs,omitempty"`
	// Files are in path order, so two manifests of the same state compare
	// equal and a diff of one is readable.
	Files []File `json:"files"`
}

// Take copies every regular file under configDir that matches a glob into dest,
// which must not exist yet, and writes the manifest beside the copies. The tree
// is built at dest+".partial" and renamed, so dest is either a complete
// snapshot or absent.
//
// The two images are recorded in the manifest before the first file is copied:
// they are what a rollback reads a week later to know what to hold the app at
// and what the hold avoids, and neither the step nor the release remembers them
// once the release has moved. A step that cannot say both is refused before it
// creates anything.
//
// A symlink that matches is a refusal: following it would copy something
// outside the app's config directory, and a restore would then write through
// it. So is a symlinked directory a glob names files under: the walk does not
// follow it, and the snapshot would hold everything except those files. Only
// regular files are copied, and a glob that matches nothing at all is a refusal
// too: a snapshot that restores nothing must not look like one that worked.
func Take(configDir string, globs []string, dest string, images Images) (Manifest, error) {
	if checkImage(images.Ran) != nil || checkImage(images.MovedTo) != nil {
		return Manifest{}, errors.New("snapshot: the image the app ran and the image it moves to are both needed")
	}
	if err := CheckGlobs(globs); err != nil {
		return Manifest{}, err
	}
	// The app's state and the snapshot tree beside it are both under the config
	// root, so one tree reaches both. Opening the root rather than either
	// directory is what makes a link planted at the config directory itself a
	// refusal instead of the tree's own root.
	t, err := openTreeAround(configDir, dest)
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	defer t.Close()
	app, err := t.rel(configDir)
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	info, err := t.stat(app)
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot: config directory: %w", err)
	}
	if !info.IsDir() {
		return Manifest{}, fmt.Errorf("snapshot: config directory %s is not a directory", configDir)
	}
	destName, err := t.rel(dest)
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dest, err)
	}
	if _, err := t.lstat(destName); err == nil {
		return Manifest{}, fmt.Errorf("snapshot %s exists; box never overwrites one", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dest, err)
	}
	// An app's config directory is a direct child of the config root, and that
	// is what makes its owner the seedbox user's ids.
	own, err := treeOwnerOf(t)
	if err != nil {
		return Manifest{}, err
	}

	partial, partialPath := destName+".partial", dest+".partial"
	// A partial tree is a snapshot whose run died before the rename. It was
	// never complete, so it is ours to replace rather than a reason to refuse:
	// the next run would otherwise never get past a crash.
	if err := t.removeAll(partial); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partialPath, err)
	}
	if err := makeDirIn(t, partial, own); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partialPath, err)
	}
	// A take that fails leaves nothing a restore could mistake for a snapshot:
	// the partial tree goes when this call does, and the rename clears the flag.
	// A failed cleanup is not the error worth reporting -- the take's own
	// failure is, and the next run replaces whatever is left.
	complete := false
	defer func() {
		if !complete {
			_ = t.removeAll(partial)
		}
	}()
	man, err := writeTree(t, app, partial, globs, images, own)
	if err != nil {
		return Manifest{}, err
	}
	if err := t.rename(partial, destName); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dest, err)
	}
	complete = true
	return man, nil
}

// writeTree copies the matching files under the app's config directory into
// partial and writes the manifest there. It is the half of Take that owns the
// temporary tree, so a failure anywhere in it is cleaned up by the only caller.
func writeTree(t *tree, app, partial string, globs []string, images Images, own treeOwner) (Manifest, error) {
	configDir := t.path(app)
	entries, err := matchEntries(t, app, globs)
	if err != nil {
		return Manifest{}, err
	}
	man := Manifest{
		App:     path.Base(app),
		Taken:   time.Now().UTC(),
		Image:   images.Ran,
		MovedTo: images.MovedTo,
		Globs:   append([]string(nil), globs...),
		Files:   make([]File, 0, len(entries)),
	}
	for _, e := range entries {
		if e.above {
			return Manifest{}, fmt.Errorf("snapshot %s: %s is a symlink and a glob names files under it; a snapshot copies regular files and never follows a link, so they would be left out", configDir, e.path)
		}
		// Following a link would copy something outside the app's config
		// directory, and a restore would then write through it; anything that
		// is not a regular file has no content a snapshot could hold.
		if e.mode&fs.ModeSymlink != 0 {
			return Manifest{}, fmt.Errorf("snapshot %s: %s is a symlink; a snapshot copies regular files and never follows a link", configDir, e.path)
		}
		if !e.mode.IsRegular() {
			return Manifest{}, fmt.Errorf("snapshot %s: %s matches but is not a regular file", configDir, e.path)
		}
		f, err := copyFile(t, app, partial, e.path, own)
		if err != nil {
			return Manifest{}, err
		}
		man.Files = append(man.Files, f)
	}
	if len(man.Files) == 0 {
		return Manifest{}, fmt.Errorf("snapshot %s: nothing matches %s; a snapshot is taken to be restored, so an empty one is a misconfigured glob", configDir, quoteGlobs(globs))
	}
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	name := path.Join(partial, ManifestName)
	// The manifest is box's own file, not a copy, so it is written readable by
	// the seedbox's user: their box reads it to find what a rollback restores.
	if err := t.writeFile(name, append(data, '\n'), treeFileMode); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	// WriteFile applies the mode subject to the umask; 0640 is the promise, not
	// what the umask allows.
	if err := t.chmod(name, treeFileMode); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	if err := own.ownIn(t, name); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	return man, nil
}

// copyFile copies one relative path from the app's config directory into the
// snapshot tree, keeping its mode, mtime and (as root) its owner. It streams,
// because a Plex library database is bigger than the memory of a small box: the
// hash is computed as the bytes go past.
func copyFile(t *tree, app, partial, rel string, own treeOwner) (File, error) {
	src := path.Join(app, rel)
	info, err := t.lstat(src)
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	in, err := t.open(src)
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	defer in.Close()
	dst := path.Join(partial, rel)
	if err := makeDirIn(t, path.Dir(dst), own); err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	// A copy inside the partial tree needs no temporary name of its own: the
	// whole tree is renamed into place at the end, so nothing reads these files
	// until they are complete.
	out, err := t.create(dst, info.Mode().Perm())
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	// A copy that fails is not left half-written for a later step to read: the
	// deferred remove covers every failure below, and the return clears it. A
	// failed remove is not the error worth reporting -- the copy's own is.
	written := false
	defer func() {
		if !written {
			_ = t.remove(dst)
		}
	}()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, sum), in)
	if err != nil {
		out.Close()
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	if err := out.Close(); err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	// The apps are stopped for a snapshot, so this cannot happen on the path
	// box drives: but a size that moved is a copy of neither version, and the
	// manifest's sha256 would describe neither either.
	if n != info.Size() {
		return File{}, fmt.Errorf("snapshot: %s changed while it was copied (%d bytes, the file says %d)", t.path(src), n, info.Size())
	}
	if err := t.chmod(dst, info.Mode().Perm()); err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	if err := t.chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	uid, gid := owner(info)
	// Only root can hand a file to the seedbox's user, which is what makes the
	// restored file the app's rather than root's. A run that is not root owns
	// what it copies already, and the manifest still records the truth.
	if os.Geteuid() == 0 {
		if err := t.chown(dst, uid, gid); err != nil {
			return File{}, fmt.Errorf("snapshot: %w", err)
		}
	}
	written = true
	return File{
		Path:    rel,
		Size:    n,
		SHA256:  hex.EncodeToString(sum.Sum(nil)),
		Mode:    uint32(info.Mode().Perm()),
		UID:     uid,
		GID:     gid,
		ModTime: info.ModTime(),
	}, nil
}

// Restore puts an app's snapshot back. It verifies the snapshot against its
// manifest first and refuses on any mismatch, before it touches the config
// directory: the only safe order, because half a restored database is worse
// than the failed update it is undoing.
//
// Then it removes what the globs match that the snapshot does not hold -- a
// `-wal` the failed run created, most often, which SQLite would otherwise
// replay over the file just restored -- and writes each snapshot file back
// through internal/atomicfile.
//
// The app that failed ran as the seedbox's user and could leave anything in its
// own directory, a link where a directory was included. Restore writes as
// whoever runs box, so a directory on a snapshot file's way down that is now a
// symlink is a refusal: the write would land wherever the link points.
func Restore(configDir string, globs []string, src string) error {
	if err := CheckGlobs(globs); err != nil {
		return err
	}
	// The snapshot being read lives under the same config root as the app it
	// restores: box took it there, and one tree reaches both halves.
	t, err := openTreeAround(configDir, src)
	if err != nil {
		return fmt.Errorf("restore %s: %w", configDir, err)
	}
	defer t.Close()
	app, err := t.rel(configDir)
	if err != nil {
		return fmt.Errorf("restore %s: %w", configDir, err)
	}
	snap, err := t.rel(src)
	if err != nil {
		return fmt.Errorf("restore %s: %w", src, err)
	}
	info, err := t.stat(app)
	if err != nil {
		return fmt.Errorf("restore: config directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("restore: config directory %s is not a directory", configDir)
	}
	man, err := readManifest(t, snap, src)
	if err != nil {
		return err
	}
	// The manifest decides what is written, so it is verified in full before
	// the first removal: a refusal here leaves the config directory exactly as
	// the failed update left it, which the operator can still inspect.
	if err := verify(t, snap, src, man); err != nil {
		return err
	}
	// This refusal comes before the first removal like the others, so it too
	// leaves the config directory as the failed update left it.
	for _, f := range man.Files {
		link, err := linkedDir(t, app, f.Path)
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if link != "" {
			return fmt.Errorf("restore %s: %s is a symlink, and %s would be written through it; a restore never writes through a link", configDir, link, f.Path)
		}
	}
	if err := removeExtras(t, app, globs, man); err != nil {
		return err
	}
	for _, f := range man.Files {
		if err := restoreFile(t, app, snap, f); err != nil {
			return err
		}
	}
	return nil
}

// loadManifest reads a snapshot's manifest without deciding whether it holds
// enough to restore. Newest needs the images a snapshot names even from a step
// whose copy a restore would refuse, so the "lists no files" rule stays with
// Restore, which is the only caller it protects.
func loadManifest(t *tree, src string) (Manifest, error) {
	data, err := t.readFile(path.Join(src, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("no readable %s, so it is not a snapshot box took: %w", ManifestName, err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestName, err)
	}
	return man, nil
}

// readManifest reads a snapshot's manifest for a restore. A manifest that lists
// no files is a refusal rather than an empty restore, because Restore's next
// step deletes every file the globs match: an unreadable manifest would read as
// "the app had nothing", which is the one answer that loses data.
func readManifest(t *tree, snap, src string) (Manifest, error) {
	man, err := loadManifest(t, snap)
	if err != nil {
		return Manifest{}, fmt.Errorf("restore %s: %w", src, err)
	}
	if len(man.Files) == 0 {
		return Manifest{}, fmt.Errorf("restore %s: %s lists no files; refusing to restore nothing over the app's state", src, ManifestName)
	}
	return man, nil
}

// verify checks every file the manifest names: present, a regular file, the
// recorded size, and the recorded sha256. A manifest path is checked to be
// strictly inside the config directory, because the manifest is what tells
// Restore where to write.
func verify(t *tree, snap, src string, man Manifest) error {
	for _, f := range man.Files {
		if err := checkRel(f.Path); err != nil {
			return fmt.Errorf("restore %s: manifest: %w", src, err)
		}
		name := path.Join(snap, f.Path)
		info, err := t.lstat(name)
		if err != nil {
			return fmt.Errorf("restore %s: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore %s: %s is no longer a regular file", src, f.Path)
		}
		if info.Size() != f.Size {
			return fmt.Errorf("restore %s: %s is %d bytes, the manifest says %d; the snapshot changed after it was taken", src, f.Path, info.Size(), f.Size)
		}
		sum, err := hashFile(t, name)
		if err != nil {
			return fmt.Errorf("restore %s: %w", src, err)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("restore %s: %s does not match the manifest's sha256; the snapshot changed after it was taken", src, f.Path)
		}
	}
	return nil
}

// removeExtras deletes the files the globs match that the snapshot does not
// hold. The common case is a `-wal` or `-shm` the failed run left behind: it
// describes a database that no longer exists, and SQLite would replay it over
// the restored file.
func removeExtras(t *tree, app string, globs []string, man Manifest) error {
	held := make(map[string]bool, len(man.Files))
	for _, f := range man.Files {
		held[f.Path] = true
	}
	entries, err := matchEntries(t, app, globs)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// A link a glob reaches below is not a match: nothing under it was
		// walked, and the link itself is not this function's to remove.
		if held[e.path] || e.above {
			continue
		}
		// Remove the entry itself, never through it: a link at a matching path
		// was not in the snapshot (Take refuses links), so it is this run's.
		if err := t.remove(path.Join(app, e.path)); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return nil
}

// restoreFile writes one snapshot file back into the config directory, through
// internal/atomicfile so a crash leaves the old file or the new one and never
// half of either, then restores the mode, mtime and (as root) the owner.
func restoreFile(t *tree, app, src string, f File) error {
	in, err := t.open(path.Join(src, f.Path))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer in.Close()
	target := path.Join(app, f.Path)
	if err := makeDirs(t, app, path.Dir(f.Path), f.UID, f.GID); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := atomicfile.CopyIn(t.root, target, in, fs.FileMode(f.Mode)); err != nil {
		return fmt.Errorf("restore %s: %w", t.path(target), err)
	}
	if err := t.chtimes(target, f.ModTime, f.ModTime); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	// A restored file belongs to the seedbox's user, not to the root process
	// that put it back. A non-root run can only leave what it owns, so it sets
	// no owner; the manifest's owner is still what it recorded.
	if os.Geteuid() == 0 {
		if err := t.chown(target, f.UID, f.GID); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return nil
}

// linkedDir names the first directory between the app's config directory and rel
// that is a symlink, or "" when none is. A directory that is missing ends the
// search: makeDirs creates it and everything below it as real directories.
//
// A link inside the config root is refused as well, although the tree would let
// the write through: a restore that followed one would put the app's state where
// the app does not look for it. The tree is what holds when the link arrives
// after this walk has read it.
func linkedDir(t *tree, app, rel string) (string, error) {
	segs := strings.Split(rel, "/")
	at := app
	for i, seg := range segs[:len(segs)-1] {
		at = path.Join(at, seg)
		info, err := t.lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return strings.Join(segs[:i+1], "/"), nil
		}
	}
	return "", nil
}

// makeDirs creates the directories between the app's config directory and a
// restored file that are missing, at dirMode, and (as root) hands each one it
// made to the file's owner: a directory left as root's would lock the app out of
// the file inside it. A directory that is already there is the app's, and is
// left as it is.
func makeDirs(t *tree, app, relDir string, uid, gid int) error {
	if relDir == "." {
		return nil
	}
	at := app
	for _, seg := range strings.Split(relDir, "/") {
		at = path.Join(at, seg)
		err := t.mkdir(at, dirMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := t.chmod(at, dirMode); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := t.chown(at, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// entry is one path under a config directory that a glob matched, with the
// type bits of what is there (regular, symlink, directory) so the caller
// decides what to do with it.
type entry struct {
	path string
	mode fs.FileMode
	// above marks a symlink that matches no glob itself but sits where a glob
	// names files below it. Take refuses it; it is never a file to remove.
	above bool
}

// matchEntries lists the entries under the app's config directory whose relative
// path matches a glob, in path order. Directories are not entries: a snapshot
// holds files, and a glob that names a directory names nothing to copy.
//
// The walk is the tree's own file system, so it does not follow links: a symlink
// is reported where it sits rather than copied from wherever it points, and one
// that stands where a glob names files below it is reported too (entry.above), or
// the walk would refuse it. A directory no glob reaches below is not read at all:
// Plex keeps its metadata bundles beside its databases, and that tree is most of
// the files on a media server's config claim.
func matchEntries(t *tree, app string, globs []string) ([]entry, error) {
	rootPath := filepath.ToSlash(app)
	var out []entry
	err := fs.WalkDir(t.root.FS(), rootPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return t.err(p, err)
		}
		if p == rootPath {
			return nil
		}
		// The walk only ever names paths under the directory it started at, so
		// the app-relative name is what follows the prefix.
		rel := strings.TrimPrefix(p, rootPath+"/")
		if d.IsDir() {
			if !reachesBelow(globs, rel) {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case matches(globs, rel):
			out = append(out, entry{path: rel, mode: d.Type()})
		case d.Type()&fs.ModeSymlink != 0 && reachesBelow(globs, rel):
			out = append(out, entry{path: rel, mode: d.Type(), above: true})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// reachesBelow reports whether a glob names something under rel: the glob has
// more segments than rel, and its leading ones match rel's. A `/` in a glob is
// always a separator here, as it is to whoever reads the catalog.
func reachesBelow(globs []string, rel string) bool {
	segs := strings.Split(rel, "/")
	for _, glob := range globs {
		gsegs := strings.Split(glob, "/")
		if len(gsegs) <= len(segs) {
			continue
		}
		below := true
		for i, seg := range segs {
			if ok, err := path.Match(gsegs[i], seg); err != nil || !ok {
				below = false
				break
			}
		}
		if below {
			return true
		}
	}
	return false
}

// matches reports whether one relative path matches any of the globs. Each glob
// is matched against the whole relative path, so `*` never crosses a directory
// separator and there is no `**`: what a glob can reach is exactly what a
// reviewer can see in the catalog.
func matches(globs []string, rel string) bool {
	for _, glob := range globs {
		if ok, err := path.Match(glob, rel); err == nil && ok {
			return true
		}
	}
	return false
}

func hashFile(t *tree, name string) (string, error) {
	f, err := t.open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// owner reads a file's uid and gid. A filesystem whose stat box cannot read
// reports -1/-1, which is what the manifest then records: nothing to restore,
// rather than a guess at root.
func owner(info fs.FileInfo) (uid, gid int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

func quoteGlobs(globs []string) string {
	quoted := make([]string, len(globs))
	for i, g := range globs {
		quoted[i] = fmt.Sprintf("%q", g)
	}
	return strings.Join(quoted, ", ")
}
