package archive

// entry.go reads one named entry out of an archive without extracting the rest
// of it.
//
// It is what asks a snapshot a question about itself before deciding to open the
// whole thing: `box migrate cutover` reads the plan.yaml of the snapshots already
// in the directory to see which source each one was a read of, and a final
// snapshot's config trees are far too big to extract for that answer.
//
// The manifest's digest is not checked here, and that is deliberate: this is a
// question, not an apply. The bytes go to a reader that validates its own format
// (internal/migrate/plan), and anything a run acts on is extracted with Open,
// which checks every entry against the manifest.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
)

// ErrNoEntry marks a name the archive does not hold. It is fs.ErrNotExist-adjacent
// in meaning -- the entry is not there -- and distinct from a copy that cannot be
// read at all.
var ErrNoEntry = errors.New("no such entry")

// ReadEntry returns the bytes of one regular entry of an archive. The name is
// the archive path, as Manifest.Entries keys them ("plan.yaml"), and only a
// regular file is an answer: a directory, a link or a device is not something
// this box reads a value out of.
//
// The entry is refused past maxManifest bytes, the same cap the manifest has: a
// value a caller reads whole has to fit in memory, and a crafted archive must not
// be able to ask for more.
func ReadEntry(archive, name string) ([]byte, error) {
	clean := path.Clean(name)
	if !safeName(clean) {
		return nil, fmt.Errorf("%q is not a path inside an archive", name)
	}
	f, gz, err := openArchive(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s: %w: %s", archive, ErrNoEntry, clean)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", archive, err)
		}
		if path.Clean(hdr.Name) != clean {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s: %s is not a file", archive, clean)
		}
		if hdr.Size < 0 || hdr.Size > maxManifest {
			return nil, fmt.Errorf("%s: %s is %d bytes, past the %d this box reads", archive, clean, hdr.Size, maxManifest)
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxManifest))
		if err != nil {
			return nil, fmt.Errorf("read %s from %s: %w", clean, archive, err)
		}
		return raw, nil
	}
}
