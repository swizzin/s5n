package space

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSizeIsTheDuplicateCountOfATree: the walk counts what a copy would write,
// one stat per entry, and a skipped directory takes its whole subtree with it.
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
	// The skipped directories are the ones a full Plex snapshot leaves out, and
	// they are named the way the archive walks the tree: relative, slash form.
	kept, err := Size(dir, []string{"Cache", "Logs/"})
	if err != nil {
		t.Fatalf("size the tree without its caches: %v", err)
	}
	if want := int64(100 + 500 + 700); kept != want {
		t.Errorf("the tree without Cache and Logs is %d bytes, want %d", kept, want)
	}
}

// TestSizeOfAMissingTreeIsAnError: a path the run cannot read is a refusal, not
// zero bytes that fit anywhere.
func TestSizeOfAMissingTreeIsAnError(t *testing.T) {
	if _, err := Size(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("sizing a directory that is not there was answered")
	}
}

// TestFreeAsksTheFilesystemThePathWillBeOn: the config directory an install
// creates does not exist when the question is asked, so the answer is its
// parent's filesystem, and a real one answers with real numbers.
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

// TestSameKnowsOneFilesystemFromTwo: a snapshot and the seedbox's copy on one
// machine are counted twice only when they really are one filesystem.
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

// TestMarginIsATenthAndNeverLessThanAFloor: a tree of nothing still needs room
// beside it, and a large one needs a tenth of itself.
func TestMarginIsATenthAndNeverLessThanAFloor(t *testing.T) {
	if got := Margin(0); got != marginFloor {
		t.Errorf("the margin of an empty tree is %d, want the floor %d", got, marginFloor)
	}
	big := int64(60) << 30
	if got, want := Margin(big), big/10; got != want {
		t.Errorf("the margin of 60 GiB is %d, want %d", got, want)
	}
}

// TestChooseNeedsRoomForEveryCopy: a full Plex tree is carried only when every
// filesystem it lands on has room for it, and a host that is both ends of the
// migration carries it twice on one filesystem.
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
		// The snapshot's own filesystem has room; the target's has room for the
		// tree but not for the margin, and that is short.
		v := Choose(size, []Place{
			{Path: "/var/lib/s5n/migrate", Free: need},
			{Path: "/var/lib/seedbox/config", Free: size},
		})
		if v.Fits() || v.Short == nil || v.Short.Path != "/var/lib/seedbox/config" {
			t.Fatalf("a target one margin short = %+v, want it short at the config path", v)
		}
	})
	t.Run("same host counted twice", func(t *testing.T) {
		// One filesystem holding both the snapshot and the copy beside it needs
		// room for two, and one tree's worth is not enough.
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

// TestHumanSaysSizesTheWayBoxDoes: the numbers in a line are the ones a person
// compares against df.
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
