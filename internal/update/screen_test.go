package update

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

// TestTheUpdateScreenIsTheReference is the layout every command is being
// converted to, produced by the flow that owns it: one heading, then one row
// per thing that happened, the failure and what box did about it, the wiring
// folded to a count, and the hold the operator has to know about with the one
// command that shows it. This is the screen docs/output.md points at.
func TestTheUpdateScreenIsTheReference(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.6.0", "0.4.0", "0.4.0", digestB)
	h.rel.state.ChartVersion = "0.5.0"
	// One app moves and its new image keeps exiting; box puts it back.
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingUntil = 1
	h.snaps.taken = Taken{
		Step: "0.5.0-0.6.0-20260102T030405Z",
		At:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/snapshots/sonarr"},
	}
	// The wiring receipt: every check passed, thirty-four of them.
	rec := release.Receipt{Status: "ok", StartedAt: h.now}
	for i := 0; i < 34; i++ {
		rec.Steps = append(rec.Steps, release.Step{Name: fmt.Sprintf("check%d", i), Result: "ok"})
	}
	h.app.receipt = rec
	// The clock reads in the order the flow reads it -- the step's start, the
	// copy's start, the copy's finish, the step's finish -- so the durations
	// in the screen are the ones written down: six seconds to copy, forty-one
	// for the step.
	reads := []time.Duration{0, 0, 6 * time.Second, 41 * time.Second}
	read := 0
	clock := func() time.Time {
		at := h.now.Add(reads[read])
		if read < len(reads)-1 {
			read++
		}
		return at
	}
	h.u.Say = &say.Plain{W: h.out, Now: clock}

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	snapshotAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).In(time.Local).Format("15:04")
	want := strings.Join([]string{
		"s5n 0.5.0 → 0.6.0 · 1 step · sonarr moves",
		"",
		"✓ snapshots      sonarr · 6s",
		"✓ 0.5.0 → 0.6.0  41s",
		"✗ sonarr         crashed 3 times on the new image",
		"✓ sonarr         rolled back to 5.0.0 · its " + snapshotAt + " snapshot",
		"✓ wiring         34 checks",
		"! sonarr is held on 5.0.0 until a release ships a fixed image",
		"  $ box status",
		"",
	}, "\n")
	if got := h.out.String(); got != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// TestAFailedStepLeavesOneCrossRow: a step that stops says which step stopped,
// once, as a row -- and then whatever it could learn about where the release is
// is the one error line the command prints.
func TestAFailedStepLeavesOneCrossRow(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errSeq = map[string][]error{"0.5.0": {fmt.Errorf("upgrade failed: timed out")}}

	if _, err := h.run(t, false); err == nil {
		t.Fatal("a failed apply reported success")
	}
	out := h.out.String()
	if n := strings.Count(out, "✗"); n != 1 {
		t.Fatalf("the failure is said %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "✗ 0.4.0 → 0.5.0") {
		t.Fatalf("the ✗ row does not name the step:\n%s", out)
	}
}

// TestACurrentBoxIsOneNote: nothing to do is one line, and it is a note: a box
// that is current has no rows to read.
func TestACurrentBoxIsOneNote(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !res.Current {
		t.Fatalf("result = %+v, want current", res)
	}
	if got, want := h.out.String(), "· s5n 0.4.0 is current\n"; got != want {
		t.Fatalf("screen = %q, want %q", got, want)
	}
}
