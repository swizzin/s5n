package archive

// writer.go is the write half: the caller adds files and trees, Close writes the
// manifest last, and nothing is at the path until then.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/linktree"
)

// Skipped is one link a tree carried that is not in the archive.
type Skipped struct {
	// Path is where it was inside the archive, e.g.
	// config/qbittorrent/logs/qbittorrent.log.
	Path string
	// Target is what the link pointed at.
	Target string
	// Why says what the link is and why it is not carried.
	Why string
}

// Writer writes one archive. A caller that abandons it leaves nothing at the path
// it named: the bytes go to a temporary file beside it, as in internal/atomicfile,
// and only Close renames it into place.
type Writer struct {
	path string
	file *atomicfile.File
	gz   *gzip.Writer
	tw   *tar.Writer
	m    *Manifest
	seen map[string]bool
	// dirs is every path an entry has made a directory: a real directory entry,
	// or the parent of something added. It is what refuses a file where a
	// directory has to be, so a writer cannot produce an archive Open rejects.
	dirs map[string]bool
	done bool
	err  error
}

// Create starts an archive at path, which must not exist. The manifest's Format
// defaults to Format and its Created to now; its Entries are added as content is.
//
// The file is 0600 from the first byte: it holds the source's API keys, the Plex token
// and qBittorrent's password hash.
func Create(path string, m Manifest) (*Writer, error) {
	if len(m.Entries) != 0 {
		return nil, fmt.Errorf("create %s: the manifest's entries are added as content is", path)
	}
	if m.Format == 0 {
		m.Format = Format
	}
	if m.Created == "" {
		m.Created = time.Now().UTC().Format(time.RFC3339)
	}
	m.Entries = make(map[string]Entry)
	if problems := m.problems(); len(problems) > 0 {
		return nil, fmt.Errorf("create %s: %s", path, joinProblems(problems))
	}
	f, err := atomicfile.Create(path, 0o600)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	return &Writer{
		path: path,
		file: f,
		gz:   gz,
		tw:   tar.NewWriter(gz),
		m:    &m,
		seen: make(map[string]bool),
		dirs: make(map[string]bool),
	}, nil
}

func joinProblems(problems []string) string {
	out := problems[0]
	for _, p := range problems[1:] {
		out += "; " + p
	}
	return out
}

// AddFile adds one file the caller holds in memory: a plan, a report, or a small
// config a caller built rather than read.
func (w *Writer) AddFile(name string, data []byte, mode fs.FileMode) error {
	if err := w.ready(name); err != nil {
		return err
	}
	return w.write(name, bytes.NewReader(data), int64(len(data)), mode.Perm(), time.Now().UTC())
}

// AddTree adds dir's contents under prefix ("config/<instance>", "source").
//
// The tree is copied under internal/linktree's rule, the one apply's staging copy
// uses: an inside link is carried, one naming the kernel's own filesystems (an app's
// log at the pod's stdout) is left out and returned in skipped, and one that leaves
// the tree stops the archive, because it would name a path no receiving host has.
//
// Ownership is not recorded: the extracted tree is box's, and apply re-owns what it
// stages.
func (w *Writer) AddTree(prefix, dir string) ([]Skipped, error) {
	return w.AddTreeExcept(prefix, dir, "", nil)
}

// AddTreeExcept adds dir's contents under prefix, with the two limits a full carry of
// an app's config directory needs (internal/migrate/plex): sub names one directory
// inside dir whose contents travel, everything beside it left out, and skip names
// directories to leave out with everything beneath them (the caches Plex rebuilds).
//
// A skip entry that is not a directory inside dir is a refusal, not a silent no-op: a
// path spelled wrong would put a library back into a snapshot somebody left it out of.
func (w *Writer) AddTreeExcept(prefix, dir, sub string, skip []string) ([]Skipped, error) {
	if err := w.ready(prefix); err != nil {
		return nil, err
	}
	if !safeName(prefix) {
		return nil, fmt.Errorf("add %s: a tree's prefix is a path inside the archive", dir)
	}
	if sub != "" && !safeName(sub) {
		return nil, fmt.Errorf("add %s: %q is not a directory inside it", dir, sub)
	}
	// A left-out directory is named relative to the subtree a snapshot carries, which is
	// where the app's data is; the set's keys are the walk's own, so a left-out path is
	// matched where the walk meets it.
	skipped, err := skipSet(joinSub(dir, sub), sub, skip)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("add %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("add %s: not a directory", dir)
	}
	if err := w.addDir(prefix, info); err != nil {
		return nil, err
	}
	var left []Skipped
	t := &treeWalk{w: w, prefix: prefix, dir: dir, sub: sub, skip: skipped, left: &left}
	if err := filepath.WalkDir(dir, t.entry); err != nil {
		w.fail(err)
		return nil, err
	}
	return left, nil
}

// treeWalk is one AddTreeExcept's walk: what it carries, what it leaves out, and
// where what it left is reported.
type treeWalk struct {
	w      *Writer
	prefix string
	dir    string
	// sub is the one directory of the tree whose contents travel, and skip the
	// directories inside it that do not.
	sub  string
	skip map[string]bool
	left *[]Skipped
}

// entry is one entry the walk met. A directory beside the subtree a snapshot
// carries is not entered, and neither is a left-out one: everything under them is
// another program's or something Plex rebuilds.
func (t *treeWalk) entry(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(t.dir, p)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	slash := filepath.ToSlash(rel)
	if t.sub != "" && !within(slash, t.sub) {
		return leave(d)
	}
	if t.skip[slash] {
		return leave(d)
	}
	name := path.Join(t.prefix, slash)
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	switch {
	case d.IsDir():
		return t.w.addDir(name, info)
	case info.Mode().IsRegular():
		return t.w.addFileFrom(name, p, info)
	case info.Mode()&fs.ModeSymlink != 0:
		skip, err := t.w.addLink(name, p, t.dir, rel)
		if err != nil {
			return err
		}
		if skip != nil {
			*t.left = append(*t.left, *skip)
		}
		return nil
	case info.Mode()&(fs.ModeSocket|fs.ModeNamedPipe) != 0:
		// A running app keeps its own IPC in its config directory (qBittorrent's
		// qBittorrent/ipc-socket), which no archive can carry and no receiving
		// host restore; the app makes it again when it starts.
		*t.left = append(*t.left, Skipped{
			Path:   name,
			Target: "",
			Why:    "it is a " + modeName(info.Mode()) + " the running app made for itself, which an archive cannot carry; the app makes it again when it starts",
		})
		return nil
	}
	return fmt.Errorf("%s is a %s: an archive carries files, directories and links inside the tree, not devices",
		p, modeName(info.Mode()))
}

func leave(d fs.DirEntry) error {
	if d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

func joinSub(dir, sub string) string {
	if sub == "" {
		return dir
	}
	return filepath.Join(dir, filepath.FromSlash(sub))
}

// within reports whether a walked path is sub itself, inside sub, or a directory
// above it: what one subtree of a config directory carries.
func within(rel, sub string) bool {
	return rel == sub || strings.HasPrefix(rel, sub+"/") || strings.HasPrefix(sub, rel+"/")
}

// skipSet is the directories a tree is walked without: each one a path inside dir,
// checked once before the walk so a spelling that names nothing is a refusal. The
// keys are paths from the walked root, which is what the walk compares.
func skipSet(dir, base string, skip []string) (map[string]bool, error) {
	if len(skip) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(skip))
	for _, name := range skip {
		if !safeName(name) || name == "." {
			return nil, fmt.Errorf("leave out %q of %s: that is not a directory inside it", name, dir)
		}
		at := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(at)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// A directory the app never made is nothing to leave out: a
				// fresh Plex has no Updates until it has updated.
				continue
			}
			return nil, fmt.Errorf("leave out %s: %w", at, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("leave out %q of %s: it is not a directory", name, dir)
		}
		out[path.Join(base, name)] = true
	}
	return out, nil
}

// Carried is what AddFiles added: which of the named files went in, which were not
// there, and which links it left out.
type Carried struct {
	// Added are the files it added, by the path they were named by, relative to
	// the tree.
	Added []string
	// Missing are the named files that are not in the tree.
	Missing []string
	// Skipped are the links of the set that were left out, with the reason.
	Skipped []Skipped
}

// AddFiles adds only the files named, and the directories above them under prefix. It
// is AddTree's other half, for a tree an archive carries a part of: Plex's
// Preferences.xml and databases are the state a migration needs, and its Metadata
// tree is tens of gigabytes of covers Plex rebuilds.
//
// sub names the directory the files are in, relative to dir in slash form. The rules
// are AddTree's, so a carried subset is as verifiable as a whole tree.
//
// A named file that is not there is returned in Missing and is not an error: a
// SQLite database's -wal and -shm exist only while a writer has it open.
func (w *Writer) AddFiles(prefix, dir, sub string, files []string) (Carried, error) {
	var res Carried
	if err := w.ready(prefix); err != nil {
		return res, err
	}
	if !safeName(prefix) {
		return res, fmt.Errorf("add %s: a tree's prefix is a path inside the archive", dir)
	}
	if sub != "" && !safeName(sub) {
		return res, fmt.Errorf("add %s: %q is not a directory inside it", dir, sub)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return res, fmt.Errorf("add %s: %w", dir, err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("add %s: not a directory", dir)
	}
	if err := w.addDir(prefix, info); err != nil {
		return res, err
	}
	// The app's own directory, and the ones that lead to it: the extract has to
	// put the files where the app looks for them.
	base, at := dir, prefix
	if sub != "" {
		parts := strings.Split(sub, "/")
		for i := 1; i <= len(parts); i++ {
			rel := strings.Join(parts[:i], "/")
			info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return res, fmt.Errorf("add %s: %w", filepath.Join(dir, filepath.FromSlash(rel)), err)
			}
			if err := w.addDir(path.Join(prefix, rel), info); err != nil {
				return res, err
			}
		}
		base, at = filepath.Join(dir, filepath.FromSlash(sub)), path.Join(prefix, sub)
	}
	// made is every directory of the tree already in the archive: a set of files
	// shares them, and an entry asked for twice is a refusal rather than two.
	made := map[string]bool{}
	for _, rel := range files {
		if !safeName(rel) {
			return res, fmt.Errorf("%q is not a file inside %s: a carried file is a relative path with no .. in it", rel, dir)
		}
		skip, added, err := w.addCarried(at, base, rel, made)
		switch {
		case err != nil:
			return res, err
		case skip != nil:
			res.Skipped = append(res.Skipped, *skip)
		case added:
			res.Added = append(res.Added, rel)
		default:
			res.Missing = append(res.Missing, rel)
		}
	}
	return res, nil
}

// addCarried adds one named file and the directories above it. A nil skip with
// added false is the tree not having it.
func (w *Writer) addCarried(prefix, dir, rel string, made map[string]bool) (skip *Skipped, added bool, err error) {
	if err := w.addCarryDirs(prefix, dir, rel, made); err != nil {
		if errors.Is(err, errAbsent) {
			return nil, false, nil
		}
		return nil, false, err
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("add %s: %w", full, err)
	}
	name := path.Join(prefix, rel)
	switch {
	case info.Mode().IsRegular():
		return nil, true, w.addFileFrom(name, full, info)
	case info.Mode()&fs.ModeSymlink != 0:
		skip, err := w.addLink(name, full, dir, rel)
		if err != nil {
			return nil, false, err
		}
		return skip, skip == nil, nil
	}
	if info.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory: a carried file is a file or a link inside the tree", full)
	}
	return nil, false, fmt.Errorf("%s is a %s: a carried file is a file or a link inside the tree", full, modeName(info.Mode()))
}

// errAbsent is a directory above a carried file not being there, which makes the
// file absent too: the caller reports it as missing, not as a failure.
var errAbsent = errors.New("not in the tree")

// addCarryDirs adds the directories above one carried file, once each, so the
// subset's shape is in the archive before the file that names it.
func (w *Writer) addCarryDirs(prefix, dir, rel string, made map[string]bool) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if made[parent] {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(parent))
		info, err := os.Lstat(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return errAbsent
			}
			return fmt.Errorf("add %s: %w", full, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("add %s: %s is not a directory, so nothing can be carried under it", full, parent)
		}
		if err := w.addDir(path.Join(prefix, parent), info); err != nil {
			return err
		}
		made[parent] = true
	}
	return nil
}

// Close writes the manifest as the archive's last entry and makes the file
// appear: it is the only thing that commits, so a run that stops before it leaves
// no archive to mistake for one.
func (w *Writer) Close() error {
	switch {
	case w.err != nil:
		return w.err
	case w.done:
		return errors.New("the archive is closed")
	}
	raw, err := json.MarshalIndent(w.m, "", "  ")
	if err != nil {
		w.fail(err)
		return fmt.Errorf("write %s: %w", ManifestName, err)
	}
	raw = append(raw, '\n')
	if int64(len(raw)) > maxManifest {
		err := fmt.Errorf("the archive holds %d entries, and its %s would be %d bytes, past the %d a box reads: no archive is written that box cannot open",
			len(w.m.Entries), ManifestName, len(raw), maxManifest)
		w.fail(err)
		return err
	}
	hdr := &tar.Header{
		Name:     ManifestName,
		Mode:     0o600,
		Size:     int64(len(raw)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now().UTC(),
		Format:   tar.FormatPAX,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		w.fail(err)
		return fmt.Errorf("write %s: %w", ManifestName, err)
	}
	if _, err := w.tw.Write(raw); err != nil {
		w.fail(err)
		return fmt.Errorf("write %s: %w", ManifestName, err)
	}
	// The tar's own end marker, then gzip's trailer, then the rename: each layer
	// has to finish before the one under it.
	if err := w.tw.Close(); err != nil {
		w.fail(err)
		return fmt.Errorf("write %s: %w", w.path, err)
	}
	if err := w.gz.Close(); err != nil {
		w.fail(err)
		return fmt.Errorf("write %s: %w", w.path, err)
	}
	if err := w.file.Commit(); err != nil {
		w.fail(err)
		return err
	}
	w.done = true
	return nil
}

// Discard abandons the archive and leaves nothing at its path.
func (w *Writer) Discard() {
	w.fail(errors.New("discarded"))
}

// ready refuses an add after Close, after a failure, or a name that is not a path
// inside the archive.
func (w *Writer) ready(name string) error {
	switch {
	case w.err != nil:
		return w.err
	case w.done:
		return errors.New("the archive is closed")
	case name == ManifestName:
		return fmt.Errorf("%s is the archive's manifest, written by Close", ManifestName)
	case !safeName(name):
		return fmt.Errorf("%q is not a path inside the archive", name)
	}
	return nil
}

// fail records the first error and throws the archive away: a layer that failed
// leaves no name behind for the next run to mistake for a whole archive.
func (w *Writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
	if !w.done {
		w.done = true
		// The two layers close best-effort; the error worth reporting is the
		// one that stopped the write.
		_ = w.tw.Close()
		_ = w.gz.Close()
		w.file.Discard()
	}
}

// claim records one entry and refuses a name that collides with what is already in
// the archive: the same path twice, a file where a directory has to be, or a
// directory where a file already is.
func (w *Writer) claim(name string, dir bool) error {
	if w.seen[name] {
		return fmt.Errorf("%s is in the archive twice", name)
	}
	if w.dirs[name] {
		return fmt.Errorf("%s is a directory already", name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if w.seen[parent] && !w.dirs[parent] {
			return fmt.Errorf("%s is a file, so %s cannot sit under it", parent, name)
		}
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		w.dirs[parent] = true
	}
	w.seen[name] = true
	if dir {
		w.dirs[name] = true
	}
	return nil
}

func (w *Writer) addDir(name string, info fs.FileInfo) error {
	if err := w.claim(name, true); err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:     name + "/",
		Mode:     int64(info.Mode().Perm()),
		Typeflag: tar.TypeDir,
		ModTime:  info.ModTime(),
		Format:   tar.FormatPAX,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	w.m.Entries[name] = Entry{Size: 0, SHA256: digest(nil), Mode: modeText(uint32(info.Mode().Perm()))}
	return nil
}

// smallFile is the size up to which a file is read whole before it is entered.
const smallFile = 8 << 20

// addFileFrom records one regular file.
//
// A small one is read whole and entered as what was read: a rehearsal reads a
// running source, and a running app rewrites its cookie jar whenever it likes, while
// the entry has to agree with the manifest.
//
// A large one is streamed (a database of gigabytes) and one that changed under the
// copy is a refusal rather than a torn entry.
func (w *Writer) addFileFrom(name, file string, info fs.FileInfo) error {
	if info.Size() <= smallFile {
		raw, err := readSmall(file)
		if err != nil {
			return err
		}
		if raw != nil {
			return w.write(name, bytes.NewReader(raw), int64(len(raw)), info.Mode().Perm(), info.ModTime())
		}
		// It grew past the small size since its stat: streamed like any
		// large file, under the same refusal.
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return w.write(name, f, info.Size(), info.Mode().Perm(), info.ModTime())
}

// addLink records one symlink, or says it is left out.
func (w *Writer) addLink(name, file, root, rel string) (*Skipped, error) {
	target, err := os.Readlink(file)
	if err != nil {
		return nil, err
	}
	res, err := linktree.Resolve(root, rel, target)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if res.Skip {
		return &Skipped{
			Path:   name,
			Target: target,
			Why:    "it names the kernel's own filesystems (the pod's stdout), which no config tree holds; the app writes the link again when it starts",
		}, nil
	}
	if err := w.claim(name, false); err != nil {
		return nil, err
	}
	link := filepath.ToSlash(res.Target)
	hdr := &tar.Header{
		Name:     name,
		Linkname: link,
		Typeflag: tar.TypeSymlink,
		Mode:     0o777,
		ModTime:  time.Now().UTC(),
		Format:   tar.FormatPAX,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return nil, fmt.Errorf("add %s: %w", name, err)
	}
	w.m.Entries[name] = Entry{Size: int64(len(link)), SHA256: digest([]byte(link)), Mode: modeText(0o777)}
	return nil, nil
}

// write streams one regular entry, hashing as it goes so the manifest's sum is the
// bytes that went into the archive.
func (w *Writer) write(name string, r io.Reader, size int64, mode fs.FileMode, mtime time.Time) error {
	if err := w.claim(name, false); err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  mtime,
		Format:   tar.FormatPAX,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w.tw, h), r)
	if err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	if n != size {
		return fmt.Errorf("%s changed while it was copied: it was %d bytes and is now %d", name, size, n)
	}
	w.m.Entries[name] = Entry{Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: modeText(uint32(mode.Perm()))}
	return nil
}

// readSmall reads a file that was small when it was stat'ed, and returns nil when
// it is no longer small: the caller streams that one instead.
func readSmall(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, smallFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > smallFile {
		return nil, nil
	}
	return raw, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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
