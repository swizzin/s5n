package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/update"
)

// timerState is update.json for a timer test, without a file.
type timerState struct {
	state  update.State
	saves  int
	loadOK bool
	loadIn error
}

func (s *timerState) Load() (update.State, error) {
	if s.loadIn != nil {
		return update.State{}, s.loadIn
	}
	return s.state, nil
}

func (s *timerState) Save(state update.State) error {
	s.saves++
	s.state = state
	return nil
}

// timerBody is one wake of the timer with the two halves counted.
type timerBody struct {
	updates, healths int
	updateErr        error
}

func (b *timerBody) run(t *testing.T, tr timerRun) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	tr.Out = &say.Plain{W: &buf}
	tr.Update = func(context.Context) error {
		b.updates++
		return b.updateErr
	}
	tr.Health = func(context.Context) { b.healths++ }
	err := tr.run(context.Background())
	return buf.String(), err
}

func at(hour, minute int) time.Time {
	return time.Date(2026, 11, 15, hour, minute, 0, 0, time.UTC)
}

// fixed is a clock stopped at one moment.
func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestTimerOffWritesNothingAndSaysWhere: turning the timer off is an edit in
// root's file, and the wake that finds it says so without touching the registry,
// the cluster or update.json.
func TestTimerOffWritesNothingAndSaysWhere(t *testing.T) {
	state := &timerState{}
	body := &timerBody{}
	out, err := body.run(t, timerRun{
		Conf:  hostconf.Config{Timer: false, Interval: time.Hour},
		Path:  "/etc/swizzin/box.yaml",
		State: state,
		Now:   fixed(at(3, 0)),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := strings.TrimSpace(out), "box does not check for updates by itself: /etc/swizzin/box.yaml says update.timer: off"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if body.updates != 0 || body.healths != 0 {
		t.Fatalf("a switched-off timer ran %d updates and %d health reads", body.updates, body.healths)
	}
	if state.saves != 0 {
		t.Fatalf("a switched-off timer wrote update.json %d times", state.saves)
	}
}

// TestTimerNotDueReadsHealthOnly: the unit wakes every fifteen minutes and the
// interval says once an hour, so most wakes read health and nothing else.
func TestTimerNotDueReadsHealthOnly(t *testing.T) {
	state := &timerState{state: update.State{LastCheck: at(3, 0)}}
	body := &timerBody{}
	if _, err := body.run(t, timerRun{
		Conf:  hostconf.Config{Timer: true, Interval: time.Hour},
		Path:  "box.yaml",
		State: state,
		Now:   fixed(at(3, 15)),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if body.updates != 0 {
		t.Fatal("a wake that is not due ran the update")
	}
	if body.healths != 1 {
		t.Fatalf("health reads = %d, want 1", body.healths)
	}
	if !state.state.LastCheck.Equal(at(3, 0)) || state.saves != 0 {
		t.Fatalf("a wake that is not due moved LastCheck to %v (%d saves)", state.state.LastCheck, state.saves)
	}
}

// TestTimerDueUpdatesThenReadsHealth: the due wake is the update and then the
// reading, in that order, so the notice about an app is about the release it
// just moved to.
func TestTimerDueUpdatesThenReadsHealth(t *testing.T) {
	for _, c := range []struct {
		name  string
		last  time.Time
		now   time.Time
		dueAt time.Time
	}{
		{"never checked", time.Time{}, at(3, 0), at(3, 0)},
		{"an interval later", at(3, 12), at(4, 12), at(4, 12)},
		{"overdue by a day", at(3, 12), at(3, 12).Add(24 * time.Hour), at(3, 12).Add(24 * time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := &timerState{state: update.State{LastCheck: c.last}}
			body := &timerBody{}
			if _, err := body.run(t, timerRun{
				Conf:  hostconf.Config{Timer: true, Interval: time.Hour},
				Path:  "box.yaml",
				State: state,
				Now:   func() time.Time { return c.now },
			}); err != nil {
				t.Fatalf("run: %v", err)
			}
			if body.updates != 1 {
				t.Fatalf("updates = %d, want 1", body.updates)
			}
			// The update is the chart half, which reads health at its end; the
			// bare reading a not-due wake makes is not run again here.
			if body.healths != 0 {
				t.Fatalf("health reads outside the update = %d, want 0", body.healths)
			}
		})
	}
}

// TestTimerStopsWhenTheUpdateFails: an update that returns an error is the
// wake's answer, and the health read does not paper over it.
func TestTimerStopsWhenTheUpdateFails(t *testing.T) {
	state := &timerState{}
	body := &timerBody{updateErr: errors.New("the registry is down")}
	_, err := body.run(t, timerRun{
		Conf:  hostconf.Config{Timer: true, Interval: time.Hour},
		Path:  "box.yaml",
		State: state,
		Now:   fixed(at(3, 0)),
	})
	if err == nil || !strings.Contains(err.Error(), "the registry is down") {
		t.Fatalf("run = %v, want the update's error", err)
	}
	if body.healths != 0 {
		t.Fatal("a failed update still read health")
	}
}

// TestTimerRefusesAStateItCannotRead: a record box cannot read is not "never
// checked" -- guessing would re-check a release the last run already failed on.
func TestTimerRefusesAStateItCannotRead(t *testing.T) {
	body := &timerBody{}
	_, err := body.run(t, timerRun{
		Conf:  hostconf.Config{Timer: true, Interval: time.Hour},
		Path:  "box.yaml",
		State: &timerState{loadIn: errors.New("update.json: unknown field")},
		Now:   fixed(at(3, 0)),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("run = %v, want the record's error", err)
	}
	if body.updates != 0 || body.healths != 0 {
		t.Fatal("a run with an unreadable record updated or read health anyway")
	}
}

// TestTimerIsUnattended: the run nobody is watching answers yes and prints plain
// lines. It is the flag mapping on its own, because the rest of the run needs a
// cluster.
func TestTimerIsUnattended(t *testing.T) {
	for _, c := range []struct {
		name              string
		timer, yes, plain bool
		wantYes, wantPl   bool
	}{
		{"a hand run asks", false, false, false, false, false},
		{"--yes answers", false, true, false, true, false},
		{"--plain prints plain", false, false, true, false, true},
		{"--timer does both", true, false, false, true, true},
		{"--timer keeps what it was given", true, true, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("update", flag.ContinueOnError)
			o := addUpdateFlags(fs)
			if err := fs.Parse(nil); err != nil {
				t.Fatal(err)
			}
			*o.timer, *o.yes, *o.plain = c.timer, c.yes, c.plain
			yes, plain := o.unattended()
			if yes != c.wantYes || plain != c.wantPl {
				t.Fatalf("unattended() = (%v, %v), want (%v, %v)", yes, plain, c.wantYes, c.wantPl)
			}
		})
	}
}

// TestUpdateHoldsTheBoxWhenRootsConfigSaysSo: `update.hold-box: true` is the
// same switch as --hold-box, on every run, so a hand update and the timer's
// agree about the box's own version.
func TestUpdateHoldsTheBoxWhenRootsConfigSaysSo(t *testing.T) {
	conf, _, err := hostconf.Load(writeHostConf(t, "update:\n  hold-box: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	o := addUpdateFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if conf.HoldBox {
		// The line runUpdate runs: the config wins before the options are built.
		*o.holdBox = true
	}
	if !*o.holdBox {
		t.Fatal("update.hold-box: true did not hold the box")
	}
	if !o.selfUpdateOptions("").Hold {
		t.Fatal("the self-update did not get the hold for services it talks to")
	}
}

func writeHostConf(t *testing.T, data string) string {
	t.Helper()
	path := t.TempDir() + "/box.yaml"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
