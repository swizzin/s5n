package main

// migrate_carry_test.go is the read's one large decision: how much of Plex's
// config directory a snapshot takes. The tree is too large to copy twice on a
// small disk, so it travels only when there is room for it wherever the run
// writes it, and a run that leaves it behind says so with the numbers.

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// fakeDisk answers the room questions a read asks, by path: a test that needs a
// short disk names the paths that are short and how much they have.
type fakeDisk struct {
	free map[string]int64
	same map[string]bool
}

func (d fakeDisk) Free(path string) (int64, error) { return d.free[path], nil }

func (d fakeDisk) Same(a, b string) (bool, error) { return d.same[a+"\x00"+b], nil }

// carryRead is a read whose plan names Plex and Sonarr, at the paths the fixture
// tree is on.
func carryRead(t *testing.T) (discoverOptions, migrationRead) {
	t.Helper()
	root := t.TempDir()
	plexSourceTree(t, root)
	p := &plan.Plan{Version: plan.Version, Apps: map[string]plan.App{
		"plex":   {Kind: plan.Kubernetes, Source: "plex", Config: "/srv/apps/plex"},
		"sonarr": {Kind: plan.Kubernetes, Source: "sonarr", Config: "/srv/apps/sonarr"},
	}}
	return discoverOptions{Root: root, CarrySize: func(string, string) (int64, error) { return 40 << 30, nil }},
		migrationRead{plan: p}
}

// carryOf is the plan's answer for one app, and whether the read wrote one at
// all.
func carryOf(t *testing.T, read migrationRead, app string) string {
	t.Helper()
	got, ok := read.plan.Apps[app]
	if !ok {
		t.Fatalf("the plan has no %s", app)
	}
	return got.Carry
}

func TestCarryTakesTheWholeDataWhenThereIsRoom(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/final.tar.gz"
	o.Disk = fakeDisk{free: map[string]int64{"/snapshots": 60 << 30}}
	short, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal)
	if err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if len(short) != 0 {
		t.Errorf("a run with room said %v", short)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryFull {
		t.Errorf("the plan carries %q of Plex, want %q", got, plan.CarryFull)
	}
	if got := carryOf(t, read, "sonarr"); got != "" {
		t.Errorf("the plan says sonarr's carry is %q, and its config is its state", got)
	}
}

func TestCarryFallsBackToTheStateWhenTheSnapshotCannotHoldTheTree(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/final.tar.gz"
	o.Disk = fakeDisk{free: map[string]int64{"/snapshots": 12 << 30}}
	short, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal)
	if err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryMinimal {
		t.Errorf("the plan carries %q of Plex with 12 GiB free and 40 GiB to carry, want %q", got, plan.CarryMinimal)
	}
	if len(short) != 1 {
		t.Fatalf("the read said %v, want one line about Plex", short)
	}
	for _, want := range []string{"plex:", "needs 44.0 GiB", "12.0 GiB is free at /snapshots", "carried its state"} {
		if !strings.Contains(short[0], want) {
			t.Errorf("the line does not say %q: %s", want, short[0])
		}
	}
}

func TestCarryCountsBothCopiesOnTheHostThatIsBothEnds(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/final.tar.gz"
	o.TargetConfig = "/seedbox/config"
	o.Disk = fakeDisk{
		free: map[string]int64{"/snapshots": 60 << 30, "/seedbox/config": 60 << 30},
		same: map[string]bool{"/snapshots\x00/seedbox/config": true},
	}
	short, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal)
	if err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryMinimal {
		t.Errorf("the plan carries %q of Plex with room for one copy of two, want %q", got, plan.CarryMinimal)
	}
	if len(short) != 1 || !strings.Contains(short[0], "free at /snapshots") {
		t.Errorf("a run that carried the state for room said %v, want the numbers naming the one filesystem", short)
	}
	// With room for both, it travels: the same disk, twice the room.
	o.Disk = fakeDisk{
		free: map[string]int64{"/snapshots": 120 << 30, "/seedbox/config": 120 << 30},
		same: map[string]bool{"/snapshots\x00/seedbox/config": true},
	}
	read.plan.Apps["plex"] = plan.App{Kind: plan.Kubernetes, Source: "plex", Config: "/srv/apps/plex"}
	if _, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal); err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryFull {
		t.Errorf("the plan carries %q of Plex with room for both copies, want %q", got, plan.CarryFull)
	}
}

func TestCarryFallsBackWhenTheTargetCannotHoldTheCopy(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/final.tar.gz"
	o.TargetConfig = "/seedbox/config"
	o.Disk = fakeDisk{
		free: map[string]int64{"/snapshots": 60 << 30, "/seedbox/config": 4 << 30},
		same: map[string]bool{"/snapshots\x00/seedbox/config": false},
	}
	if _, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal); err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryMinimal {
		t.Errorf("the plan carries %q of Plex with 4 GiB at the config root and 40 GiB to carry, want %q", got, plan.CarryMinimal)
	}
}

func TestPlexMinimalIsTheOperatorsChoiceAndIsNeverSized(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/final.tar.gz"
	o.PlexMinimal = true
	sized := false
	o.CarrySize = func(string, string) (int64, error) {
		sized = true
		return 1, nil
	}
	o.Disk = fakeDisk{free: map[string]int64{"/snapshots": 1 << 40}}
	if _, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeFinal); err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if sized {
		t.Error("--plex-minimal sized a tree it will not carry")
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryMinimal {
		t.Errorf("the plan carries %q of Plex with --plex-minimal, want %q", got, plan.CarryMinimal)
	}
}

func TestASurveySettlesThePlanWithoutMeasuringAnything(t *testing.T) {
	o, read := carryRead(t)
	o.Out = "/snapshots/survey.tar.gz"
	o.CarrySize = func(string, string) (int64, error) {
		t.Error("a survey sized a tree it does not carry")
		return 0, nil
	}
	o.Disk = fakeDisk{free: map[string]int64{"/snapshots": 0}}
	if _, err := decideSnapshotCarry(&read, o, o.Out, archive.ModeSurvey); err != nil {
		t.Fatalf("decide the carry: %v", err)
	}
	if got := carryOf(t, read, "plex"); got != plan.CarryFull {
		t.Errorf("the plan carries %q of Plex, and a survey has not asked the disk", got)
	}
}

func TestTheDecisionFollowsThePlansCarryIntoTheSnapshot(t *testing.T) {
	if plan.CarryFull == plan.CarryMinimal {
		t.Fatal("the plan's two carries are one value")
	}
	// The space decision itself is internal/migrate/space's: this is the wiring
	// between the two, over the same answers.
	need := (40 << 30) + space.Margin(40<<30)
	if v := space.Choose(40<<30, []space.Place{{Path: "/snapshots", Free: need}}); !v.Fits() {
		t.Errorf("a disk with exactly the margin free did not fit: %+v", v)
	}
}
