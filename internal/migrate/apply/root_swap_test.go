package apply

// root_swap_test.go proves what a run cannot be made to do through the two roots the
// seedbox's user can write: `.box`, where staging and the record live, and a library
// directory, are swapped for a link out of the root between the run's check and its
// act. Every step names its directory inside a handle box opened on the root, so the
// outside directory is untouched and the run refuses by name.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// readyToStep is a migration with its config root open, for a test that drives one
// step rather than a whole run.
func readyToStep(t *testing.T) (*migration, *fixture) {
	t.Helper()
	f := newFixture(t)
	p, raw := f.plan()
	m := &migration{
		p:        p,
		env:      f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{})),
		id:       PlanID(raw),
		st:       &State{Plan: PlanDigest(raw)},
		checks:   map[string]rewrite.Report{},
		deferred: map[string]bool{},
	}
	if err := m.open(); err != nil {
		t.Fatalf("open the config root: %v", err)
	}
	t.Cleanup(m.close)
	return m, f
}

// swapDotBox moves the run's own `.box` out of the config root and leaves a link to
// where it went: what the seedbox's user can do to a directory they own, and the only
// thing between root and a create, a write or a rename somewhere else.
func swapDotBox(t *testing.T, f *fixture) string {
	t.Helper()
	outside := t.TempDir()
	if err := os.Rename(filepath.Join(f.configRoot, ".box"), filepath.Join(outside, "box")); err != nil {
		t.Fatal(err)
	}
	// The link names the tree that was moved, so every name under it still resolves:
	// a run that works by path finds the staging copy there and acts on it.
	if err := os.Symlink(filepath.Join(outside, "box"), filepath.Join(f.configRoot, ".box")); err != nil {
		t.Fatal(err)
	}
	return outside
}

// The staging directory is made under `.box`, so a `.box` that is a link out of the
// config root is where the copy would land: nothing is created outside, and the run
// names the directory it refused.
func TestStageDoesNotWriteThroughADotBoxSwappedForALink(t *testing.T) {
	m, f := readyToStep(t)
	if err := os.MkdirAll(filepath.Join(f.configRoot, ".box"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := swapDotBox(t, f)

	err := m.stage(context.Background())
	if err == nil {
		t.Fatal("the run staged through a .box that is a link out of the config root")
	}
	if !strings.Contains(err.Error(), ".box") {
		t.Errorf("the refusal does not name the directory it would have written through:\n%v", err)
	}
	entries, rerr := os.ReadDir(outside)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if e.Name() != "box" {
			t.Errorf("the run made %s outside the config root", e.Name())
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "box", "migrate")); !os.IsNotExist(err) {
		t.Errorf("the run made a staging directory outside the config root: %v", err)
	}
}

// The move into place renames the staged tree to the instance's name: with `.box`
// swapped, the name it renames from is the link's tree, and root would carry that
// away from wherever the link points.
func TestMoveDoesNotMoveATreeThroughADotBoxSwappedForALink(t *testing.T) {
	m, f := readyToStep(t)
	if err := m.stage(context.Background()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	outside := swapDotBox(t, f)
	staged := filepath.Join(outside, "box", "migrate", m.id, "sonarr")
	if _, err := os.Lstat(staged); err != nil {
		t.Fatalf("the staged copy is not where the swap left it: %v", err)
	}

	err := m.move()
	if err == nil {
		t.Fatal("the run moved a tree out of a .box that is a link")
	}
	if !strings.Contains(err.Error(), ".box") {
		t.Errorf("the refusal does not name the directory it would have moved through:\n%v", err)
	}
	if _, err := os.Lstat(staged); err != nil {
		t.Errorf("the staged copy was moved out of the directory the link names: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.configRoot, "sonarr")); !os.IsNotExist(err) {
		t.Errorf("the instance's config directory was made from a tree outside the root: %v", err)
	}
}

// The record is written under `.box` too: a save through a swapped one writes a
// state file into whatever the link names, which is how root would leave a record of
// a migration in a directory the user chose.
func TestStateSaveDoesNotWriteThroughADotBoxSwappedForALink(t *testing.T) {
	m, f := readyToStep(t)
	if err := m.stage(context.Background()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	outside := swapDotBox(t, f)

	err := m.recordRenamed("Downloads")
	if err == nil {
		t.Fatal("the run saved its record through a .box that is a link")
	}
	if !strings.Contains(err.Error(), ".box") {
		t.Errorf("the refusal does not name the directory it would have written through:\n%v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "box", "migrate", m.id, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("the run wrote its state outside the config root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "box", "migrate", m.id, stateFileName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("the run left a temporary state file outside the config root: %v", err)
	}
}

// The library's directories are renamed inside the media directory, which the
// seedbox's user owns: swapped for a link, the rename would carry a directory out of
// whatever tree the link names.
func TestDataRenameDoesNotMoveThroughASwappedLibraryDirectory(t *testing.T) {
	f := mediaFixture(t)
	p, err := plan.Parse([]byte(f.doc))
	if err != nil {
		t.Fatalf("parse the plan with a library rename: %v", err)
	}
	m := &migration{
		p:      p,
		env:    f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{})),
		id:     PlanID([]byte(f.doc)),
		st:     &State{Plan: PlanDigest([]byte(f.doc))},
		checks: map[string]rewrite.Report{},
	}
	if err := m.open(); err != nil {
		t.Fatalf("open the config root: %v", err)
	}
	defer m.close()

	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "4K", "TV", "Show4k", "ep1.mkv"), "the library\n", 0o644)
	writeFile(t, filepath.Join(outside, "TV", "Show", "ep1.mkv"), "the library\n", 0o644)
	if err := os.RemoveAll(filepath.Join(f.dataRoot, "media")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.dataRoot, "media")); err != nil {
		t.Fatal(err)
	}

	err = m.renameData()
	if err == nil {
		t.Fatal("the run renamed a library directory out of a link")
	}
	if !strings.Contains(err.Error(), "media") {
		t.Errorf("the refusal does not name the directory it would have renamed through:\n%v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "TV", "Show", "ep1.mkv")); err != nil {
		t.Errorf("the library directory the link names was moved: %v", err)
	}
	for _, made := range []string{"tv", "tv-4k"} {
		if _, err := os.Lstat(filepath.Join(outside, made)); !os.IsNotExist(err) {
			t.Errorf("the run renamed a directory outside the data root, to %s: %v", made, err)
		}
	}
}

// A config root that is a link is not the root box init made: the run refuses it by
// name before it writes anything, however ordinary everything below it looks.
func TestRunRefusesAConfigRootThatIsALink(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	real := f.configRoot
	moved := f.root + "/config-real"
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), p, raw, f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{})))
	if err == nil {
		t.Fatal("a config root that is a link was taken as the root")
	}
	if !strings.Contains(err.Error(), real) || !strings.Contains(err.Error(), "link") {
		t.Errorf("the refusal does not name the root and why:\n%v", err)
	}
	if exists(filepath.Join(moved, ".box")) {
		t.Error("the refused run staged into the directory the link names")
	}
}

// The runner's exchange is files the pod reads and box writes, both under the config
// root: box opens that root itself, so a `.box` swapped for a link is a refusal
// before the Job, and the directory the link names is left alone.
func TestRunnerDoesNotWriteItsExchangeThroughASwappedDotBox(t *testing.T) {
	f := newRunnerFixture(t, "row|1|\n", "", 0)
	if err := os.MkdirAll(filepath.Join(f.root, ".box"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Rename(filepath.Join(f.root, ".box"), filepath.Join(outside, "box")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "box"), filepath.Join(f.root, ".box")); err != nil {
		t.Fatal(err)
	}

	_, err := f.runner.Run(context.Background(), "plex", []string{"/usr/lib/plexmediaserver/Plex SQLite", "/config/library.db"}, []byte("SELECT @tables;\n"))
	if err == nil {
		t.Fatal("the runner wrote its exchange through a .box that is a link")
	}
	if !strings.Contains(err.Error(), ".box") {
		t.Errorf("the refusal does not name the directory it would have written through:\n%v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "box", "migrate", f.planID)); !os.IsNotExist(err) {
		t.Errorf("the runner made its exchange outside the config root: %v", err)
	}
	if f.job != nil {
		t.Error("the run started a Job after it could not make its exchange")
	}
}
