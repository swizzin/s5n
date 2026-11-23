package archive

// open.go is the read half: the manifest is found first, so every entry is
// checked against a list before a byte of it is trusted, and the extracted tree
// appears whole or not at all.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/linktree"
)

// maxManifest is the largest manifest this box reads. A manifest is one entry
// per file the archive holds, about 250 bytes each, and a real config tree is
// thousands of files (a torrent client keeps two per torrent), so a migration
// snapshot's manifest runs to megabytes. 64 MiB is a quarter of a million
// files; the limit is what keeps a crafted archive from asking for memory it
// could not otherwise get, and the writer refuses to write past it (Close), so
// no archive exists that a box cannot read back. A variable only so a test can
// reach the refusal without writing that many files.
var maxManifest int64 = 64 << 20

// Read is what an archive says about itself, without extracting it: the kind, the
// mode, when and by which box it was written, and the entry per path. A caller
// that has to choose an archive -- the newest final snapshot in the default
// location -- reads this and nothing else.
func Read(path string) (*Manifest, error) {
	return readManifest(path)
}

// Filter selects the entries of an archive: a name it answers false for is left
// in the file for a later pass.
type Filter func(name string) bool

// Owner is who an extraction's entries belong to. Nil is the user who ran box: a
// snapshot's own extract is box's, and a staged copy belongs to the seedbox's
// user, which the run that stages it has just read out of the values.
type Owner struct{ UID, GID int }

// Open verifies an archive and extracts it to dir, which must not exist yet.
//
// It refuses a missing or truncated archive, an entry the manifest does not name
// and a manifest entry the archive does not hold, a size or sha256 that does not
// match, a path that is absolute or steps out with `..`, and a link, device, fifo
// or hardlink that is not something box writes. A failure leaves nothing at dir.
//
// The extracted tree is 0700 directories and 0600 files and belongs to whoever
// ran box: ownership is not carried, because only apply knows the uid that will
// own the staged copy.
func Open(archive, dir string) (*Manifest, error) {
	m, err := readManifest(archive)
	if err != nil {
		return nil, err
	}
	if err := Extract(archive, dir, m, nil, nil); err != nil {
		return nil, err
	}
	return m, nil
}

// Extract writes the entries keep accepts into dir, which must not exist yet, and
// refuses a half-extracted tree: a failure leaves nothing at dir.
//
// keep is what part of an archive this pass writes -- nil is all of it -- and
// every entry it leaves for a later pass has to have been checked already:
// internal/archive's Verify is the pass that reads the whole archive without
// writing any of it. What this pass does not write is still checked as it is
// skipped, so an entry the manifest does not name is refused here too.
//
// own is who the written tree belongs to; nil is the user who ran box.
func Extract(archive, dir string, m *Manifest, keep Filter, own *Owner) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("extract to %s: it already exists, and a snapshot extracts to a new directory", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := extract(archive, dir, m, keep, own); err != nil {
		// A half-extracted tree is worse than none: it looks like a snapshot.
		os.RemoveAll(dir)
		return err
	}
	return nil
}

// readManifest is the first pass: walk the archive to its last entry, which the
// manifest must be, and read it. Nothing is extracted until this returns.
func readManifest(archive string) (*Manifest, error) {
	f, gz, err := openArchive(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(gz)
	var raw []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", archive, err)
		}
		name := path.Clean(hdr.Name)
		if !safeName(name) {
			return nil, fmt.Errorf("%s holds %q, which is not a path inside an archive", archive, hdr.Name)
		}
		if name != ManifestName {
			if raw != nil {
				return nil, fmt.Errorf("%s holds %s after its manifest: box writes the manifest last, so this is not an archive box wrote", archive, name)
			}
			continue
		}
		if raw != nil {
			return nil, fmt.Errorf("%s holds more than one %s", archive, ManifestName)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s: %s is not a file", archive, ManifestName)
		}
		if hdr.Size < 0 || hdr.Size > maxManifest {
			return nil, fmt.Errorf("%s: %s is %d bytes, past the %d this box reads", archive, ManifestName, hdr.Size, maxManifest)
		}
		if raw, err = io.ReadAll(tr); err != nil {
			return nil, fmt.Errorf("read %s: %w", archive, err)
		}
	}
	// tar answers io.EOF at its end-of-archive marker without reading the rest
	// of the file, so the gzip stream is drained here: its trailer is the only
	// thing that says the copy reached the end rather than stopping a byte
	// short.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("read %s: %w", archive, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%s has no %s: it is not a snapshot, or the copy stopped before the end", archive, ManifestName)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A key this box does not know is a newer manifest than it can honour, and
	// ignoring it would read one field's meaning out of a shape built for
	// another.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %s does not read: %w", archive, ManifestName, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%s: %s holds more than one JSON value", archive, ManifestName)
	}
	if problems := m.problems(); len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s: %s", archive, ManifestName, joinProblems(problems))
	}
	return &m, nil
}

// extract is the second pass: every tar entry keep accepts is checked against
// the manifest and written, and every manifest entry keep accepts has to appear.
func extract(archive, dir string, m *Manifest, keep Filter, own *Owner) error {
	f, gz, err := openArchive(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(gz)
	seen := make(map[string]bool, len(m.Entries))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}
		if err := extractEntry(archive, dir, m, tr, hdr, keep, own, seen); err != nil {
			return err
		}
	}
	// The same drain as the first pass: the gzip trailer is what proves the
	// file reached its end.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("read %s: %w", archive, err)
	}
	for _, name := range m.names() {
		if keep != nil && !keep(name) {
			// A later pass writes it, and Verify checked it: this pass owes the
			// entries it was told to write, and no others.
			continue
		}
		if !seen[name] {
			return fmt.Errorf("%s does not hold %s, which its manifest names: the copy is incomplete", archive, name)
		}
	}
	return nil
}

// extractEntry checks one tar entry against the manifest and writes it, unless
// keep leaves it for a later pass.
func extractEntry(archive, dir string, m *Manifest, tr *tar.Reader, hdr *tar.Header, keep Filter, own *Owner, seen map[string]bool) error {
	name := path.Clean(hdr.Name)
	// The manifest was verified in the first pass, and readManifest refuses an
	// archive with anything after it; the entry itself is not extracted.
	if name == ManifestName {
		return nil
	}
	if !safeName(name) {
		return fmt.Errorf("%s holds %q, which is not a path inside an archive", archive, hdr.Name)
	}
	if m.lookup(name) == nil {
		return fmt.Errorf("%s holds %s, which its manifest does not name", archive, name)
	}
	if seen[name] {
		return fmt.Errorf("%s holds %s twice", archive, name)
	}
	if keep != nil && !keep(name) {
		return nil
	}
	seen[name] = true
	if err := makeParents(dir, name); err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(name))
	switch hdr.Typeflag {
	case tar.TypeDir:
		if _, err := checkEntry(archive, nil, m, tr, hdr); err != nil {
			return err
		}
		if err := extractDir(target, name); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		return hand(own, target, false)
	case tar.TypeReg:
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		_, cerr := checkEntry(archive, out, m, tr, hdr)
		if err := out.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
		if err := os.Chmod(target, 0o600); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		return hand(own, target, false)
	case tar.TypeSymlink:
		if _, err := checkEntry(archive, nil, m, tr, hdr); err != nil {
			return err
		}
		if err := extractLink(dir, target, name, hdr.Linkname); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		return hand(own, target, true)
	}
	// checkEntry refuses the rest by name: a hardlink, a device, a fifo.
	_, err := checkEntry(archive, nil, m, tr, hdr)
	return err
}

// hand gives one extracted path to the owner the extraction was told, and does
// nothing when it was told none: a snapshot's own extract belongs to whoever ran
// box, and a staged copy to the seedbox's user. link is a symlink, owned with
// Lchown so the extraction never follows it.
func hand(own *Owner, path string, link bool) error {
	if own == nil {
		return nil
	}
	var err error
	if link {
		err = os.Lchown(path, own.UID, own.GID)
	} else {
		err = os.Chown(path, own.UID, own.GID)
	}
	if err != nil {
		return fmt.Errorf("own %s: %w", path, err)
	}
	return nil
}

// extractDir makes one directory entry, 0700, and refuses a path that is
// already something else.
func extractDir(target, name string) error {
	if info, err := os.Lstat(target); err == nil && !info.IsDir() {
		return fmt.Errorf("extract %s: it is already there and is not a directory", name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	return nil
}

// extractLink writes one symlink under internal/linktree's rule, so a link that
// leaves the tree the archive names is refused rather than materialised.
func extractLink(dir, target, name, linkname string) error {
	res, err := linktree.Resolve(dir, filepath.FromSlash(name), linkname)
	if err != nil {
		return err
	}
	if res.Skip {
		return fmt.Errorf("%s is a link to %s, which names the kernel's own filesystems: box leaves such a link out, so this is not an archive box wrote", name, linkname)
	}
	if err := os.Symlink(res.Target, target); err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	return nil
}

// makeParents creates the directories a path needs, 0700, and refuses a parent
// that is already something else: an archive cannot be allowed to write through
// a link it brought with it.
func makeParents(dir, name string) error {
	parts := []string{}
	if parent := path.Dir(name); parent != "." && parent != "/" {
		parts = splitPath(parent)
	}
	for i := range parts {
		at := filepath.Join(dir, filepath.FromSlash(path.Join(parts[:i+1]...)))
		info, err := os.Lstat(at)
		switch {
		case err == nil:
			if !info.IsDir() {
				return fmt.Errorf("extract %s: %s is not a directory", name, path.Join(parts[:i+1]...))
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(at, 0o700); err != nil {
				return fmt.Errorf("extract %s: %w", name, err)
			}
		default:
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}
	return nil
}

// splitPath splits a cleaned relative path into its parts.
func splitPath(p string) []string { return strings.Split(p, "/") }

// openArchive opens the file and its gzip stream.
func openArchive(archive string) (*os.File, *gzip.Reader, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", archive, err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("read %s: %w", archive, err)
	}
	return f, gz, nil
}

// typeName names a tar entry type the way a refusal reads it.
func typeName(typeflag byte) string {
	switch typeflag {
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "named pipe"
	}
	return "special file"
}
