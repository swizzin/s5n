package hostroot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyPath is one of box's files where box kept it before 0.5.
func legacyPath(name string) string { return filepath.Join(LegacyDir, name) }

// disk is a host with nothing on it but what a case puts there.
type disk map[string]bool

func (d disk) stat(path string) (bool, error) { return d[path], nil }

// staleFixture is the box a host had before 0.5: the record, root's config and
// root's grant, all under the old root.
func staleFixture(files ...string) disk {
	d := disk{}
	for _, f := range files {
		d[filepath.Join(LegacyDir, f)] = true
	}
	return d
}

func TestLookRefusesAFreshRootWithOldFiles(t *testing.T) {
	s, err := Look(RecordPath, SeedboxDir+"/root.yaml", staleFixture("cluster.yaml", "box.yaml", "seedboxes").stat)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{legacyPath("cluster.yaml"), legacyPath("box.yaml"), legacyPath("seedboxes")}; strings.Join(s.Move, ",") != strings.Join(want, ",") {
		t.Fatalf("Move = %v, want %v", s.Move, want)
	}
	if len(s.Shadowed) != 0 {
		t.Fatalf("a new root with nothing in it has nothing shadowed: %v", s.Shadowed)
	}
	refusal := s.Err()
	if refusal == nil {
		t.Fatal("the old files with none at the new root were not refused")
	}
	for _, want := range []string{
		"box keeps its files in /etc/s5n now",
		"mv /etc/swizzin/cluster.yaml /etc/swizzin/box.yaml /etc/swizzin/seedboxes /etc/s5n",
		"Check, then move them:",
	} {
		if !strings.Contains(refusal.Error(), want) {
			t.Fatalf("refusal does not say %q:\n%s", want, refusal)
		}
	}
	if got := strings.Join(s.Warnings(), ";"); got != "" {
		t.Fatalf("a refusal warned too: %q", got)
	}
}

// A bootstrap-only host has a record and no grant, and the move must not name a
// directory that does not exist: mv would fail halfway.
func TestLookNamesOnlyTheFilesItFound(t *testing.T) {
	s, err := Look(RecordPath, SeedboxDir+"/root.yaml", staleFixture("cluster.yaml").stat)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Move) != 1 || s.Move[0] != legacyPath("cluster.yaml") {
		t.Fatalf("Move = %v, want just the record", s.Move)
	}
	if strings.Contains(s.Err().Error(), "seedboxes") {
		t.Fatalf("the move names a file that is not there:\n%s", s.Err())
	}
}

// After the move the new root wins and the run proceeds; refusing there would
// lock out a box that did the right thing. The old copy is named once, because
// it is the file someone would go on editing.
func TestLookWarnsWhenBothRootsHaveTheFile(t *testing.T) {
	d := staleFixture("cluster.yaml", "box.yaml", "seedboxes")
	d[RecordPath] = true
	d[filepath.Join(Dir, ConfigName)] = true
	d[SeedboxDir] = true

	s, err := Look(RecordPath, SeedboxDir+"/root.yaml", d.stat)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Move) != 0 || s.Err() != nil {
		t.Fatalf("a moved record was refused: move=%v err=%v", s.Move, s.Err())
	}
	if want := []string{legacyPath("cluster.yaml"), legacyPath("box.yaml"), legacyPath("seedboxes")}; strings.Join(s.Shadowed, ",") != strings.Join(want, ",") {
		t.Fatalf("Shadowed = %v, want %v", s.Shadowed, want)
	}
	warnings := s.Warnings()
	if len(warnings) != 3 {
		t.Fatalf("Warnings = %v, want one per shadowed file", warnings)
	}
	if !strings.Contains(warnings[0], legacyPath("cluster.yaml")) || !strings.Contains(warnings[0], "stale") {
		t.Fatalf("warning does not name the stale file: %q", warnings[0])
	}
}

func TestLookOnAFreshHostIsSilent(t *testing.T) {
	s, err := Look(RecordPath, SeedboxDir+"/root.yaml", disk{}.stat)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Empty() || s.Err() != nil || len(s.Warnings()) != 0 {
		t.Fatalf("a fresh host was not silent: %+v", s)
	}
}

// --record names a file box reads instead of the default, so the default root's
// old files are not this run's: `box bootstrap --record /srv/e2e/cluster.yaml`
// must not refuse because the machine has an old box on it.
func TestLookIgnoresARecordSomewhereElse(t *testing.T) {
	s, err := Look("/srv/e2e/cluster.yaml", "/srv/e2e/root.yaml", staleFixture("cluster.yaml", "box.yaml", "seedboxes").stat)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Empty() {
		t.Fatalf("a run with its own paths looked at the default root: %+v", s)
	}
}

// box never follows a symlink when it walks a seedbox's files, so a link standing
// in for one of its own is not that file: statting through it would make a link
// in the old root look like a record box could move.
func TestLookDoesNotReadThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("decided: install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stat is given the link's own path; it answers for the link, not the target.
	if err := os.Symlink(target, filepath.Join(dir, "cluster.yaml")); err != nil {
		t.Fatal(err)
	}
	have, err := OSStat(filepath.Join(dir, "cluster.yaml"))
	if err != nil || !have {
		t.Fatalf("OSStat of a link = %v, %v, want it seen as there", have, err)
	}
	if _, err := OSStat(filepath.Join(dir, "missing.yaml")); err != nil {
		t.Fatalf("OSStat of a missing path = %v, want no error", err)
	}
}

// A directory box cannot look into is not "the old files are gone": deciding
// either way from a failed read turns a refusal into an install over a live box.
func TestLookReportsAStatThatFailed(t *testing.T) {
	failed := errors.New("permission denied")
	stat := func(path string) (bool, error) { return false, failed }
	_, err := Look(RecordPath, SeedboxDir+"/root.yaml", stat)
	if err == nil || !errors.Is(err, failed) {
		t.Fatalf("Look = %v, want the stat's error", err)
	}
	if !strings.Contains(err.Error(), LegacyDir) {
		t.Fatalf("the error does not name what could not be read: %v", err)
	}
}

// A second spelling of a path is the bug this package exists to prevent.
func TestPathsAreOneSet(t *testing.T) {
	if RecordPath != Dir+"/"+RecordName || SeedboxDir != Dir+"/"+SeedboxName {
		t.Fatalf("paths drifted: %q %q %q", RecordPath, SeedboxDir, Dir)
	}
	if LegacyDir == Dir {
		t.Fatal("the old root is the new one")
	}
}
