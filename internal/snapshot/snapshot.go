// Package snapshot copies the app state an image upgrade can rewrite, so a failed
// update can put it back. Updates run forward-only migrations, so a step copies
// the files a catalog entry's `snapshot:` globs name before the release moves, and
// rollback puts them back. It is not backup: one update step, on the config
// claim's own filesystem, and nothing ages a snapshot out.
//
// Take builds dest+".partial" and renames the whole tree into place, so a crash
// leaves nothing a Restore would trust; Restore writes one file at a time through
// internal/atomicfile into a live config directory, so a crash leaves each file
// old or new, never truncated, and it verifies the whole snapshot against the
// manifest before it touches anything.
//
// This runs as root over a config root the seedbox's user owns, so every path under
// one goes through an os.Root opened on that root (tree.go): the checks below refuse
// a link with a sentence an operator can act on, and the root is what holds when the
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
	// Path is relative to the app's config directory, slash-separated, and is the
	// manifest's key: what Restore writes back, and what the globs match.
	Path string `json:"path"`
	// Size and SHA256 are the verification Restore does before it writes anything:
	// a snapshot file that changed on disk is a refusal, not a copy.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Mode holds the permission bits, and UID/GID their owner, as they were when
	// the copy was taken: a restored file must look like the one the app wrote,
	// not like the root process that restored it.
	Mode uint32 `json:"mode"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// ModTime is restored too: an app comparing a settings file with the database
	// reads a bumped mtime as a change it has not seen.
	ModTime time.Time `json:"mtime"`
}

// Manifest is what one app's snapshot holds.
type Manifest struct {
	// App is the config directory's name, which is the instance name.
	App string `json:"app"`
	// Taken is when the copy was made, in UTC.
	Taken time.Time `json:"taken"`
	// Image is what the app ran when the copy was taken, and MovedTo is what the step
	// moved it to: a rollback needs both, and nothing else says which image wrote these
	// files once the release has moved. A snapshot that predates both has them zero, and
	// a caller decides what that means.
	Image   ImageRef `json:"image"`
	MovedTo ImageRef `json:"movedTo"`
	// Globs are the globs the copy was taken with, and a restore uses exactly
	// these: the catalog naming more files since would have it delete files the
	// snapshot never held.
	Globs []string `json:"globs,omitempty"`
	// Files are in path order, so two manifests of the same state compare equal.
	Files []File `json:"files"`
}

// Take copies every regular file under configDir that matches a glob into dest, which
// must not exist yet, and writes the manifest beside the copies; the tree is built at
// dest+".partial" and renamed, so dest is either complete or absent.
//
// Both images are recorded before the first file is copied, because neither the step
// nor the release remembers them once the release has moved.
//
// A matching symlink is a refusal (following it would copy from outside the config
// directory, and a restore would write through it), and so is a symlinked directory a
// glob names files under. A glob that matches nothing is a refusal too: a snapshot
// that restores nothing must not look like one that worked.
func Take(configDir string, globs []string, dest string, images Images) (Manifest, error) {
	if checkImage(images.Ran) != nil || checkImage(images.MovedTo) != nil {
		return Manifest{}, errors.New("snapshot: the image the app ran and the image it moves to are both needed")
	}
	if err := CheckGlobs(globs); err != nil {
		return Manifest{}, err
	}
	// The app's state and the snapshot tree beside it are both under the config
	// root, so one tree reaches both; opening the root rather than either directory
	// is what makes a link planted at the config directory a refusal instead of the
	// tree's own root.
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
	// An app's config directory is a direct child of the config root, and that is
	// what makes its owner the seedbox user's ids.
	own, err := treeOwnerOf(t)
	if err != nil {
		return Manifest{}, err
	}

	partial, partialPath := destName+".partial", dest+".partial"
	// A partial tree is a snapshot whose run died before the rename: never complete,
	// so it is ours to replace rather than a reason to refuse, or the next run would
	// never get past a crash.
	if err := t.removeAll(partial); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partialPath, err)
	}
	if err := makeDirIn(t, partial, own); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partialPath, err)
	}
	// A take that fails leaves nothing a restore could mistake for a snapshot. A
	// failed cleanup is not the error worth reporting; the next run replaces
	// whatever is left.
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

// writeTree copies the matching files under the app's config directory into partial
// and writes the manifest there; Take owns the temporary tree and cleans it up.
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
		// Following a link would copy something outside the app's config directory,
		// and a restore would then write through it; anything that is not a regular
		// file has no content a snapshot could hold.
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
	// The manifest is box's own file, so it is readable by the seedbox's user: their
	// box reads it to find what a rollback restores.
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

// copyFile copies one relative path into the snapshot tree, keeping its mode, mtime
// and (as root) its owner. It streams -- a Plex library database is bigger than a
// small box's memory -- hashing as the bytes go past.
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
	// No temporary name is needed inside the partial tree: the whole tree is renamed
	// into place, so nothing reads these files until they are complete.
	out, err := t.create(dst, info.Mode().Perm())
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	// A copy that fails is not left half-written for a later step to read; a failed
	// remove is not the error worth reporting, the copy's own is.
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
	// The apps are stopped for a snapshot, so this cannot happen on the path box
	// drives: but a size that moved is a copy of neither version, and the manifest's
	// sha256 would describe neither either.
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
	// restored file the app's rather than root's; a run that is not root owns what
	// it copies already, and the manifest still records the truth.
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

// Restore puts an app's snapshot back. It verifies the snapshot against its manifest
// first and refuses on any mismatch before it touches the config directory: half a
// restored database is worse than the failed update it undoes.
//
// It then removes what the globs match that the snapshot does not hold (a `-wal` the
// failed run created, which SQLite would replay over the restored file) and writes
// each snapshot file back through internal/atomicfile.
//
// The failed app ran as the seedbox's user and could leave a link where a directory
// was, so a symlinked directory on a snapshot file's way down is a refusal.
func Restore(configDir string, globs []string, src string) error {
	if err := CheckGlobs(globs); err != nil {
		return err
	}
	// The snapshot lives under the same config root as the app it restores: box took
	// it there, and one tree reaches both halves.
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
	// The manifest decides what is written, so it is verified in full before the
	// first removal: a refusal leaves the config directory as the failed update left
	// it, which the operator can still inspect.
	if err := verify(t, snap, src, man); err != nil {
		return err
	}
	// Like the others, this refusal comes before the first removal.
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

// loadManifest reads a snapshot's manifest without deciding whether it holds enough
// to restore: Newest needs the images a snapshot names even from a step a restore
// would refuse, so the "lists no files" rule stays with Restore.
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

// readManifest reads a snapshot's manifest for a restore. A manifest that lists no
// files is a refusal rather than an empty restore, because Restore's next step
// deletes every file the globs match: "the app had nothing" is the one answer
// that loses data.
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

// verify checks every file the manifest names: present, a regular file, the recorded
// size, and the recorded sha256. A manifest path is checked to be strictly inside
// the config directory, because the manifest tells Restore where to write.
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

// removeExtras deletes the files the globs match that the snapshot does not hold: a
// `-wal` or `-shm` the failed run left describes a database that no longer exists,
// and SQLite would replay it over the restored file.
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
		// A link a glob reaches below is not a match: nothing under it was walked,
		// and the link itself is not this function's to remove.
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

// restoreFile writes one snapshot file back through internal/atomicfile, so a crash
// leaves the old file or the new one and never half of either, then restores the
// mode, mtime and (as root) the owner.
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
	// A restored file belongs to the seedbox's user, not to the root process that put
	// it back; a non-root run can only leave what it owns, so it sets no owner.
	if os.Geteuid() == 0 {
		if err := t.chown(target, f.UID, f.GID); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return nil
}

// linkedDir names the first directory between the app's config directory and rel that
// is a symlink, or "" when none is; a missing directory ends the search, because
// makeDirs creates it as a real directory.
//
// A link inside the config root is refused too, although the tree would let the write
// through: a restore that followed one would put the app's state where the app does
// not look for it.
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

// makeDirs creates the missing directories between the app's config directory and a
// restored file at dirMode, and (as root) hands each one it made to the file's
// owner: a directory left as root's would lock the app out of the file inside it.
// A directory that is already there is the app's.
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

// entry is one path under a config directory that a glob matched, with the type bits
// of what is there, so the caller decides what to do with it.
type entry struct {
	path string
	mode fs.FileMode
	// above marks a symlink that matches no glob itself but sits where a glob
	// names files below it. Take refuses it; it is never a file to remove.
	above bool
}

// matchEntries lists the entries under the app's config directory whose relative path
// matches a glob, in path order; directories are not entries.
//
// The walk does not follow links: a symlink is reported where it sits, and one where a
// glob names files below it is reported too (entry.above), or the walk would refuse
// it. A directory no glob reaches below is not read at all: Plex's metadata bundles
// are most of the files on a media server's config claim.
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

// reachesBelow reports whether a glob names something under rel: it has more
// segments than rel, and its leading ones match rel's.
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

// matches reports whether one relative path matches any of the globs. Each glob is
// matched against the whole path, so `*` never crosses a directory separator and
// there is no `**`: what a glob can reach is what a reviewer sees in the catalog.
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

// owner reads a file's uid and gid. A filesystem whose stat box cannot read reports
// -1/-1, which the manifest records: nothing to restore, rather than a guess.
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
