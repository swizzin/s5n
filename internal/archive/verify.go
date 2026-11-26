package archive

// verify.go is the pass that reads an archive whole and writes nothing of it.
//
// Extraction verifies each entry as it writes it, which is enough while everything
// an archive holds is extracted before it is used. A migration snapshot is not: the
// bulk of an app's config (a Plex library of artwork, tens of gigabytes) is written
// straight out of the archive after the box is installed. So a copy that stopped
// halfway or lost a byte is refused before the first write of any kind.

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strconv"
)

// EntryKind is what one entry of an archive is. A verified archive holds nothing
// else: a device, a fifo or a hardlink is a refusal.
type EntryKind int

const (
	// EntryFile is a regular file.
	EntryFile EntryKind = iota
	// EntryDir is a directory.
	EntryDir
	// EntryLink is a symbolic link.
	EntryLink
)

// Seen is one entry a verify pass checked: what it is, and how many bytes of it
// the archive holds.
type Seen struct {
	// Name is the entry's path inside the archive, cleaned.
	Name string
	// Kind is what it is.
	Kind EntryKind
	// Size is the bytes it holds: a file's content, or a link's target.
	Size int64
}

// Verify streams an archive against its manifest and writes nothing: every entry is
// checked against the manifest (name, mode, type, and the bytes a digest covers), and
// every entry the manifest names has to appear.
//
// It is what a caller extracting part of an archive runs first, so a damaged snapshot
// is refused before anything is written anywhere. seen, when not nil, is told every
// entry as it is checked, so a caller counts what it will write without walking a
// tree that is not on the host.
func Verify(archive string, seen func(Seen)) (*Manifest, error) {
	m, err := readManifest(archive)
	if err != nil {
		return nil, err
	}
	f, gz, err := openArchive(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(gz)
	names := make(map[string]bool, len(m.Entries))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", archive, err)
		}
		name := path.Clean(hdr.Name)
		if name == ManifestName {
			continue
		}
		if !safeName(name) {
			return nil, fmt.Errorf("%s holds %q, which is not a path inside an archive", archive, hdr.Name)
		}
		if names[name] {
			return nil, fmt.Errorf("%s holds %s twice", archive, name)
		}
		names[name] = true
		entry, err := checkEntry(archive, nil, m, tr, hdr)
		if err != nil {
			return nil, err
		}
		if seen != nil {
			seen(entry)
		}
	}
	// The same drain as the two passes of an extraction: the gzip trailer is the
	// only thing that says the copy reached the end.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("read %s: %w", archive, err)
	}
	for _, name := range m.names() {
		if !names[name] {
			return nil, fmt.Errorf("%s does not hold %s, which its manifest names: the copy is incomplete", archive, name)
		}
	}
	return m, nil
}

// checkEntry checks one tar entry against the manifest: the entry is one the manifest
// names, its mode is the manifest's, and its bytes are the promised size and digest.
// w, when not nil, is written those bytes, so one pass of checks serves extraction and
// verification both.
func checkEntry(archive string, w io.Writer, m *Manifest, tr *tar.Reader, hdr *tar.Header) (Seen, error) {
	name := path.Clean(hdr.Name)
	e := m.lookup(name)
	if e == nil {
		return Seen{}, fmt.Errorf("%s holds %s, which its manifest does not name", archive, name)
	}
	want, err := strconv.ParseUint(e.Mode, 8, 32)
	if err != nil {
		return Seen{}, fmt.Errorf("%s: %s: mode %q is not octal", archive, name, e.Mode)
	}
	if uint32(hdr.Mode)&0o7777 != uint32(want) {
		return Seen{}, fmt.Errorf("%s: %s is %04o and its manifest says %04o", archive, name, hdr.Mode, want)
	}
	seen := Seen{Name: name, Kind: EntryFile, Size: e.Size}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if e.Size != 0 || e.SHA256 != digest(nil) {
			return Seen{}, fmt.Errorf("%s does not match its manifest", name)
		}
		seen.Kind = EntryDir
		return seen, nil
	case tar.TypeReg:
		h := sha256.New()
		dst := io.Writer(h)
		if w != nil {
			dst = io.MultiWriter(w, h)
		}
		n, err := io.Copy(dst, tr)
		if err != nil {
			return Seen{}, fmt.Errorf("read %s: %w", name, err)
		}
		if n != e.Size {
			return Seen{}, fmt.Errorf("%s is %d bytes and its manifest says %d", name, n, e.Size)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return Seen{}, fmt.Errorf("%s does not match its manifest: sha256 %s, want %s", name, got, e.SHA256)
		}
		return seen, nil
	case tar.TypeSymlink:
		if int64(len(hdr.Linkname)) != e.Size || digest([]byte(hdr.Linkname)) != e.SHA256 {
			return Seen{}, fmt.Errorf("%s does not match its manifest", name)
		}
		seen.Kind = EntryLink
		return seen, nil
	case tar.TypeLink:
		return Seen{}, fmt.Errorf("%s holds a hardlink, %s -> %s: box writes none, so this is not an archive box wrote", archive, name, hdr.Linkname)
	}
	return Seen{}, fmt.Errorf("%s holds %s, which is a %s: an archive carries files, directories and links", archive, name, typeName(hdr.Typeflag))
}
