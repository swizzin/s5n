package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// statusTest is one line's facts: a box that is current, on the timer, holding
// nothing. Each case changes one thing, so what a segment is for is visible.
func statusTest() statusFacts {
	last := time.Date(2026, 11, 15, 3, 12, 0, 0, time.Local)
	return statusFacts{
		Name:           "s5n",
		Installed:      "0.5.1",
		Timer:          true,
		TimerInstalled: true,
		Interval:       time.Hour,
		Now:            last.Add(5 * time.Minute),
		State: update.State{
			LastCheck: last,
			LastStep: &update.StepRecord{
				From: "0.5.0", To: "0.5.1", At: last, Outcome: update.OutcomeOK,
			},
		},
	}
}

// TestStatusLine pins every variant of the one line, against the exact strings
// docs/box.md writes down. The line is a contract -- the e2e greps it and a
// person reads it from a shell -- so a change to it is a test change.
func TestStatusLine(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*statusFacts)
		want  string
		notes string
	}{
		{
			name: "a current box whose next check is due",
			edit: func(f *statusFacts) { f.Now = f.State.LastCheck.Add(90 * time.Minute) },
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · next check within 15m",
		},
		{
			name: "the example from docs/box.md",
			edit: func(f *statusFacts) {},
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · next check ~04:12",
		},
		{
			name: "a box that has never stepped",
			edit: func(f *statusFacts) { f.State.LastStep = nil },
			want: "s5n 0.5.1 · last update none · next check ~04:12",
		},
		{
			name: "a step that failed",
			edit: func(f *statusFacts) {
				f.State.LastStep.Outcome = update.OutcomeFailed
				f.State.LastStep.Gate = "health"
			},
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 failed (health) · next check ~04:12",
		},
		{
			name: "a step that rolled an app back and passed",
			edit: func(f *statusFacts) { f.State.LastStep.RolledBack = []string{"radarr"} },
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok, radarr rolled back · next check ~04:12",
		},
		{
			name: "the cluster cannot be read",
			edit: func(f *statusFacts) { f.Unreachable = true },
			want: "s5n ? (cluster not reachable) · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · next check ~04:12",
		},
		{
			name: "no release is deployed",
			edit: func(f *statusFacts) { f.Installed = "" },
			want: "s5n ? (no release is deployed) · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · next check ~04:12",
		},
		{
			name: "the timer is off in root's config",
			edit: func(f *statusFacts) { f.Timer = false },
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · timer off",
		},
		{
			name: "the timer's unit is not on this host",
			edit: func(f *statusFacts) { f.TimerInstalled = false },
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · timer not installed",
		},
		{
			name: "everything held at once",
			edit: func(f *statusFacts) {
				f.HoldBox = true
				f.State.Failed = []string{"0.5.2"}
				f.Holds = []snapshot.Hold{{App: "radarr", Avoiding: snapshot.ImageRef{Repository: "ghcr.io/example/radarr", Tag: "6.3"}}}
				f.Pins = map[string]string{"sonarr": "4.0.14"}
			},
			want: "s5n 0.5.1 · last update 0.5.0 → 0.5.1 Nov 15 03:12 ok · next check ~04:12 · held: box, release 0.5.2 (failed), radarr (rolled back from 6.3), sonarr (pinned)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := statusTest()
			c.edit(&f)
			if got := statusLine(f); got != c.want {
				t.Fatalf("statusLine:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestStatusNextCheckIsLocalTime: the clock the line names is the operator's, not
// the record's UTC.
func TestStatusNextCheckIsLocalTime(t *testing.T) {
	f := statusTest()
	at := time.Date(2026, 11, 15, 3, 12, 0, 0, time.Local)
	f.State.LastCheck = at
	f.Now = at
	if got := statusNextCheck(f); got != "next check ~04:12" {
		t.Fatalf("next check = %q, want the local clock time", got)
	}
}

// TestValuePinsReadsOnlyWhatTheUserPinned: status names the apps the values hold,
// with the label the pin is, and says nothing about an app it merely enables.
func TestValuePins(t *testing.T) {
	pins := valuePins(loadStatusSeedbox(t, `apps:
  sonarr:
    enabled: true
    image:
      tag: "4.0.14"
  radarr:
    enabled: true
    image:
      digest: "sha256:abc"
  plex:
    enabled: true
`))
	if len(pins) != 2 || pins["sonarr"] != "4.0.14" || pins["radarr"] != "sha256:abc" {
		t.Fatalf("pins = %v", pins)
	}
	if _, ok := pins["plex"]; ok {
		t.Fatal("an app with no pin is named as pinned")
	}
}

// loadStatusSeedbox loads a seedbox whose grant is the minimum and whose values
// file is the document given: status reads pins from it and nothing else.
func loadStatusSeedbox(t *testing.T, values string) *seedbox.Seedbox {
	t.Helper()
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant.yaml")
	grantYAML := "namespace: testbox\nstorage:\n  data:\n    path: " + filepath.Join(dir, "data") +
		"\n  config:\n    path: " + filepath.Join(dir, "config") + "\n"
	if err := os.WriteFile(grant, []byte(grantYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesPath, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grant, valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestStatusHeldNamesItsReason: a held thing without a reason does not tell
// anyone what to do, so each kind carries its own words.
func TestStatusHeldNamesItsReason(t *testing.T) {
	f := statusTest()
	f.Holds = []snapshot.Hold{{App: "radarr", Avoiding: snapshot.ImageRef{Tag: "6.3"}}}
	f.Pins = map[string]string{"sonarr": "4.0.14", "autobrr": "1.2"}
	got := statusHeld(f)
	if !strings.Contains(got, "radarr (rolled back from 6.3)") {
		t.Fatalf("held = %q", got)
	}
	// Pins are named in app order, so two runs read the same.
	if strings.Index(got, "autobrr") > strings.Index(got, "sonarr") {
		t.Fatalf("pins are not in app order: %q", got)
	}
}

// TestStatusClusterReadIsBounded: the line is one line and someone runs it from
// a shell prompt, so its one cluster read cannot outlast it. A server that never
// answers is not an error -- it is "cluster not reachable", and the other three
// segments still print. A Ctrl-C is still the operator's, not an answer.
func TestStatusClusterReadIsBounded(t *testing.T) {
	t.Run("a server that never answers", func(t *testing.T) {
		var passed context.Context
		read := func(ctx context.Context) (release.ReleaseState, error) {
			passed = ctx
			<-ctx.Done()
			return release.ReleaseState{}, ctx.Err()
		}
		start := time.Now()
		state, unreachable, err := readDeployed(context.Background(), 20*time.Millisecond, read)
		if err != nil {
			t.Fatalf("a read that ran out of time returned %v, want the line's answer", err)
		}
		if !unreachable {
			t.Fatal("a server that never answered was not cluster not reachable")
		}
		if state.ChartVersion != "" {
			t.Fatalf("installed = %q, want nothing", state.ChartVersion)
		}
		if deadline, ok := passed.Deadline(); !ok || deadline.After(start.Add(2*time.Second)) {
			t.Fatalf("the read got no deadline of its own: %v", passed)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("the read took %v: the deadline did not reach it", elapsed)
		}
	})

	t.Run("a release that answers", func(t *testing.T) {
		read := func(context.Context) (release.ReleaseState, error) {
			return release.ReleaseState{Exists: true, ChartVersion: "0.5.1"}, nil
		}
		state, unreachable, err := readDeployed(context.Background(), time.Second, read)
		if err != nil || unreachable {
			t.Fatalf("readDeployed = (%+v, %v, %v), want the release", state, unreachable, err)
		}
		if state.ChartVersion != "0.5.1" {
			t.Fatalf("installed = %q, want 0.5.1", state.ChartVersion)
		}
	})

	t.Run("the operator pressed Ctrl-C", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		read := func(ctx context.Context) (release.ReleaseState, error) {
			cancel()
			<-ctx.Done()
			return release.ReleaseState{}, ctx.Err()
		}
		if _, unreachable, err := readDeployed(ctx, time.Minute, read); !errors.Is(err, context.Canceled) {
			t.Fatalf("readDeployed = %v, want the cancellation", err)
		} else if unreachable {
			t.Fatal("a cancelled read was reported as an unreachable cluster")
		}
	})
}
