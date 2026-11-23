package bundle

// carry_test.go proves the split a full carry takes through a snapshot: the
// archive holds Plex's whole data, the extraction keeps the state the checks read
// and nothing else, the numbers a summary prints come from the pass that verified
// the archive, and the tree is written once -- at the config root -- by Stage.
//
// It also proves the two refusals that make the split safe: a damaged snapshot is
// refused before anything is extracted, and a snapshot whose state does not fit
// beside it is refused with the numbers, before its first write.

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/plex"
)

// plexSnapshot writes one final snapshot of a source that ran Plex with its whole
// data, and returns its path and the tree it was taken from.
func plexSnapshot(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeFinal, time.Now()))
	tree := homeTree(t)
	scope := plex.Snapshot(true)
	if _, err := Write(path, Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   []byte(testPlan),
		Report: []byte(testReport),
		Configs: []Config{
			{Instance: "plex", Dir: tree, Sub: scope.Sub, Skip: scope.Skip},
		},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return path, tree
}

// homeTree is the config directory a real Plex keeps: everything -- the state,
// the Metadata tree of covers and the cache it rebuilds -- under the one
// directory the image names, which is the subtree a snapshot of Plex carries.
func homeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "Library", "Application Support", "Plex Media Server")
	for rel, body := range map[string]string{
		"Preferences.xml": "<Preferences/>\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db":     "sqlite\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db-wal": "wal\n",
		"Metadata/Posters/poster.jpg":                                  "jpeg\n",
		"Cache/PhotoTranscoder/thumb.jpg":                              "jpeg\n",
		"Logs/Plex Media Server.log":                                   "log\n",
	} {
		at := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stateOnly is what an apply's extraction keeps of a full carry: Plex's
// Preferences.xml, its databases, and the directories above them.
func stateOnly(name string) bool {
	rest, ok := strings.CutPrefix(name, archive.ConfigPrefix+"/plex/")
	if !ok {
		return true
	}
	return plex.State(rest)
}

// TestTheExtractionKeepsPlexsStateAndTheArchiveKeepsItsTree: the checks and the
// pre-install pass read a Plex config directory for its state, and the tree is
// tens of gigabytes they never open -- so it stays in the snapshot, and the
// numbers that describe it come from the pass that verified the archive.
func TestTheExtractionKeepsPlexsStateAndItsTreeIsStagedOnce(t *testing.T) {
	path, tree := plexSnapshot(t)
	out := filepath.Join(t.TempDir(), "extract")
	opened, err := Open(path, out, stateOnly, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	home := filepath.Join(opened.Configs["plex"], "Library", "Application Support", "Plex Media Server")
	for _, kept := range []string{"Preferences.xml", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(kept))); err != nil {
			t.Errorf("the extraction holds no %s: %v", kept, err)
		}
	}
	for _, left := range []string{"Metadata", "Cache"} {
		if _, err := os.Stat(filepath.Join(opened.Configs["plex"], left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the extraction holds %s, and no check reads it: %v", left, err)
		}
		if _, err := os.Stat(filepath.Join(home, left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the extraction holds Plex's %s, and no check reads it: %v", left, err)
		}
	}
	c := opened.Contents["plex"]
	if !c.Tree {
		t.Error("the snapshot holds Plex's whole data, and Open says it kept all of it")
	}
	if c.Files == 0 || c.Bytes == 0 {
		t.Errorf("the snapshot holds %d files and %d bytes of Plex's config, want the tree's numbers", c.Files, c.Bytes)
	}
	// The numbers are the whole instance's: what Stage writes -- the tree the
	// snapshot carried, which is the source's less the caches it left out.
	want := tallyOf(t, tree, plex.Snapshot(true).Skip)
	if c.Files != want.Files || c.Bytes != want.Bytes {
		t.Errorf("the snapshot holds %d files and %d bytes, want %d and %d (the tree minus its caches)",
			c.Files, c.Bytes, want.Files, want.Bytes)
	}

	// The tree is written once, at the config root, out of the snapshot: the same
	// bytes the source had, with the caches the snapshot left out absent.
	dst := filepath.Join(t.TempDir(), "plex")
	staged, err := opened.Stage("plex", dst, os.Geteuid(), os.Getegid())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staged.Files != want.Files || staged.Bytes != want.Bytes {
		t.Errorf("Stage wrote %d files and %d bytes, want %d and %d: the row a summary prints has to be what landed",
			staged.Files, staged.Bytes, want.Files, want.Bytes)
	}
	for _, carried := range []string{"Preferences.xml", "Metadata/Posters/poster.jpg", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		if _, err := os.Stat(filepath.Join(dst, "Library", "Application Support", "Plex Media Server", filepath.FromSlash(carried))); err != nil {
			t.Errorf("the staged copy holds no %s: %v", carried, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "Cache")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staged copy holds Plex's Cache tree, which no snapshot carries: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "Library", "Application Support", "Plex Media Server", "Preferences.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("a staged file is %04o, want 0600: it holds the app's own settings", info.Mode().Perm())
	}
}

// tallyOf is what a copy of a tree writes: its regular files and their bytes,
// which is the rule the extraction counts by.
func tallyOf(t *testing.T, tree string, skip []string) Contents {
	t.Helper()
	home := filepath.Join(tree, "Library", "Application Support", "Plex Media Server")
	dropped := map[string]bool{}
	for _, s := range skip {
		dropped[filepath.Join(home, filepath.FromSlash(s))] = true
	}
	var c Contents
	err := filepath.Walk(tree, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if dropped[p] {
			return filepath.SkipDir
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		c.Files++
		c.Bytes += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestASnapshotThatKeepsEverythingSaysSo: a snapshot whose configs are all
// extracted holds no tree, and its numbers are the files it holds.
func TestASnapshotThatKeepsEverythingSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeFinal, time.Now()))
	if _, err := Write(path, Snapshot{
		Mode:    archive.ModeFinal,
		Plan:    []byte(testPlan),
		Configs: []Config{{Instance: "sonarr", Dir: tree(t)}},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opened, err := Open(path, filepath.Join(dir, "open"), nil, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c := opened.Contents["sonarr"]; c.Tree {
		t.Errorf("a snapshot that was extracted whole says it holds a tree: %+v", c)
	} else if c.Files != 2 || c.Bytes == 0 {
		t.Errorf("the snapshot counts %d files and %d bytes, want the tree's two files", c.Files, c.Bytes)
	}
}

// TestADamagedSnapshotIsRefusedBeforeItsExtraction: one byte of one file is not
// the manifest's, and the run stops with nothing extracted -- the snapshot's tree
// is written after the install, so a copy that lost a byte has to stop the run
// now.
func TestADamagedSnapshotIsRefusedBeforeItsExtraction(t *testing.T) {
	path, _ := plexSnapshot(t)
	tamper(t, path, archive.ConfigPrefix+"/plex/Library/Application Support/Plex Media Server/Preferences.xml")
	out := filepath.Join(t.TempDir(), "extract")
	if _, err := Open(path, out, stateOnly, nil); err == nil {
		t.Fatal("Open accepted a snapshot whose bytes are not its manifest's")
	} else if !strings.Contains(err.Error(), "does not match its manifest") {
		t.Errorf("the refusal is %q, want the file that does not match its manifest", err)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused snapshot left %s behind", out)
	}
}

// tamper rewrites one member's bytes and leaves its size alone, so the manifest's
// digest is what catches it rather than its length.
func tamper(t *testing.T, path, name string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		hdr  *tar.Header
		data []byte
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(strings.TrimPrefix(hdr.Name, "./")) == name {
			if len(raw) == 0 {
				t.Fatalf("%s holds no bytes to change", name)
			}
			raw[0] ^= 0xff
		}
		entries = append(entries, struct {
			hdr  *tar.Header
			data []byte
		}{hdr, raw})
	}
	f.Close()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// fixedDisk is one filesystem with one answer for every path.
type fixedDisk struct{ free int64 }

func (d fixedDisk) Free(string) (int64, error) { return d.free, nil }

func (d fixedDisk) Same(string, string) (bool, error) { return true, nil }

// TestTheExtractionsRoomIsTheManifestsNumbers: the room a snapshot's state needs
// is the manifest's, asked before anything is written, and a filesystem that
// cannot hold it is a refusal that names the two numbers and the directory.
func TestTheExtractionsRoomIsTheManifestsNumbers(t *testing.T) {
	path, _ := plexSnapshot(t)
	m, err := archive.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var need int64
	for name, e := range m.Entries {
		if stateOnly(name) {
			need += e.Size
		}
	}
	if need == 0 {
		t.Fatal("the snapshot's state is no bytes at all, so this test cannot say anything")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "extract")
	_, err = Open(path, out, stateOnly, fixedDisk{free: need - 1})
	if err == nil {
		t.Fatal("Open extracted a snapshot into a filesystem that cannot hold it")
	}
	for _, want := range []string{strconv.FormatInt(need, 10), out} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused extraction left %s behind", out)
	}
	// One byte more than it needs, and the run goes ahead: the check is the
	// extraction's own room and nothing else.
	if _, err := Open(path, out, stateOnly, fixedDisk{free: need}); err != nil {
		t.Fatalf("Open with exactly the room it needs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, archive.PlanName)); err != nil {
		t.Errorf("the extraction holds no plan: %v", err)
	}
}
