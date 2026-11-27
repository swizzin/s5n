package archive

// swap_test.go proves what a tree the seedbox's user can write can do to an
// extraction, and what it cannot: an entry, or the directory holding the extraction
// itself, swapped for a link while box is working in the tree leaves the link's
// target -- a file outside it -- untouched, and the run refuses by name.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/safefs"
)

// swapTime is the time every entry of a swap test's archive carries.
var swapTime = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// swapArchive writes these entries, in this order, each with swapTime as its time:
// the order is what a test needs, since the swap happens between two of them.
func swapArchive(t *testing.T, entries ...craftEntry) string {
	t.Helper()
	m := manifestOf(entries...)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "swap.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.linkname, ModTime: swapTime, Format: tar.FormatPAX}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	hdr := &tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(raw)), Typeflag: tar.TypeReg, ModTime: swapTime, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// swapEntry is the one test seam the extraction has.
func swapEntry(t *testing.T, swap func(name string) error) {
	t.Helper()
	beforeEntry = swap
	t.Cleanup(func() { beforeEntry = func(string) error { return nil } })
}

// outsideDir is a directory outside the tree, with a mode and a time of its own:
// what a chown, a chmod or a chtimes that followed a link would change.
func outsideDir(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chmod(dir, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir, at
}

// A directory of the extraction swapped for a link to one outside it is not given
// the source's mode and time at the link's target: the owner and mode the end of a
// pass sets are set on the directory box made, and a link there by then is a
// refusal naming it.
func TestExtractDoesNotFinishADirectorySwappedForALink(t *testing.T) {
	const sub = "config/app/sub"
	path := swapArchive(t,
		craftEntry{name: "config/app", typeflag: tar.TypeDir, mode: 0o750},
		craftEntry{name: sub, typeflag: tar.TypeDir, mode: 0o750},
		craftEntry{name: sub + "/keep.conf", typeflag: tar.TypeReg, mode: 0o600, data: []byte("mine\n")},
		craftEntry{name: "config/app/other.conf", typeflag: tar.TypeReg, mode: 0o600, data: []byte("other\n")},
	)
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	outside, at := outsideDir(t)
	dir := filepath.Join(t.TempDir(), "extract")
	swapped := false
	swapEntry(t, func(name string) error {
		if swapped || name != "config/app/other.conf" {
			return nil
		}
		swapped = true
		if err := os.Rename(filepath.Join(dir, "config", "app", "sub"), filepath.Join(dir, "config", "app", "sub.real")); err != nil {
			return err
		}
		return os.Symlink(outside, filepath.Join(dir, "config", "app", "sub"))
	})
	err = Extract(path, dir, m, nil, &Owner{UID: os.Geteuid(), GID: os.Getegid()})
	if err == nil {
		t.Fatalf("the extraction finished with %s a link out of the tree", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Errorf("the refusal does not name the entry: %v", err)
	}
	info, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o701 {
		t.Errorf("%s, outside the tree, is %04o: the extraction set the source's mode on the link's target", outside, info.Mode().Perm())
	}
	if !info.ModTime().Equal(at) {
		t.Errorf("%s, outside the tree, was modified %s, want %s", outside, info.ModTime(), at)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("%s, outside the tree, holds %v (%v): the extraction wrote through the link", outside, entries, err)
	}
}

// The directory holding the extraction is the seedbox's user's too. Swapped for a
// link once the tree is made, the tree still goes where box made it, never into the
// directory the link names.
func TestExtractDoesNotWriteThroughTheDirectoryHoldingIt(t *testing.T) {
	path := swapArchive(t,
		craftEntry{name: "config/app", typeflag: tar.TypeDir, mode: 0o750},
		craftEntry{name: "config/app/settings.conf", typeflag: tar.TypeReg, mode: 0o600, data: []byte("mine\n")},
	)
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "extract")
	outside := t.TempDir()
	// The directory the link names, with the extraction's own name already in it:
	// a by-path write lands in it, one through a handle does not.
	landing := filepath.Join(outside, "extract")
	if err := os.Mkdir(landing, 0o700); err != nil {
		t.Fatal(err)
	}
	swapped := false
	swapEntry(t, func(string) error {
		if swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(work, work+".real"); err != nil {
			return err
		}
		return os.Symlink(outside, work)
	})
	// Either answer is the right one -- a refusal, or a tree the owner can reach at
	// the name it was told -- and neither is a write into the directory the link
	// names.
	_ = Extract(path, dir, m, nil, &Owner{UID: os.Geteuid(), GID: os.Getegid()})
	if entries, err := os.ReadDir(landing); err != nil || len(entries) != 0 {
		t.Errorf("%s, named by the link, holds %v (%v): the extraction wrote through it", landing, entries, err)
	}
}

// The tree a run writes a snapshot's config into hangs off the seedbox's config
// root, which its user owns: the tree is made through the handle the caller holds on
// that root, so a `.box` swapped for a link out of it is a refusal and the directory
// the link names is left alone.
func TestExtractTreeWritesOnlyInsideItsRoot(t *testing.T) {
	path, _ := modeArchive(t)
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	configRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configRoot, ".box", "migrate", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.OpenAnchor(configRoot)
	if err != nil {
		t.Fatalf("open the config root: %v", err)
	}
	defer root.Close()
	outside := t.TempDir()
	if err := os.Rename(filepath.Join(configRoot, ".box"), filepath.Join(outside, "box")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "box"), filepath.Join(configRoot, ".box")); err != nil {
		t.Fatal(err)
	}

	name := ".box/migrate/abc/app"
	err = ExtractTree(root, name, path, "config/app", m, &Owner{UID: os.Geteuid(), GID: os.Getegid()})
	if err == nil {
		t.Fatal("the extraction ran through a .box that is a link out of the config root")
	}
	if !strings.Contains(err.Error(), ".box") {
		t.Errorf("the refusal does not name the directory it would have written through:\n%v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "box", "migrate", "abc", "app")); !os.IsNotExist(err) {
		t.Errorf("the snapshot was extracted outside the config root: %v", err)
	}
}
