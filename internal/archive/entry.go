package archive

// entry.go reads one named entry out of an archive without extracting the rest of
// it, which is how `box migrate cutover` reads a snapshot's plan.yaml without
// paying for its config trees.
//
// The manifest's digest is not checked here, deliberately: this is a question, not
// an apply, and the reader of the bytes validates its own format.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
)

// ErrNoEntry marks a name the archive does not hold: fs.ErrNotExist-adjacent in
// meaning, and distinct from a copy that cannot be read at all.
var ErrNoEntry = errors.New("no such entry")

// ReadEntry returns the bytes of one regular entry of an archive, named as
// Manifest.Entries keys it ("plan.yaml"); a directory, a link or a device is not
// an answer.
//
// maxEntry caps what one entry may cost in memory -- a crafted archive must not ask
// for more -- and is the largest single entry ReadEntry returns, not the manifest's
// cap, which grows with the number of files an archive holds.
const maxEntry = 1 << 20

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
		if hdr.Size < 0 || hdr.Size > maxEntry {
			return nil, fmt.Errorf("%s: %s is %d bytes, past the %d this box reads", archive, clean, hdr.Size, maxEntry)
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxEntry))
		if err != nil {
			return nil, fmt.Errorf("read %s from %s: %w", clean, archive, err)
		}
		return raw, nil
	}
}
