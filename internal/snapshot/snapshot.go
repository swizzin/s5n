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

// dirMode is how box creates the snapshot tree and any config directory a
// restore has to put back. The snapshots sit under a config root owned by the
// seedbox's uid, and root writes them: 0750 keeps the snapshot out of the app's
// reach, so an app cannot corrupt the copy that would restore it.
const dirMode fs.FileMode = 0o750

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
	// Files are in path order, so two manifests of the same state compare
	// equal and a diff of one is readable.
	Files []File `json:"files"`
}

// Take copies every regular file under configDir that matches a glob into dest,
// which must not exist yet, and writes the manifest beside the copies. The tree
// is built at dest+".partial" and renamed, so dest is either a complete
// snapshot or absent.
//
// A symlink that matches is a refusal: following it would copy something
// outside the app's config directory, and a restore would then write through
// it. So is a symlinked directory a glob names files under: the walk does not
// follow it, and the snapshot would hold everything except those files. Only
// regular files are copied, and a glob that matches nothing at all is a refusal
// too: a snapshot that restores nothing must not look like one that worked.
func Take(configDir string, globs []string, dest string) (Manifest, error) {
	if err := CheckGlobs(globs); err != nil {
		return Manifest{}, err
	}
	info, err := os.Stat(configDir)
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot: config directory: %w", err)
	}
	if !info.IsDir() {
		return Manifest{}, fmt.Errorf("snapshot: config directory %s is not a directory", configDir)
	}
	if _, err := os.Lstat(dest); err == nil {
		return Manifest{}, fmt.Errorf("snapshot %s exists; box never overwrites one", dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dest, err)
	}

	partial := dest + ".partial"
	// A partial tree is a snapshot whose run died before the rename. It was
	// never complete, so it is ours to replace rather than a reason to refuse:
	// the next run would otherwise never get past a crash.
	if err := os.RemoveAll(partial); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partial, err)
	}
	if err := os.MkdirAll(partial, dirMode); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", partial, err)
	}
	man, err := writeTree(configDir, globs, partial)
	if err != nil {
		os.RemoveAll(partial)
		return Manifest{}, err
	}
	if err := os.Rename(partial, dest); err != nil {
		os.RemoveAll(partial)
		return Manifest{}, fmt.Errorf("snapshot %s: %w", dest, err)
	}
	return man, nil
}

// writeTree copies the matching files under configDir into partial and writes
// the manifest there. It is the half of Take that owns the temporary tree, so a
// failure anywhere in it is cleaned up by the only caller.
func writeTree(configDir string, globs []string, partial string) (Manifest, error) {
	entries, err := matchEntries(configDir, globs)
	if err != nil {
		return Manifest{}, err
	}
	man := Manifest{App: filepath.Base(configDir), Taken: time.Now().UTC(), Files: make([]File, 0, len(entries))}
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
		f, err := copyFile(configDir, partial, e.path)
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
	if err := os.WriteFile(filepath.Join(partial, ManifestName), append(data, '\n'), 0o600); err != nil {
		return Manifest{}, fmt.Errorf("snapshot %s: %w", configDir, err)
	}
	return man, nil
}

// copyFile copies one relative path from configDir into the snapshot tree,
// keeping its mode, mtime and (as root) its owner. It streams, because a Plex
// library database is bigger than the memory of a small box: the hash is
// computed as the bytes go past.
func copyFile(configDir, partial, rel string) (File, error) {
	src := filepath.Join(configDir, filepath.FromSlash(rel))
	info, err := os.Lstat(src)
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	defer in.Close()
	dst := filepath.Join(partial, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	// A copy inside the partial tree needs no temporary name of its own: the
	// whole tree is renamed into place at the end, so nothing reads these files
	// until they are complete.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	fail := func(err error) (File, error) {
		out.Close()
		os.Remove(dst)
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, sum), in)
	if err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		return fail(err)
	}
	// The apps are stopped for a snapshot, so this cannot happen on the path
	// box drives: but a size that moved is a copy of neither version, and the
	// manifest's sha256 would describe neither either.
	if n != info.Size() {
		os.Remove(dst)
		return File{}, fmt.Errorf("snapshot: %s changed while it was copied (%d bytes, the file says %d)", src, n, info.Size())
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		os.Remove(dst)
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		os.Remove(dst)
		return File{}, fmt.Errorf("snapshot: %w", err)
	}
	uid, gid := owner(info)
	// Only root can hand a file to the seedbox's user, which is what makes the
	// restored file the app's rather than root's. A run that is not root owns
	// what it copies already, and the manifest still records the truth.
	if os.Geteuid() == 0 {
		if err := os.Chown(dst, uid, gid); err != nil {
			os.Remove(dst)
			return File{}, fmt.Errorf("snapshot: %w", err)
		}
	}
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
	info, err := os.Stat(configDir)
	if err != nil {
		return fmt.Errorf("restore: config directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("restore: config directory %s is not a directory", configDir)
	}
	man, err := readManifest(src)
	if err != nil {
		return err
	}
	// The manifest decides what is written, so it is verified in full before
	// the first removal: a refusal here leaves the config directory exactly as
	// the failed update left it, which the operator can still inspect.
	if err := verify(src, man); err != nil {
		return err
	}
	// This refusal comes before the first removal like the others, so it too
	// leaves the config directory as the failed update left it.
	for _, f := range man.Files {
		link, err := linkedDir(configDir, f.Path)
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if link != "" {
			return fmt.Errorf("restore %s: %s is a symlink, and %s would be written through it; a restore never writes through a link", configDir, link, f.Path)
		}
	}
	if err := removeExtras(configDir, globs, man); err != nil {
		return err
	}
	for _, f := range man.Files {
		if err := restoreFile(configDir, src, f); err != nil {
			return err
		}
	}
	return nil
}

// readManifest reads a snapshot's manifest. A manifest that lists no files is a
// refusal rather than an empty restore, because Restore's next step deletes
// every file the globs match: an unreadable manifest would read as "the app had
// nothing", which is the one answer that loses data.
func readManifest(src string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(src, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("restore %s: no readable %s, so it is not a snapshot box took: %w", src, ManifestName, err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return Manifest{}, fmt.Errorf("restore %s: %s: %w", src, ManifestName, err)
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
func verify(src string, man Manifest) error {
	for _, f := range man.Files {
		if err := checkRel(f.Path); err != nil {
			return fmt.Errorf("restore %s: manifest: %w", src, err)
		}
		path := filepath.Join(src, filepath.FromSlash(f.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("restore %s: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore %s: %s is no longer a regular file", src, f.Path)
		}
		if info.Size() != f.Size {
			return fmt.Errorf("restore %s: %s is %d bytes, the manifest says %d; the snapshot changed after it was taken", src, f.Path, info.Size(), f.Size)
		}
		sum, err := hashFile(path)
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
func removeExtras(configDir string, globs []string, man Manifest) error {
	held := make(map[string]bool, len(man.Files))
	for _, f := range man.Files {
		held[f.Path] = true
	}
	entries, err := matchEntries(configDir, globs)
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
		if err := os.Remove(filepath.Join(configDir, filepath.FromSlash(e.path))); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return nil
}

// restoreFile writes one snapshot file back into the config directory, through
// internal/atomicfile so a crash leaves the old file or the new one and never
// half of either, then restores the mode, mtime and (as root) the owner.
func restoreFile(configDir, src string, f File) error {
	in, err := os.Open(filepath.Join(src, filepath.FromSlash(f.Path)))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer in.Close()
	target := filepath.Join(configDir, filepath.FromSlash(f.Path))
	if err := makeDirs(configDir, path.Dir(f.Path), f.UID, f.GID); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := atomicfile.Copy(target, in, fs.FileMode(f.Mode)); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if err := os.Chtimes(target, f.ModTime, f.ModTime); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	// A restored file belongs to the seedbox's user, not to the root process
	// that put it back. A non-root run can only leave what it owns, so it sets
	// no owner; the manifest's owner is still what it recorded.
	if os.Geteuid() == 0 {
		if err := os.Chown(target, f.UID, f.GID); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}
	return nil
}

// linkedDir names the first directory between configDir and rel that is a
// symlink, or "" when none is. A directory that is missing ends the search:
// makeDirs creates it and everything below it as real directories.
//
// A check and then a write is sound only because nothing else is writing: the
// apps are scaled to zero for a restore.
func linkedDir(configDir, rel string) (string, error) {
	segs := strings.Split(rel, "/")
	at := configDir
	for i, seg := range segs[:len(segs)-1] {
		at = filepath.Join(at, seg)
		info, err := os.Lstat(at)
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

// makeDirs creates the directories between configDir and a restored file that
// are missing and (as root) hands each one it made to the file's owner: a
// directory left as root's would lock the app out of the file inside it. A
// directory that is already there is the app's, and is left as it is.
func makeDirs(configDir, relDir string, uid, gid int) error {
	if relDir == "." {
		return nil
	}
	at := configDir
	for _, seg := range strings.Split(relDir, "/") {
		at = filepath.Join(at, seg)
		err := os.Mkdir(at, dirMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(at, uid, gid); err != nil {
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

// matchEntries lists the entries under configDir whose relative path matches a
// glob, in path order. Directories are not entries: a snapshot holds files, and
// a glob that names a directory names nothing to copy.
//
// The walk does not follow links, so a symlink is reported where it sits rather
// than copied from wherever it points, and one that stands where a glob names
// files below it is reported too (entry.above). A directory no glob reaches
// below is not read at all: Plex keeps its metadata bundles beside its
// databases, and that tree is most of the files on a media server's config
// claim.
func matchEntries(configDir string, globs []string) ([]entry, error) {
	var out []entry
	err := filepath.WalkDir(configDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == configDir {
			return nil
		}
		rel, err := filepath.Rel(configDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
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

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
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
