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
	"github.com/swizzin/s5n/internal/setup"
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
	if got, want := strings.TrimSpace(out), "· updates        off       /etc/s5n/box.yaml says update.timer: off"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if body.updates != 0 || body.healths != 0 {
		t.Fatalf("a switched-off timer ran %d updates and %d health reads", body.updates, body.healths)
	}
	if state.saves != 0 {
		t.Fatalf("a switched-off timer wrote update.json %d times", state.saves)
	}
}

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

func TestScheduleUpdatesSaysCronWhenThereIsNowhereToSchedule(t *testing.T) {
	var buf bytes.Buffer
	rows := scheduleUpdates(context.Background(), "", "c", "g", "v", &say.Plain{W: &buf})
	if len(rows) != 1 {
		t.Fatalf("an unschedulable host left %d rows, want one: %+v", len(rows), rows)
	}
	if got, want := rows[0].Command, "box update --timer"; got != want {
		t.Fatalf("the follow-up's command = %q, want %q", got, want)
	}
	var screen bytes.Buffer
	(&say.Plain{W: &screen}).ToFinish(rows)
	want := "! to finish\n  updates  run this from cron every 15 minutes\n           $ box update --timer\n"
	if screen.String() != want {
		t.Fatalf("the block = %q, want %q", screen.String(), want)
	}
}

func TestNoDNSFollowUpNamesTheRecordsAndTheCommand(t *testing.T) {
	hosts := []setup.Host{
		{Host: "box.example.test", Issuer: setup.IssuerSelfSigned, NoDNS: true},
		{Host: "plex.box.example.test", Issuer: setup.IssuerSelfSigned, NoDNS: true},
		{Host: "sonarr.box.example.test", Issuer: setup.IssuerLetsEncrypt},
	}
	row, ok := noDNSFollowUp(hosts)
	if !ok {
		t.Fatal("two names with no record left no follow-up")
	}
	if got, want := strings.Join(row.Facts, " · "), "add a DNS record for box.example.test, plex.box.example.test"; got != want {
		t.Fatalf("the row says %q, want %q", got, want)
	}
	if got, want := row.Command, "box init"; got != want {
		t.Fatalf("the row's command = %q, want %q", got, want)
	}
	if _, ok := noDNSFollowUp([]setup.Host{{Host: "box.example.test", Issuer: setup.IssuerLetsEncrypt}}); ok {
		t.Fatal("a name whose record exists left a follow-up")
	}
}
