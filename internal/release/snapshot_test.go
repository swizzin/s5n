package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/snapshot"
)

// fakeSnapshots is the snapshot step for the tests that prove where in the
// apply it runs: it shares the fakes' event log with fakeRelease and
// fakeStorage, so one test can assert the whole order.
type fakeSnapshots struct {
	work       SnapshotWork
	dirs       SnapshotDirs
	pendingErr error
	takeErr    error
	pruneErr   error

	pendings []SnapshotPlan
	takes    []SnapshotPlan
	prunes   []SnapshotPlan
	events   *[]string
}

func (f *fakeSnapshots) Pending(_ context.Context, plan SnapshotPlan) (SnapshotWork, error) {
	f.log("snapshot pending")
	f.pendings = append(f.pendings, plan)
	return f.work, f.pendingErr
}

func (f *fakeSnapshots) Take(_ context.Context, plan SnapshotPlan, _ SnapshotWork) (SnapshotDirs, error) {
	f.log("snapshot")
	f.takes = append(f.takes, plan)
	if f.takeErr != nil {
		return nil, f.takeErr
	}
	return f.dirs, nil
}

func (f *fakeSnapshots) Prune(_ context.Context, plan SnapshotPlan, _ SnapshotWork) error {
	f.log("prune")
	f.prunes = append(f.prunes, plan)
	return f.pruneErr
}

func (f *fakeSnapshots) log(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

// movingSnapshots is the step's answer for an apply that moves sonarr: one app
// with state to copy, and the directory the copy goes in.
func (f *fakes) movingSnapshots() *fakeSnapshots {
	f.snaps = &fakeSnapshots{
		work: SnapshotWork{
			From: "0.4.0",
			To:   "0.4.0",
			Moves: []SnapshotMove{{
				App:     "sonarr",
				Ran:     snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0", Digest: "sha256:aaaa"},
				MovedTo: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: "sha256:bbbb"},
				Copied:  true,
			}},
		},
		dirs:   SnapshotDirs{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.4.0-20260102T030405Z/sonarr"},
		events: &f.events,
	}
	return f.snaps
}

// installed is one apply that has already installed the release, so the next
// one has a diff to make and a release to move.
func installed(t *testing.T) *fakes {
	t.Helper()
	f := newFakes(t)
	if _, err := f.apply(t); err != nil {
		t.Fatalf("first apply: %v\n%s", err, f.out)
	}
	f.events = nil
	f.out.Reset()
	return f
}

// assertSequence fails unless the events contain want in order.
func assertSequence(t *testing.T, events []string, want ...string) {
	t.Helper()
	at := 0
	for _, name := range want {
		found := -1
		for i := at; i < len(events); i++ {
			if events[i] == name {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("events = %v, want %q after %v", events, name, want[:len(want)-1])
		}
		at = found + 1
	}
}

// A values change that moves an image is copied before the release moves, and
// only after the operator said yes: the app is down for the copy, and a "no"
// must leave it running.
func TestApplyCopiesAMovingAppBeforeTheReleaseMoves(t *testing.T) {
	f := installed(t)
	snaps := f.movingSnapshots()
	f.app.Confirm = confirmFunc(func(string) (bool, error) {
		f.events = append(f.events, "confirm")
		return true, nil
	})
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	assertSequence(t, f.events, "confirm", "snapshot", "helm", "prune")
	if len(snaps.pendings) != 1 || len(snaps.takes) != 1 || len(snaps.prunes) != 1 {
		t.Fatalf("pending/take/prune calls = %d/%d/%d, want 1/1/1",
			len(snaps.pendings), len(snaps.takes), len(snaps.prunes))
	}
	// The step is given the release being installed, the values this apply
	// renders and the grant's config root -- the same inputs the plan had.
	plan := snaps.takes[0]
	if plan.Chart.Version == "" || plan.Namespace != "testbox" || plan.ConfigRoot != f.configPath {
		t.Fatalf("take plan = %+v, want the chart, testbox and %s", plan, f.configPath)
	}
	if got := plan.Values["wiring"].(map[string]any)["timeoutSeconds"]; got != 600 {
		t.Fatalf("take values wiring.timeoutSeconds = %v, want 600: the final values", got)
	}
	out := f.out.String()
	for _, want := range []string{
		"snapshots:\n  sonarr: stopped and snapshotted before it moves from 4.9.0 to 5.0.0\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
}

// An app the catalog gives nothing to copy moves without one, and the plan says
// so rather than leaving it out: it is never stopped, and box never rolls it
// back.
func TestApplyPlansAnUncopiedMoveWithoutStoppingIt(t *testing.T) {
	f := installed(t)
	f.snaps = &fakeSnapshots{
		work: SnapshotWork{From: "0.4.0", To: "0.4.0", Moves: []SnapshotMove{{
			App:     "qui",
			Ran:     snapshot.ImageRef{Repository: "registry.example.com/seedbox/qui", Tag: "1.0.0"},
			MovedTo: snapshot.ImageRef{Repository: "registry.example.com/seedbox/qui", Tag: "1.1.0"},
		}}},
		dirs:   SnapshotDirs{},
		events: &f.events,
	}
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.snaps.takes) != 0 {
		t.Fatalf("take called for an app with nothing to copy: %+v", f.snaps.takes)
	}
	want := "snapshots:\n  qui: moves from 1.0.0 to 1.1.0 without a snapshot (nothing to copy)\n"
	if !strings.Contains(f.out.String(), want) {
		t.Fatalf("output does not contain %q:\n%s", want, f.out)
	}
}

// A values change that moves no image prints nothing new, and a run with no
// change at all still says "nothing to change" without reading the cluster's
// apps.
func TestApplyWithoutAMovingImagePlansNoSnapshot(t *testing.T) {
	f := installed(t)
	f.snaps = &fakeSnapshots{events: &f.events}
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if strings.Contains(f.out.String(), "snapshots:") {
		t.Fatalf("a run that moves no image printed a snapshot plan:\n%s", f.out)
	}
	if len(f.snaps.takes) != 0 || len(f.snaps.prunes) != 0 {
		t.Fatalf("took or pruned with nothing moving: takes=%d prunes=%d", len(f.snaps.takes), len(f.snaps.prunes))
	}
	if f.rel.upgrades != 2 {
		t.Fatalf("upgrades = %d, want the changed values applied", f.rel.upgrades)
	}

	// The installed release again: nothing differs, so there is no question to
	// ask and no reason to read the apps.
	f.snaps = &fakeSnapshots{events: &f.events}
	noop, err := f.apply(t)
	if err != nil {
		t.Fatalf("no-op apply: %v\n%s", err, f.out)
	}
	if noop.Applied || !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("the unchanged apply was not a no-op: %+v\n%s", noop, f.out)
	}
	if len(f.snaps.pendings) != 0 {
		t.Fatalf("the no-op apply read the cluster's apps: %+v", f.snaps.pendings)
	}
}

// A first install has nothing to migrate: no release is deployed, so no app's
// image can have moved and nothing is read.
func TestFirstInstallTakesNoSnapshot(t *testing.T) {
	f := newFakes(t)
	f.movingSnapshots()
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.snaps.pendings) != 0 {
		t.Fatalf("a first install asked for a snapshot plane: %+v", f.snaps.pendings)
	}
	if strings.Contains(f.out.String(), "snapshots:") {
		t.Fatalf("a first install printed a snapshot plan:\n%s", f.out)
	}
}

// Saying no writes nothing, so the app is not stopped for a copy nobody asked
// for.
func TestDeclinedApplyNeverCopies(t *testing.T) {
	f := installed(t)
	f.movingSnapshots()
	f.app.Confirm = confirmFunc(func(string) (bool, error) { return false, nil })
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	_, err := f.apply(t)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("apply = %v, want a cancelled apply", err)
	}
	if len(f.snaps.takes) != 0 {
		t.Fatalf("a declined apply copied: %+v", f.snaps.takes)
	}
}

// A copy box cannot take refuses the run before the question, so an app is
// never stopped for a snapshot that would fail.
func TestARefusedSnapshotStopsTheApplyBeforeTheQuestion(t *testing.T) {
	f := installed(t)
	f.snaps = &fakeSnapshots{pendingErr: errors.New("sonarr: its config directory is not there"), events: &f.events}
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")
	asked := 0
	f.app.Confirm = confirmFunc(func(string) (bool, error) {
		asked++
		return true, nil
	})

	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "config directory is not there") {
		t.Fatalf("apply = %v, want the refusal", err)
	}
	if asked != 0 {
		t.Fatalf("the refusal asked the question %d time(s)", asked)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want only the install", f.rel.upgrades)
	}
}

// A release that fails after the copy leaves the apps down: the error says
// where the state they had is, and the one command that puts it back.
func TestAFailedUpgradeSaysWhereTheSnapshotIs(t *testing.T) {
	f := installed(t)
	snaps := f.movingSnapshots()
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")
	f.rel.upgradeErr = errors.New("helm: timed out waiting for the condition")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although Helm refused the upgrade")
	}
	want := "the snapshot of sonarr is in " + snaps.dirs["sonarr"] + "; `box rollback sonarr` puts it back"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error does not say where the copy is:\n%v", err)
	}

	// The copy is the newest one per app, so a prune after a failed release
	// never runs: there is nothing newer than it.
	if len(snaps.prunes) != 0 {
		t.Fatalf("a failed apply pruned: %+v", snaps.prunes)
	}
}

// A copy that cannot be pruned costs disk, not the apply: the run still ends
// with the release installed and a warning.
func TestAPruneFailureIsAWarning(t *testing.T) {
	f := installed(t)
	f.movingSnapshots()
	f.snaps.pruneErr = errors.New("read-only file system")
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "keeping the snapshot of sonarr: read-only file system") {
		t.Fatalf("output does not warn about the kept snapshot:\n%s", f.out)
	}
}
