package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// boxFile is one of the small JSON records box keeps under .box: a file whose
// absence means none, and whose contents box acts on.
type boxFile struct {
	// name is the file's own name, which is also what an error calls it.
	name string
	// noun is what the file holds, for a refusal a person reads.
	noun string
}

var (
	holdsFileRecord = boxFile{name: HoldsName, noun: "holds"}
	pinsFileRecord  = boxFile{name: PinsName, noun: "pins"}
)

// loadBoxFile reads one record store. A missing file, or a config root that is not
// there, is none; anything else is an error naming the path, because guessing here
// drops a record box acts on.
func loadBoxFile[T any](configRoot string, file boxFile) (T, bool, error) {
	var none T
	path := filepath.Join(configRoot, boxDir, file.name)
	t, err := openTree(configRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return none, false, nil
		}
		return none, false, fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	defer t.Close()
	name, err := t.rel(path)
	if err != nil {
		return none, false, fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	data, err := t.readFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return none, false, nil
	}
	if err != nil {
		return none, false, fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	var doc T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return none, false, fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	// A second document after the first is trailing junk: only io.EOF means the
	// file ended where the object did.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return none, false, fmt.Errorf("%s %s: has more than the one document", file.noun, path)
	}
	return doc, true, nil
}

// saveBoxFile writes one record store, through internal/atomicfile so a crash
// leaves the file as it was or complete. An empty list removes the file: its
// absence is what loadBoxFile reads as none.
func saveBoxFile[T any](configRoot string, file boxFile, doc T, empty bool) error {
	path := filepath.Join(configRoot, boxDir, file.name)
	t, err := openTree(configRoot)
	if err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	defer t.Close()
	name, err := t.rel(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	if empty {
		if err := t.remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s %s: %w", file.noun, path, err)
		}
		return nil
	}
	// A link where box's file goes is a refusal: box writes this file itself, and
	// the user can only have put a link there to point box somewhere.
	info, err := t.lstat(name)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s %s: it is a symlink, and box writes the %s file itself; remove it and box will write a new one", file.noun, path, file.noun)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	own, err := treeOwnerOf(t)
	if err != nil {
		return err
	}
	if err := makeDirIn(t, filepath.Dir(name), own); err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	if err := atomicfile.WriteIn(t.root, name, append(data, '\n'), treeFileMode); err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	if err := own.ownIn(t, name); err != nil {
		return fmt.Errorf("%s %s: %w", file.noun, path, err)
	}
	return nil
}
