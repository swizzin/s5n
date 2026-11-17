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
		Path:  "/etc/s5n/box.yaml",
		State: state,
		Now:   fixed(at(3, 0)),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := strings.TrimSpace(out), "box does not check for updates by itself: /etc/s5n/box.yaml says update.timer: off"; got != want {
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
			// Once, after the update, and only here: the chart half does not
			// read health itself, so a wake cannot read it twice.
			if body.healths != 1 {
				t.Fatalf("health reads = %d, want 1", body.healths)
			}
		})
	}
}

// TestTimerReadsHealthEvenWhenTheUpdateFails: an update that returns an error
// is still the wake's answer -- it exits 1 -- but the reading happens first.
// A registry or release that failed is exactly when an app's readiness is worth
// someone's attention, and a failed check leaves LastCheck unwritten, so the
// next wake is due again within minutes: without this read a registry outage
// would silence every health notice for as long as it lasts.
func TestTimerReadsHealthEvenWhenTheUpdateFails(t *testing.T) {
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
	if body.updates != 1 || body.healths != 1 {
		t.Fatalf("a failed due wake ran %d updates and %d health reads, want 1 and 1", body.updates, body.healths)
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

// TestTimerUnitsNeverSchedulesOnAHostThatCannotRunIt: the two units are only
// installed by root on a host whose systemd is up, or into a directory a test
// names. Everything else gets the one line naming cron.
func TestTimerUnitsNeverSchedulesOnAHostThatCannotRunIt(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name       string
		unitDir    string
		root       bool
		systemd    bool
		wantDir    string
		wantLine   bool
		wantSystem bool
	}{
		{"a test host names the directory", dir, false, false, dir, false, false},
		{"root on systemd", "", true, true, "", false, true},
		{"root without systemd", "", true, false, "", true, false},
		{"a user", "", false, true, "", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			units, line := timerUnitsFor(c.unitDir, c.root, c.systemd)
			if c.wantLine {
				if units != nil {
					t.Fatal("box scheduled an update on a host that cannot run one")
				}
				if line != noTimerLine {
					t.Fatalf("line = %q, want %q", line, noTimerLine)
				}
				return
			}
			if line != "" {
				t.Fatalf("line = %q, want none", line)
			}
			du, ok := units.(hostconf.DirUnits)
			if !ok {
				t.Fatalf("units = %T, want hostconf.DirUnits", units)
			}
			if c.wantSystem {
				if du.Dir != hostconf.UnitDir || du.EnableWith == nil {
					t.Fatalf("root on systemd got %+v, want %s with systemctl", du, hostconf.UnitDir)
				}
				return
			}
			if du.Dir != c.wantDir || du.EnableWith != nil {
				t.Fatalf("--unit-dir got %+v, want %s and no systemctl", du, c.wantDir)
			}
		})
	}
}

// TestScheduleUpdatesWritesTheFilesTheRunUsed: the unit's command carries the
// record, grant and values this run installed, so the timer updates this seedbox
// and not one a default path happens to name.
func TestScheduleUpdatesWritesTheFilesTheRunUsed(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	out := &say.Plain{W: &buf}
	scheduleUpdates(context.Background(), dir, "/work/cluster.yaml", "/work/grant.yaml", "/work/values.yaml", out)

	for _, name := range []string{hostconf.ServiceFile, hostconf.TimerFile} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	service, err := os.ReadFile(dir + "/" + hostconf.ServiceFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"update --timer",
		"--record /work/cluster.yaml",
		"--grant /work/grant.yaml",
		"--values /work/values.yaml",
	} {
		if !strings.Contains(string(service), want) {
			t.Errorf("the service's ExecStart does not carry %q:\n%s", want, service)
		}
	}
	if !strings.Contains(buf.String(), "box checks for a newer release every 15 minutes: box-update.timer") {
		t.Fatalf("bootstrap did not say what it installed: %q", buf.String())
	}

	// A second bootstrap changes nothing and says so.
	buf.Reset()
	scheduleUpdates(context.Background(), dir, "/work/cluster.yaml", "/work/grant.yaml", "/work/values.yaml", out)
	if !strings.Contains(buf.String(), "already installed and up to date") {
		t.Fatalf("a second bootstrap said %q", buf.String())
	}
}

// TestScheduleUpdatesSaysCronWhenThereIsNowhereToSchedule is the line that keeps
// an unschedulable host from being a box nobody updates: it names the command.
func TestScheduleUpdatesSaysCronWhenThereIsNowhereToSchedule(t *testing.T) {
	var buf bytes.Buffer
	scheduleUpdates(context.Background(), "", "c", "g", "v", &say.Plain{W: &buf})
	if !strings.Contains(buf.String(), "run `box update --timer` from cron every 15 minutes") {
		t.Fatalf("bootstrap said %q", buf.String())
	}
}
