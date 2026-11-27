package safefs

// safefs_test.go proves the handle, not the path, is the tree: what box writes is
// found again at the directory it was opened on, and a name that is a link out of
// the directory holding it is refused rather than followed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tree swapped for a link after box made it is not given to the owner at the
// link's target: the directory box wrote into is the one that moves.
func TestGiveRefusesATreeReplacedByALink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tree")
	tree, err := Make(dir, 0o700)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	defer tree.Close()
	if err := tree.Root().WriteFile("settings.conf", []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o701); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}

	if err := tree.Give(os.Geteuid(), os.Getegid(), 0o750); err == nil {
		t.Fatalf("%s, a link out of %s, was given to %d:%d", dir, base, os.Geteuid(), os.Getegid())
	} else if got := err.Error(); got == "" || !contains(got, dir) {
		t.Errorf("the refusal does not name the tree: %v", err)
	}
	info, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o701 {
		t.Errorf("%s, the link's target, is %04o, want the 0701 it had", outside, info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(moved, "settings.conf")); err != nil {
		t.Errorf("the tree box wrote is not where it was moved to: %v", err)
	}
}

// A directory that is a link out of the directory holding it is not a tree: opened,
// it would be whatever the link names, which is the one thing this package exists to
// refuse.
func TestOpenRefusesALinkOutOfTheDirectoryHoldingIt(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "app")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(base, "app")); err == nil {
		t.Fatalf("%s is a link to %s and was opened as a tree", filepath.Join(base, "app"), outside)
	}
}

// Make gives the tree the mode it was asked for, whatever the process umask is, and
// a name that is already there is a refusal: box fills a tree it made.
func TestMakeSetsTheModeAndRefusesAnExistingName(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tree")
	tree, err := Make(dir, 0o700)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	defer tree.Close()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("%s is %04o, want 0700", dir, info.Mode().Perm())
	}
	if _, err := Make(dir, 0o700); err == nil {
		t.Errorf("%s was made twice", dir)
	}
}

// Give sets the mode it is asked for on the tree it opened, after the chown that
// would otherwise clear the bits it sets.
func TestGiveSetsTheModeOfTheTreeItOpened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tree")
	tree, err := Make(dir, 0o700)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	defer tree.Close()
	if err := tree.Give(os.Geteuid(), os.Getegid(), 0o751); err != nil {
		t.Fatalf("Give: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Errorf("%s is %04o, want 0751", dir, info.Mode().Perm())
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Remove deletes a tree box made, through the handle on the directory it was made
// in, and refuses one it only opened: that is a seedbox user's config directory.
func TestRemoveDeletesOnlyATreeBoxMade(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "tree")
	made, err := Make(dir, 0o700)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if err := made.Root().WriteFile("settings.conf", []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	theirs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := theirs.Remove(); err == nil {
		t.Errorf("%s was removed from a tree box only opened", dir)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("%s is gone after a refused removal: %v", dir, err)
	}
	theirs.Close()

	if err := made.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is still there after Remove: %v", dir, err)
	}
	made.Close()
}
