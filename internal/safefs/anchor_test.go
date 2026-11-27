package safefs

// anchor_test.go pins the directory a run is opened on: a link at its name is not a
// root box works in, whatever it points at, and only a directory the seedbox's user or
// root owns can anchor root's work.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A link at the name is refused even when it stays inside the directory holding it:
// os.OpenRoot would follow it, and a name that decides what the root is has been
// decided by the user who owns that directory.
func TestOpenAnchorRefusesALinkAtTheName(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "config")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenAnchor(filepath.Join(base, "config"))
	if err == nil {
		t.Fatalf("%s, a link to %s, was opened as a root", filepath.Join(base, "config"), filepath.Join(base, "real"))
	}
	if !strings.Contains(err.Error(), filepath.Join(base, "config")) || !strings.Contains(err.Error(), "link") {
		t.Errorf("the refusal does not name the root and why: %v", err)
	}
}

// The config root box init made is a directory it opened, never the link a user put
// at its name: a swapped root would anchor every create, write and rename of the run.
func TestOpenOwnedRefusesAConfigRootThatIsALink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "config")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOwned(filepath.Join(base, "config"), os.Geteuid()); err == nil {
		t.Fatalf("%s, a link to %s, was taken as the config root", filepath.Join(base, "config"), outside)
	}
}

// A root someone else owns is not a root root's work is anchored on: only the
// seedbox's own user's, or root's.
func TestOpenOwnedRefusesADirectoryOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a directory to another user needs root")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, 4310, 4311); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOwned(dir, 4311); err == nil {
		t.Fatalf("%s, owned by 4310, was taken as the root for 4311", dir)
	} else if !strings.Contains(err.Error(), "4310") {
		t.Errorf("the refusal does not name the owner: %v", err)
	}
}

// The rule itself: the seedbox's user and root own a root box works in, and nobody
// else does.
func TestOwnsAnchorIsTheSeedboxUserOrRoot(t *testing.T) {
	uid := os.Geteuid()
	if !ownsAnchor(&syscall.Stat_t{Uid: uint32(uid)}, uid) {
		t.Errorf("the seedbox's own user %d was refused its root", uid)
	}
	if !ownsAnchor(&syscall.Stat_t{Uid: 0}, 4310) {
		t.Error("root, which box init leaves a root as, was refused")
	}
	if ownsAnchor(&syscall.Stat_t{Uid: 4310}, 4311) {
		t.Error("a third user's directory was accepted as the root for 4311")
	}
}
