package space

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestSizeIsTheDuplicateCountOfATree: a skipped directory takes its subtree with
// it.
func TestSizeIsTheDuplicateCountOfATree(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Preferences.xml", 100)
	write("Plug-in Support/Databases/library.db", 500)
	write("Metadata/Posters/poster.jpg", 700)
	write("Cache/PhotoTranscoder/thumb.jpg", 9000)
	write("Cache/HTTP/a.bin", 9000)
	write("Logs/Plex Media Server.log", 3)
	if err := os.Symlink("/somewhere/else", filepath.Join(dir, "Metadata", "link")); err != nil {
		t.Fatal(err)
	}

	all, err := Size(dir, nil)
	if err != nil {
		t.Fatalf("size the tree: %v", err)
	}
	if want := int64(100 + 500 + 700 + 9000 + 9000 + 3); all != want {
		t.Errorf("the whole tree is %d bytes, want %d (a symlink is its link, not its target)", all, want)
	}
	// Skipped directories are named the way the archive walks: relative, slash
	// form.
	kept, err := Size(dir, []string{"Cache", "Logs/"})
	if err != nil {
		t.Fatalf("size the tree without its caches: %v", err)
	}
	if want := int64(100 + 500 + 700); kept != want {
		t.Errorf("the tree without Cache and Logs is %d bytes, want %d", kept, want)
	}
}

// TestSizeOfAMissingTreeIsAnError: a path the run cannot read is a refusal, not
// zero bytes.
func TestSizeOfAMissingTreeIsAnError(t *testing.T) {
	if _, err := Size(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("sizing a directory that is not there was answered")
	}
}

// TestFreeAsksTheFilesystemThePathWillBeOn: a directory the install has not made
// yet is answered by its parent's filesystem.
func TestFreeAsksTheFilesystemThePathWillBeOn(t *testing.T) {
	dir := t.TempDir()
	free, err := Free(dir)
	if err != nil {
		t.Fatalf("free space at %s: %v", dir, err)
	}
	if free <= 0 {
		t.Errorf("free space at %s is %d, and a test runs on a filesystem with room", dir, free)
	}
	under, err := Free(filepath.Join(dir, "not", "created", "yet"))
	if err != nil {
		t.Fatalf("free space above a path that is not there: %v", err)
	}
	if under <= 0 {
		t.Errorf("free space above a path that is not there is %d", under)
	}
}

// TestSameKnowsOneFilesystemFromTwo: two paths count twice only on one
// filesystem.
func TestSameKnowsOneFilesystemFromTwo(t *testing.T) {
	dir := t.TempDir()
	same, err := Same(dir, filepath.Join(dir, "not", "there", "yet"))
	if err != nil {
		t.Fatalf("same filesystem: %v", err)
	}
	if !same {
		t.Errorf("%s and a directory under it are on different filesystems", dir)
	}
	if dev, err := device(dir); err != nil || dev == 0 {
		t.Errorf("device of %s = %d, %v", dir, dev, err)
	}
}

// TestMarginIsATenthAndNeverLessThanAFloor: even an empty tree needs the floor.
func TestMarginIsATenthAndNeverLessThanAFloor(t *testing.T) {
	if got := Margin(0); got != marginFloor {
		t.Errorf("the margin of an empty tree is %d, want the floor %d", got, marginFloor)
	}
	big := int64(60) << 30
	if got, want := Margin(big), big/10; got != want {
		t.Errorf("the margin of 60 GiB is %d, want %d", got, want)
	}
}

// TestChooseNeedsRoomForEveryCopy: a host that is both ends carries it twice on
// one filesystem.
func TestChooseNeedsRoomForEveryCopy(t *testing.T) {
	const gib = int64(1) << 30
	size := 20 * gib
	need := size + Margin(size)

	t.Run("full", func(t *testing.T) {
		v := Choose(size, []Place{{Path: "/var/lib/s5n/migrate", Free: need}})
		if !v.Fits() || v.Need != need {
			t.Errorf("a filesystem with exactly the margin free = %+v, want it to fit", v)
		}
	})
	t.Run("short at the snapshot directory", func(t *testing.T) {
		v := Choose(size, []Place{{Path: "/var/lib/s5n/migrate", Free: need - 1}})
		if v.Fits() || v.Short == nil || v.Short.Path != "/var/lib/s5n/migrate" {
			t.Fatalf("a filesystem one byte short = %+v, want it short there", v)
		}
	})
	t.Run("short at the config path", func(t *testing.T) {
		// The target has room for the tree but not the margin.
		v := Choose(size, []Place{
			{Path: "/var/lib/s5n/migrate", Free: need},
			{Path: "/var/lib/seedbox/config", Free: size},
		})
		if v.Fits() || v.Short == nil || v.Short.Path != "/var/lib/seedbox/config" {
			t.Fatalf("a target one margin short = %+v, want it short at the config path", v)
		}
	})
	t.Run("same host counted twice", func(t *testing.T) {
		// Room for one copy of two is short.
		both := []Place{{Path: "/var/lib/s5n/migrate", Free: need, Copies: 2}}
		if v := Choose(size, both); v.Fits() {
			t.Errorf("one filesystem with room for one copy of two = %+v, want it short", v)
		}
		both[0].Free = 2 * need
		if v := Choose(size, both); !v.Fits() {
			t.Errorf("one filesystem with room for both copies = %+v, want it to fit", v)
		}
	})
}

// TestHumanSaysSizesTheWayBoxDoes: the numbers a person compares against df.
func TestHumanSaysSizesTheWayBoxDoes(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 bytes"},
		{1023, "1023 bytes"},
		{1024, "1.0 KiB"},
		{494 << 20, "494.0 MiB"},
		{60 << 30, "60.0 GiB"},
	} {
		if got := Human(c.n); got != c.want {
			t.Errorf("Human(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// goneEntry is an entry whose file has gone by the time the walk asks, as a live
// source's temporary file does.
type goneEntry struct{ name string }

func (e goneEntry) Name() string               { return e.name }
func (e goneEntry) IsDir() bool                { return false }
func (e goneEntry) Type() fs.FileMode          { return 0 }
func (e goneEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

// TestTallyCountsAFileThatWentAwayAsNothing: a name listed but gone when stat'd
// counts nothing, or a temporary file would stop a migration.
func TestTallyCountsAFileThatWentAwayAsNothing(t *testing.T) {
	dir := t.TempDir()
	c := &counter{dir: dir}
	if err := c.entry(filepath.Join(dir, "Preferences.xml.tmp.7133"), goneEntry{"Preferences.xml.tmp.7133"}, nil); err != nil {
		t.Fatalf("a file that went away while the walk ran stopped it: %v", err)
	}
	if c.files != 0 || c.bytes != 0 {
		t.Errorf("the walk counted %d files and %d bytes for a file that is not there", c.files, c.bytes)
	}
	// The tree itself is still a refusal.
	if _, _, err := Tally(filepath.Join(dir, "gone"), nil); err == nil {
		t.Error("a tree that is not there was counted as nothing")
	}
}

// TestTallyCountsADirectoryThatWentAwayAsNothing: one level up, likewise
// nothing.
func TestTallyCountsADirectoryThatWentAwayAsNothing(t *testing.T) {
	dir := t.TempDir()
	c := &counter{dir: dir}
	if err := c.entry(filepath.Join(dir, "Cache"), goneEntry{"Cache"}, fs.ErrNotExist); err != nil {
		t.Fatalf("a directory that went away while the walk ran stopped it: %v", err)
	}
}
