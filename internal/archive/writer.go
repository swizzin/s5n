package archive

// writer.go is the write half: the caller adds the files it has in memory and
// the trees it wants carried, and Close writes the manifest last and makes the
// file appear. Nothing is left at the path until then.

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

// Writer writes one archive. A caller that abandons it leaves nothing at the
// path it named: the bytes go to a temporary file beside it, as in
// internal/atomicfile, and only Close renames it into place.
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
// defaults to Format and its Created to now; its Entries are the writer's, added
// as the caller adds content.
//
// The file is 0600 from the first byte: it holds the source's API keys, the Plex
// token and qBittorrent's password hash.
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

// joinProblems is a manifest's problems as one sentence.
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
// uses: a link that stays inside the tree is carried, one that names the kernel's
// own filesystems (an app's log at the pod's stdout) is left out and returned in
// skipped, and one that leaves the tree stops the archive, because it would name
// a path no receiving host has.
//
// Nothing of the tree's ownership is recorded: the extracted tree is box's, and
// apply re-owns what it stages.
func (w *Writer) AddTree(prefix, dir string) ([]Skipped, error) {
	if err := w.ready(prefix); err != nil {
		return nil, err
	}
	if !safeName(prefix) {
		return nil, fmt.Errorf("add %s: a tree's prefix is a path inside the archive", dir)
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
	var skipped []Skipped
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := path.Join(prefix, filepath.ToSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return w.addDir(name, info)
		case info.Mode().IsRegular():
			return w.addFileFrom(name, p, info)
		case info.Mode()&fs.ModeSymlink != 0:
			skip, err := w.addLink(name, p, dir, rel)
			if err != nil {
				return err
			}
			if skip != nil {
				skipped = append(skipped, *skip)
			}
			return nil
		case info.Mode()&(fs.ModeSocket|fs.ModeNamedPipe) != 0:
			// A running app keeps its own IPC in its config directory --
			// qBittorrent's qBittorrent/ipc-socket is one -- and a socket is
			// not something an archive can carry or a receiving host restore.
			// The app makes it again when it starts, so it is left out with
			// the reason, as a link at the pod's stdout is.
			skipped = append(skipped, Skipped{
				Path:   name,
				Target: "",
				Why:    "it is a " + modeName(info.Mode()) + " the running app made for itself, which an archive cannot carry; the app makes it again when it starts",
			})
			return nil
		}
		return fmt.Errorf("%s is a %s: an archive carries files, directories and links inside the tree, not devices",
			p, modeName(info.Mode()))
	})
	if err != nil {
		w.fail(err)
		return nil, err
	}
	return skipped, nil
}

// Close writes the manifest as the archive's last entry and makes the file
// appear. It is the only thing that commits, so a run that stops before it leaves
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
	// The tar's own end marker, then gzip's trailer, then the rename: each
	// layer has to finish before the one under it.
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

// ready refuses an add after Close, after a failure, or a name that is not a
// path inside the archive.
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

// claim records one entry and refuses a name that collides with what is already
// in the archive: the same path twice, a file where a directory has to be, or a
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

// addDir records one directory.
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

// addFileFrom records one regular file, streaming it: an app's config tree can
// hold a database of gigabytes, and a file that changed under the copy is a
// refusal rather than a torn entry.
func (w *Writer) addFileFrom(name, file string, info fs.FileInfo) error {
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

// write streams one regular entry, hashing as it goes so the manifest's sum is
// the bytes that went into the archive.
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

// digest is the manifest's sum of a handful of bytes.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
