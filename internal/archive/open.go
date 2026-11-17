package archive

// open.go is the read half: the manifest is found first, so every entry is
// checked against a list before a byte of it is trusted, and the extracted tree
// appears whole or not at all.

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
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/linktree"
)

// maxManifest is the largest manifest this box reads. An archive's plan and
// report live outside it, so a manifest is one line per file it holds; a
// megabyte is far past any config tree, and a limit is what keeps a crafted
// archive from asking for memory it could not otherwise get.
const maxManifest = 1 << 20

// Read is what an archive says about itself, without extracting it: the kind, the
// mode, when and by which box it was written, and the entry per path. A caller
// that has to choose an archive -- the newest final snapshot in the default
// location -- reads this and nothing else.
func Read(path string) (*Manifest, error) {
	return readManifest(path)
}

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
	if _, err := os.Lstat(dir); err == nil {
		return nil, fmt.Errorf("extract to %s: it already exists, and a snapshot extracts to a new directory", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("extract to %s: %w", dir, err)
	}
	if err := extract(archive, dir, m); err != nil {
		// A half-extracted tree is worse than none: it looks like a snapshot.
		os.RemoveAll(dir)
		return nil, err
	}
	return m, nil
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

// extract is the second pass: every tar entry is checked against the manifest
// and written, and every manifest entry has to appear.
func extract(archive, dir string, m *Manifest) error {
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
		if err := extractEntry(archive, dir, m, tr, hdr, seen); err != nil {
			return err
		}
	}
	// The same drain as the first pass: the gzip trailer is what proves the
	// file reached its end.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("read %s: %w", archive, err)
	}
	for _, name := range m.names() {
		if !seen[name] {
			return fmt.Errorf("%s does not hold %s, which its manifest names: the copy is incomplete", archive, name)
		}
	}
	return nil
}

// extractEntry checks one tar entry against the manifest and writes it.
func extractEntry(archive, dir string, m *Manifest, tr *tar.Reader, hdr *tar.Header, seen map[string]bool) error {
	name := path.Clean(hdr.Name)
	// The manifest was verified in the first pass, and readManifest refuses an
	// archive with anything after it; the entry itself is not extracted.
	if name == ManifestName {
		return nil
	}
	if !safeName(name) {
		return fmt.Errorf("%s holds %q, which is not a path inside an archive", archive, hdr.Name)
	}
	e := m.lookup(name)
	if e == nil {
		return fmt.Errorf("%s holds %s, which its manifest does not name", archive, name)
	}
	if seen[name] {
		return fmt.Errorf("%s holds %s twice", archive, name)
	}
	seen[name] = true
	if err := makeParents(dir, name); err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(name))
	want, err := strconv.ParseUint(e.Mode, 8, 32)
	if err != nil {
		return fmt.Errorf("%s: %s: mode %q is not octal", archive, name, e.Mode)
	}
	if uint32(hdr.Mode)&0o7777 != uint32(want) {
		return fmt.Errorf("%s: %s is %04o and its manifest says %04o", archive, name, hdr.Mode, want)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := extractDir(target, name, *e); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
	case tar.TypeReg:
		if err := extractFile(target, name, tr, *e); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
	case tar.TypeSymlink:
		if err := extractLink(dir, target, name, hdr.Linkname, *e); err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
	case tar.TypeLink:
		return fmt.Errorf("%s holds a hardlink, %s -> %s: box writes none, so this is not an archive box wrote", archive, name, hdr.Linkname)
	default:
		return fmt.Errorf("%s holds %s, which is a %s: an archive carries files, directories and links", archive, name, typeName(hdr.Typeflag))
	}
	return nil
}

// extractDir makes one directory entry, 0700, and refuses a path that is
// already something else.
func extractDir(target, name string, e Entry) error {
	if e.Size != 0 || e.SHA256 != digest(nil) {
		return fmt.Errorf("%s does not match its manifest", name)
	}
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

// extractFile writes one regular entry, hashing as it writes, and refuses a byte
// count or a digest that is not the manifest's.
func extractFile(target, name string, r io.Reader, e Entry) error {
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	if n != e.Size {
		return fmt.Errorf("%s is %d bytes and its manifest says %d", name, n, e.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("%s does not match its manifest: sha256 %s, want %s", name, got, e.SHA256)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return fmt.Errorf("extract %s: %w", name, err)
	}
	return nil
}

// extractLink writes one symlink under internal/linktree's rule, so a link that
// leaves the tree the archive names is refused rather than materialised.
func extractLink(dir, target, name, linkname string, e Entry) error {
	if int64(len(linkname)) != e.Size || digest([]byte(linkname)) != e.SHA256 {
		return fmt.Errorf("%s does not match its manifest", name)
	}
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
