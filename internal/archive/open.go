package archive

// open.go is the read half: the manifest is found first, so every entry is checked
// against a list before a byte of it is trusted, and the extracted tree appears
// whole or not at all.

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
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/linktree"
	"github.com/swizzin/s5n/internal/safefs"
)

// maxManifest is the largest manifest this box reads. A real config tree is
// thousands of files (a torrent client keeps two per torrent), so a snapshot's
// manifest runs to megabytes: 64 MiB is a quarter of a million files, and Close
// refuses to write past it, so no archive exists that a box cannot read back.
var maxManifest int64 = 64 << 20

// beforeEntry is a test's seam: it runs as one entry is about to be written, which
// is one of the moments the directory holding the extraction can change under it.
var beforeEntry = func(name string) error { return nil }

// Read is what an archive says about itself, without extracting it: the kind, the
// mode, when and by which box it was written, and the entry per path. A caller
// choosing an archive reads this and nothing else.
func Read(path string) (*Manifest, error) {
	return readManifest(path)
}

// Filter selects the entries of an archive: a name it answers false for is left in
// the file for a later pass.
type Filter func(name string) bool

// Owner is who an extraction's entries belong to. Nil is the user who ran box.
type Owner struct{ UID, GID int }

// Open verifies an archive and extracts it to dir, which must not exist yet.
//
// It refuses a missing or truncated archive, an entry the manifest does not name
// and a manifest entry the archive does not hold, a size or sha256 that does not
// match, an absolute or `..` path, and a link, device, fifo or hardlink box never
// writes. A failure leaves nothing at dir.
//
// The extraction is 0700 and belongs to whoever ran box (ownership is not carried,
// because only apply knows the uid that will own the staged copy), and inside it
// every entry has the mode and time the source gave it: a config tree is copied on
// from here, and a script in it has to stay one.
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
// keep is what part of an archive this pass writes (nil is all of it), and every
// entry it skips has to have been checked already by Verify. What it does not write
// is still checked as it is skipped.
func Extract(archive, dir string, m *Manifest, keep Filter, own *Owner) error {
	at, err := safefs.OpenAnchor(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("extract to %s: %w", dir, err)
	}
	defer at.Close()
	return extractInto(at, filepath.Base(dir), archive, dir, "", m, keep, own)
}

// ExtractTree writes the subtree under prefix into name, which must not exist yet and
// is named inside the root the caller holds: config/<instance>/ becomes the instance's
// config directory at the config root, whose user can write it, so the tree is made
// through the run's own handle rather than a path resolved again. The prefix is taken
// off the paths, and the bytes are checked against the manifest as they are written.
func ExtractTree(at *safefs.Anchor, name, archive, prefix string, m *Manifest, own *Owner) error {
	return extractInto(at, name, archive, at.Path(name), path.Clean(prefix), m, nil, own)
}

// extractInto makes name inside at and fills it. The tree stays box's own, 0700,
// until every entry is written and none of the work goes through a path resolved
// outside it, so the seedbox's user -- who can write the directory holding it, and
// who would be handed the link's target by a chown by path -- cannot reach anything
// under it while it is being filled. The tree is given to the owner last.
func extractInto(at *safefs.Anchor, name, archive, dir, base string, m *Manifest, keep Filter, own *Owner) error {
	if _, err := at.Root().Lstat(name); err == nil {
		return fmt.Errorf("extract to %s: it already exists, and a snapshot extracts to a new directory", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("extract to %s: %w", dir, at.Err(name, err))
	}
	tree, err := safefs.Make(at, name, 0o700)
	if err != nil {
		return fmt.Errorf("extract to %s: %w", dir, err)
	}
	defer tree.Close()
	if err := extract(archive, tree, base, m, keep, own); err != nil {
		// A half-extracted tree is worse than none: it looks like a snapshot.
		if rerr := tree.Remove(); rerr != nil {
			return fmt.Errorf("%w (and %s could not be removed: %v)", err, dir, rerr)
		}
		return err
	}
	return nil
}

// extract is the second pass: every tar entry keep accepts is checked against the
// manifest and written, and every manifest entry keep accepts has to appear. base,
// when not empty, is the one subtree written, with its name taken off the paths.
func extract(archive string, tree *safefs.Tree, base string, m *Manifest, keep Filter, own *Owner) error {
	f, gz, err := openArchive(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	ex := &extractor{archive: archive, tree: tree, m: m, keep: keep, own: own, seen: make(map[string]bool, len(m.Entries))}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}
		if err := ex.entry(tr, hdr, base); err != nil {
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
			// A later pass writes it, and Verify checked it.
			continue
		}
		if base != "" && name != base && !strings.HasPrefix(name, base+"/") {
			continue
		}
		if !ex.seen[name] {
			return fmt.Errorf("%s does not hold %s, which its manifest names: the copy is incomplete", archive, name)
		}
	}
	return ex.give()
}

// extractor is one extraction in progress: the tree it writes through, and the
// entries whose owner, mode and time are given once the whole tree is in.
type extractor struct {
	archive string
	tree    *safefs.Tree
	m       *Manifest
	keep    Filter
	own     *Owner
	seen    map[string]bool
	// held is every entry waiting for the end: a directory's mode, time and owner,
	// a link's owner alone.
	held []held
	// top is the tree's own directory entry, when the archive has one: a subtree
	// extracted as an instance's config directory is the source's.
	top *held
}

// held is one extracted path's source mode and time, out of its tar header, which
// checkEntry has held to the manifest's mode.
type held struct {
	// name is the path inside the archive, which is what a refusal prints.
	name  string
	rel   string
	mode  fs.FileMode
	mtime time.Time
	// link is a symlink: it is owned (never followed) and has no mode or time of
	// its own to set.
	link bool
}

func heldOf(rel, name string, hdr *tar.Header) held {
	return held{name: name, rel: rel, mode: fs.FileMode(hdr.Mode) & fs.ModePerm, mtime: hdr.ModTime}
}

// entry checks one tar entry against the manifest and writes it, unless keep
// leaves it for a later pass. A directory and a link are owned and given their mode
// and time at the end, once nothing is written into them any more; a file's are set
// while it is open.
func (ex *extractor) entry(tr *tar.Reader, hdr *tar.Header, base string) error {
	name := path.Clean(hdr.Name)
	// The manifest was verified in the first pass, and readManifest refuses an
	// archive with anything after it; the entry itself is not extracted.
	if name == ManifestName {
		return nil
	}
	if !safeName(name) {
		return fmt.Errorf("%s holds %q, which is not a path inside an archive", ex.archive, hdr.Name)
	}
	if ex.m.lookup(name) == nil {
		return fmt.Errorf("%s holds %s, which its manifest does not name", ex.archive, name)
	}
	if ex.seen[name] {
		return fmt.Errorf("%s holds %s twice", ex.archive, name)
	}
	if ex.keep != nil && !ex.keep(name) {
		return nil
	}
	rel := name
	if base != "" {
		if name == base {
			ex.seen[name] = true
			return ex.base(name, tr, hdr)
		}
		var ok bool
		if rel, ok = strings.CutPrefix(name, base+"/"); !ok {
			return nil
		}
	}
	if err := beforeEntry(name); err != nil {
		return err
	}
	ex.seen[name] = true
	if err := ex.parents(rel, name); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if _, err := checkEntry(ex.archive, nil, ex.m, tr, hdr); err != nil {
			return err
		}
		if err := ex.mkdir(rel, name); err != nil {
			return err
		}
		ex.held = append(ex.held, heldOf(rel, name, hdr))
		return nil
	case tar.TypeReg:
		return ex.file(rel, name, tr, hdr)
	case tar.TypeSymlink:
		if _, err := checkEntry(ex.archive, nil, ex.m, tr, hdr); err != nil {
			return err
		}
		if err := ex.link(rel, name, hdr.Linkname); err != nil {
			return err
		}
		ex.held = append(ex.held, held{name: name, rel: rel, link: true})
		return nil
	}
	// checkEntry refuses the rest by name: a hardlink, a device, a fifo.
	_, err := checkEntry(ex.archive, nil, ex.m, tr, hdr)
	return err
}

// base takes the subtree's own directory entry: the extraction directory itself is
// what it becomes, and it is already there. It is the instance's config directory,
// so its mode is the source's too.
func (ex *extractor) base(name string, tr *tar.Reader, hdr *tar.Header) error {
	if hdr.Typeflag != tar.TypeDir {
		return fmt.Errorf("%s holds %s as a %s, and it is the tree being extracted: it has to be a directory", ex.archive, name, typeName(hdr.Typeflag))
	}
	if _, err := checkEntry(ex.archive, nil, ex.m, tr, hdr); err != nil {
		return err
	}
	top := heldOf(".", name, hdr)
	ex.top = &top
	return nil
}

// file writes one regular file, checks its bytes against the manifest as they are
// written, and gives it its owner, mode and time: the first two through the handle
// it is already open on, so the file that was checked is the file that is owned.
func (ex *extractor) file(rel, name string, tr *tar.Reader, hdr *tar.Header) error {
	root := ex.tree.Root()
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("extract %s: %w", name, ex.tree.Err(rel, err))
	}
	_, cerr := checkEntry(ex.archive, out, ex.m, tr, hdr)
	if cerr == nil && ex.own != nil {
		if err := out.Chown(ex.own.UID, ex.own.GID); err != nil {
			cerr = fmt.Errorf("own %s: %w", ex.tree.Path(rel), err)
		}
	}
	if cerr == nil {
		if err := out.Chmod(fs.FileMode(hdr.Mode) & fs.ModePerm); err != nil {
			cerr = fmt.Errorf("extract %s: %w", ex.tree.Path(rel), err)
		}
	}
	if err := out.Close(); cerr == nil {
		cerr = ex.tree.Err(rel, err)
	}
	if cerr != nil {
		return cerr
	}
	if err := root.Chtimes(rel, hdr.ModTime, hdr.ModTime); err != nil {
		return fmt.Errorf("extract %s: %w", ex.tree.Path(rel), ex.tree.Err(rel, err))
	}
	return nil
}

// link writes one symlink under internal/linktree's rule, so a link that leaves the
// tree the archive names is refused rather than materialised.
func (ex *extractor) link(rel, name, linkname string) error {
	res, err := linktree.Resolve(ex.tree.Dir(), filepath.FromSlash(rel), linkname)
	if err != nil {
		return err
	}
	if res.Skip {
		return fmt.Errorf("%s is a link to %s, which names the kernel's own filesystems: box leaves such a link out, so this is not an archive box wrote", name, linkname)
	}
	if err := ex.tree.Root().Symlink(res.Target, rel); err != nil {
		return fmt.Errorf("extract %s: %w", name, ex.tree.Err(rel, err))
	}
	return nil
}

// parents creates the directories a path needs, 0700, and refuses a parent that is
// already something else: an archive cannot be allowed to write through a link it
// brought with it.
func (ex *extractor) parents(rel, name string) error {
	parent := path.Dir(rel)
	if parent == "." || parent == "/" {
		return nil
	}
	root := ex.tree.Root()
	parts := strings.Split(parent, "/")
	for i := range parts {
		at := path.Join(parts[:i+1]...)
		info, err := root.Lstat(at)
		switch {
		case err == nil:
			if !info.IsDir() {
				return fmt.Errorf("extract %s: %s is not a directory", name, at)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(at, 0o700); err != nil {
				return fmt.Errorf("extract %s: %w", name, ex.tree.Err(at, err))
			}
		default:
			return fmt.Errorf("extract %s: %w", name, ex.tree.Err(at, err))
		}
	}
	return nil
}

// mkdir makes one directory entry, 0700 until give() sets the source's mode, and
// refuses a path that is already something else.
func (ex *extractor) mkdir(rel, name string) error {
	root := ex.tree.Root()
	if info, err := root.Lstat(rel); err == nil && !info.IsDir() {
		return fmt.Errorf("extract %s: it is already there and is not a directory", name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("extract %s: %w", name, ex.tree.Err(rel, err))
	}
	if err := root.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("extract %s: %w", name, ex.tree.Err(rel, err))
	}
	if err := root.Chmod(rel, 0o700); err != nil {
		return fmt.Errorf("extract %s: %w", name, ex.tree.Err(rel, err))
	}
	return nil
}

// give gives the extraction to the owner: every entry deepest first, and the tree's
// own directory last. A directory's own mtime is its last child written, and until
// the top is given away the owner cannot enter the tree at all, so what it holds is
// box's own while box is still working in it.
func (ex *extractor) give() error {
	order := make([]held, len(ex.held))
	copy(order, ex.held)
	sort.SliceStable(order, func(i, j int) bool {
		di, dj := strings.Count(order[i].rel, "/"), strings.Count(order[j].rel, "/")
		if di != dj {
			return di > dj
		}
		return order[i].rel < order[j].rel
	})
	for _, h := range order {
		if err := ex.hand(h); err != nil {
			return err
		}
	}
	return ex.giveTop()
}

// hand gives one entry to the owner and sets its mode and time. It never follows a
// link: a directory that is one by now is not the directory that was extracted.
func (ex *extractor) hand(h held) error {
	root := ex.tree.Root()
	if ex.own != nil {
		if err := root.Lchown(h.rel, ex.own.UID, ex.own.GID); err != nil {
			return fmt.Errorf("own %s: %w", ex.tree.Path(h.rel), ex.tree.Err(h.rel, err))
		}
	}
	if h.link {
		return nil
	}
	if err := root.Chmod(h.rel, h.mode); err != nil {
		return fmt.Errorf("extract %s: %w", ex.tree.Path(h.rel), ex.tree.Err(h.rel, err))
	}
	if err := root.Chtimes(h.rel, h.mtime, h.mtime); err != nil {
		return fmt.Errorf("extract %s: %w", ex.tree.Path(h.rel), ex.tree.Err(h.rel, err))
	}
	return nil
}

// giveTop gives the extraction's own directory to the owner, last, through a handle
// that is opened without following a link: a tree replaced by one since it was made
// is refused rather than owned.
func (ex *extractor) giveTop() error {
	mode := fs.FileMode(0o700)
	if ex.top != nil {
		mode = ex.top.mode
	}
	if ex.own != nil {
		if err := ex.tree.Give(ex.own.UID, ex.own.GID, mode); err != nil {
			return fmt.Errorf("own %s: %w", ex.tree.Dir(), err)
		}
	} else if err := ex.tree.Root().Chmod(".", mode); err != nil {
		return fmt.Errorf("extract to %s: %w", ex.tree.Dir(), err)
	}
	if ex.top != nil {
		if err := ex.tree.Root().Chtimes(".", ex.top.mtime, ex.top.mtime); err != nil {
			return fmt.Errorf("extract to %s: %w", ex.tree.Dir(), err)
		}
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
	// tar answers io.EOF at its end-of-archive marker without reading the rest of
	// the file, so the gzip stream is drained here: its trailer is the only thing
	// that says the copy reached the end rather than stopping a byte short.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("read %s: %w", archive, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%s has no %s: it is not a snapshot, or the copy stopped before the end", archive, ManifestName)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A key this box does not know is a newer manifest than it can honour, and
	// ignoring it would read one field's meaning out of a shape built for another.
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
