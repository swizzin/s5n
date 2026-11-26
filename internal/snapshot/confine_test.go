package snapshot

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// confined is the fixture the confinement tests share: a config root with one app's
// config directory under it, and a sentinel directory beside the root standing for
// anything on the host that is not the seedbox's. Every test here plants a link at one
// of the places box touches and checks that the call fails and the sentinel is exactly
// as it was.
type confined struct {
	root     string // the config root
	app      string // <root>/sonarr, one app's config directory
	sentinel string // beside the root, outside it
	step     string // <root>/.box/snapshots/<from>-<to>-<time>
	dest     string // <step>/sonarr
}

func newConfined(t *testing.T) *confined {
	t.Helper()
	base := t.TempDir()
	c := &confined{
		root:     filepath.Join(base, "config"),
		sentinel: filepath.Join(base, "sentinel"),
	}
	c.app = filepath.Join(c.root, "sonarr")
	c.step = filepath.Join(c.root, boxDir, "snapshots", "0.4.0-0.5.0-20260304T050607Z")
	c.dest = filepath.Join(c.step, "sonarr")
	for _, dir := range []string{c.app, c.sentinel, filepath.Join(c.sentinel, "sub")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(c.app, "app.db"), "the database", 0o640)
	write(t, filepath.Join(c.app, "config.xml"), "<Config/>", 0o640)
	write(t, filepath.Join(c.sentinel, "secret.txt"), "not box's to write", 0o600)
	return c
}

// shape is what a directory holds, path by path, so two equal shapes mean box
// left nothing behind: no new file, no rewritten content.
func shape(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[rel] = "dir " + d.Type().String()
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link -> " + target
		default:
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file %v %q", d.Type(), body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("shape of %s: %v", dir, err)
	}
	return out
}

// mustBeUnchanged fails the test if the sentinel's shape is not the one recorded.
func (c *confined) mustBeUnchanged(t *testing.T, before map[string]string) {
	t.Helper()
	if after := shape(t, c.sentinel); !reflect.DeepEqual(before, after) {
		t.Fatalf("the sentinel changed: box reached outside the config root\nbefore: %v\nafter:  %v", before, after)
	}
}

// .box is the seedbox's to replace, and a link there is the widest hole there is:
// the snapshot tree and the hold file both hang off it.
func TestTakeAndHoldsRefuseALinkedBoxDirectory(t *testing.T) {
	c := newConfined(t)
	if err := os.Symlink(c.sentinel, filepath.Join(c.root, boxDir)); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, err := Take(c.app, globs(), c.dest, images()); err == nil {
		t.Fatal("the snapshot was taken through a symlinked .box")
	}
	c.mustBeUnchanged(t, before)

	if err := SaveHolds(c.root, []Hold{holdFor("sonarr", heldAt)}); err == nil {
		t.Fatal("the holds were written through a symlinked .box")
	}
	c.mustBeUnchanged(t, before)
}

// .box is real and .box/snapshots is a link out, so every step directory a take
// creates would be created outside the tree.
func TestTakeRefusesALinkedSnapshotsRoot(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(filepath.Join(c.root, boxDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, Root(c.root)); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, err := Take(c.app, globs(), c.dest, images()); err == nil {
		t.Fatal("the snapshot was taken through a symlinked snapshots root")
	}
	c.mustBeUnchanged(t, before)
}

// A read through the link would find another tree's snapshots and restore them
// over this app's state; a prune through it would delete another tree's copies.
func TestNewestAndPruneRefuseALinkedSnapshotsRoot(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(filepath.Join(c.root, boxDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, Root(c.root)); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, _, err := Newest(Root(c.root), "sonarr"); err == nil {
		t.Fatal("the newest snapshot was read through a symlinked snapshots root")
	}
	c.mustBeUnchanged(t, before)

	if err := Prune(Root(c.root), "sonarr", 1); err == nil {
		t.Fatal("the snapshots were pruned through a symlinked snapshots root")
	}
	c.mustBeUnchanged(t, before)
}

// A step holds one copy of every app that moved, and a link there sends both
// halves of the step outside the tree.
func TestNewestAndTakeRefuseALinkedStepDirectory(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(Root(c.root), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, c.step); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, _, err := Newest(Root(c.root), "sonarr"); err == nil {
		t.Fatal("a snapshot was read through a symlinked step directory")
	}
	c.mustBeUnchanged(t, before)

	if _, err := Take(c.app, globs(), c.dest, images()); err == nil {
		t.Fatal("a snapshot was taken into a symlinked step directory")
	}
	c.mustBeUnchanged(t, before)
}

// The copy's own directory is a link out. Take refuses because a snapshot is
// already there by that name, and what the link points at is not touched either.
func TestTakeRefusesALinkedSnapshotDirectory(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(c.step, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, c.dest); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, err := Take(c.app, globs(), c.dest, images()); err == nil {
		t.Fatal("a snapshot was taken over a symlinked snapshot directory")
	}
	c.mustBeUnchanged(t, before)
}

// The copy a rollback would put back is a link out of the tree, and what it points
// at holds a complete snapshot: without the tree, the restore would read it,
// verify it and write it over the app's state.
func TestRestoreRefusesALinkedSnapshotDirectory(t *testing.T) {
	c := newConfined(t)
	if _, err := Take(c.app, globs(), c.dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	// The snapshot a link would reach: complete, at the sentinel, where box
	// would never have put it.
	stolen := filepath.Join(c.sentinel, "sonarr")
	if err := os.Rename(c.dest, stolen); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stolen, c.dest); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)
	// What the failed update left, so a restore that got as far as removing or
	// writing is visible.
	write(t, filepath.Join(c.app, "app.db"), "the migrated database", 0o600)
	write(t, filepath.Join(c.app, "app.db-shm"), "the new shm", 0o600)

	err := Restore(c.app, globs(), c.dest)
	if err == nil {
		t.Fatal("a snapshot was restored from a symlinked snapshot directory")
	}
	c.mustBeUnchanged(t, before)
	body, readErr := os.ReadFile(filepath.Join(c.app, "app.db"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "the migrated database" {
		t.Fatalf("app.db = %q, want the failed update's copy, untouched", body)
	}
	if _, err := os.Lstat(filepath.Join(c.app, "app.db-shm")); err != nil {
		t.Fatalf("the refusal removed a file first: %v", err)
	}
}

// A directory inside the app's config directory is the app's own to replace, and
// the walk does not follow it: a snapshot that skipped everything under it would
// restore the app to a state it was never in.
func TestTakeRefusesALinkedDirectoryUnderTheApp(t *testing.T) {
	c := newConfined(t)
	write(t, filepath.Join(c.app, "db", "db.sqlite3"), "the nested database", 0o640)
	if err := os.RemoveAll(filepath.Join(c.app, "db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, filepath.Join(c.app, "db")); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	_, err := Take(c.app, []string{"db/*.db"}, c.dest, images())
	if err == nil {
		t.Fatal("a snapshot was taken through a symlinked directory inside the app")
	}
	if want := "is a symlink and a glob names files under it"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want the readable refusal (%s)", err, want)
	}
	c.mustBeUnchanged(t, before)
}

// The app's config directory itself is a link out. Its owner is what a snapshot
// is handed to, so a take that followed it would copy another tree's files and
// hand them to this seedbox's user.
func TestTakeRefusesALinkedAppDirectory(t *testing.T) {
	c := newConfined(t)
	if err := os.RemoveAll(c.app); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, c.app); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, err := Take(c.app, globs(), c.dest, images()); err == nil {
		t.Fatal("a snapshot was taken from a symlinked app directory")
	}
	c.mustBeUnchanged(t, before)
}

// The hold file is box's own, and a link there points the record at another tree:
// reading it would take another box's holds as this box's.
func TestHoldsRefuseALinkedHoldFile(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(filepath.Join(c.root, boxDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.sentinel, HoldsPath(c.root)); err != nil {
		t.Fatal(err)
	}
	before := shape(t, c.sentinel)

	if _, err := LoadHolds(c.root); err == nil {
		t.Fatal("the holds were read through a symlink")
	}
	c.mustBeUnchanged(t, before)

	err := SaveHolds(c.root, []Hold{holdFor("sonarr", heldAt)})
	if err == nil {
		t.Fatal("the holds were written over a symlink")
	}
	if want := "it is a symlink"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want the readable refusal (%s)", err, want)
	}
	c.mustBeUnchanged(t, before)
}

// A link that stays inside the tree is the seedbox's own business and every operation
// keeps working through it.
//
// It has to be relative: os.Root refuses an absolute link wherever it points, the one
// behaviour the root cannot keep (TestAnAbsoluteLinkInsideTheRootIsRefused).
func TestALinkInsideTheConfigRootStillWorks(t *testing.T) {
	c := newConfined(t)
	inside := filepath.Join(c.root, "box-tree")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("box-tree", filepath.Join(c.root, boxDir)); err != nil {
		t.Fatal(err)
	}

	if err := SaveHolds(c.root, []Hold{holdFor("sonarr", heldAt)}); err != nil {
		t.Fatalf("save through a link inside the root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(inside, HoldsName)); err != nil {
		t.Fatalf("the hold file was not written where the link points: %v", err)
	}
	holds, err := LoadHolds(c.root)
	if err != nil || len(holds) != 1 || holds[0].App != "sonarr" {
		t.Fatalf("load through a link inside the root: %v %+v", err, holds)
	}

	if _, err := Take(c.app, globs(), c.dest, images()); err != nil {
		t.Fatalf("take through a link inside the root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(inside, "snapshots", filepath.Base(c.step), "sonarr", "app.db")); err != nil {
		t.Fatalf("the snapshot was not written where the link points: %v", err)
	}
}

// os.Root will not resolve an absolute link even when it points inside the tree,
// so a seedbox whose .box is one is refused where box used to follow it. Box never
// creates such a link itself.
func TestAnAbsoluteLinkInsideTheRootIsRefused(t *testing.T) {
	c := newConfined(t)
	if err := os.MkdirAll(filepath.Join(c.root, "box-tree"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.root, "box-tree"), filepath.Join(c.root, boxDir)); err != nil {
		t.Fatal(err)
	}

	err := SaveHolds(c.root, []Hold{holdFor("sonarr", heldAt)})
	if err == nil {
		t.Fatal("an absolute link inside the root was followed")
	}
	if _, statErr := os.Lstat(filepath.Join(c.root, "box-tree", HoldsName)); !os.IsNotExist(statErr) {
		t.Fatalf("the hold file was written through the link anyway: %v", statErr)
	}
}

// A step box cannot open is not a step without this app's copy: Prune would report
// success having deleted nothing, and Newest would answer "there is no snapshot"
// about a disk it never read.
func TestPruneAndNewestReportAStepTheyCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	c := newConfined(t)
	if _, err := Take(c.app, globs(), c.dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := os.Chmod(c.step, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.step, 0o750) })

	if _, _, err := Newest(Root(c.root), "sonarr"); err == nil {
		t.Fatal("a step that cannot be read was reported as no snapshot")
	}
	if err := Prune(Root(c.root), "sonarr", 1); err == nil {
		t.Fatal("a step that cannot be read was skipped")
	}
}

// The tree's edge: the copy a take writes, or a restore reads, must sit inside the
// config root holding the app's config directory, or it is a tree the seedbox's
// links could leave.
func TestTakeAndRestoreRefuseACopyOutsideTheConfigRoot(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, "app.db"), "state", 0o600)
	outside := t.TempDir()

	_, err := Take(app, []string{"*.db"}, filepath.Join(outside, "copy"), images())
	if err == nil || !strings.Contains(err.Error(), "is not inside the config root") {
		t.Fatalf("take error = %v, want a refusal naming the config root", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("take wrote %d entries outside the config root", len(entries))
	}
	err = Restore(app, []string{"*.db"}, filepath.Join(outside, "copy"))
	if err == nil || !strings.Contains(err.Error(), "is not inside the config root") {
		t.Fatalf("restore error = %v, want a refusal naming the config root", err)
	}
}
