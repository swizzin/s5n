package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/update"
)

// timerRun is `box update --timer`, which box-update.timer wakes. box decides
// what a wake is worth, so box.yaml is the only switch and systemd is untouched.
type timerRun struct {
	// Conf is root's parsed config.
	Conf hostconf.Config
	// Path lets a not-due line name the file.
	Path string
	// State is update.json: the last check and health.
	State update.StateStore
	// Now lets a test decide due without waiting.
	Now func() time.Time
	Out say.Reporter

	// Update runs only when a check is due: box, then charts.
	Update func(context.Context) error
	// Health is read after Update on a due wake whatever it returned, and alone
	// otherwise. A replaced box never returns here; the new binary reads its own.
	// Nil skips it.
	Health func(context.Context)
}

func (t timerRun) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t timerRun) report() say.Reporter { return say.Or(t.Out, nil) }

// run: off writes nothing; not due reads health only; due updates then reads it.
func (t timerRun) run(ctx context.Context) error {
	if !t.Conf.Timer {
		// One line and no work: the wake still shows the timer is off.
		t.report().Rows([]say.Row{{Mark: say.MarkNote, Name: "updates", State: "off", Facts: []string{t.Path + " says update.timer: off"}}})
		return nil
	}
	state, err := t.State.Load()
	if err != nil {
		return err
	}
	if state.Due(t.now(), t.Conf.Interval) {
		// Read health here, whatever Update returned: a failed check leaves
		// LastCheck unwritten, so without this a registry outage would silence
		// every health notice. The wake still exits 1.
		err := t.Update(ctx)
		if t.Health != nil {
			t.Health(ctx)
		}
		return err
	}
	if t.Health != nil {
		t.Health(ctx)
	}
	return nil
}

// unattended is --timer's answer: yes, with plain lines, because nobody watches.
func (o *updateFlags) unattended() (yes, plain bool) {
	if *o.timer {
		return true, true
	}
	return *o.yes, *o.plain
}

// noTimerLine names the cron command when box cannot schedule itself.
const noTimerLine = "no update timer on this host: run box update --timer from cron every 15 minutes"

// systemdRunning: /run/systemd/system exists only while systemd is up.
func systemdRunning() bool {
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}

// timerUnits is where bootstrap puts the timer files, or why it cannot.
//
// --unit-dir is the development switch: the files land in a directory this run
// names and no systemctl runs at all, because the systemd they are for is not
// this host's. Otherwise the timer is installed only by root, on a host whose
// systemd is running -- never by a test, and never by a non-root run.
func timerUnits(unitDir string) (hostconf.Units, string) {
	return timerUnitsFor(unitDir, os.Geteuid() == 0, systemdRunning())
}

// timerUnitsFor is timerUnits over the three facts, so a test covers the root and
// systemd cases on a host that is neither.
func timerUnitsFor(unitDir string, root, systemd bool) (hostconf.Units, string) {
	switch {
	case unitDir != "":
		return hostconf.DirUnits{Dir: unitDir}, ""
	case root && systemd:
		return hostconf.Systemd(), ""
	default:
		return nil, noTimerLine
	}
}

// boxPath is the binary the unit runs: this process's, with symlinks resolved,
// because a unit that runs a link runs whatever it points at next time.
func boxPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find box's binary to schedule: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

// scheduleUpdates writes the timer's units for the files this run installed, and
// says what it did. A failure is a follow-up row, not a failed bootstrap: the
// seedbox is installed and serving, and the row names the cron command that
// does the same job. The rows are the caller's to print with whatever else the
// run left a person.
func scheduleUpdates(ctx context.Context, unitDir, record, grant, values string, out say.Reporter) []say.Row {
	units, why := timerUnits(unitDir)
	if units == nil {
		out.Detail(why)
		return []say.Row{timerFollowUp(why)}
	}
	path, err := boxPath()
	if err != nil {
		out.Detail(noTimerLine + " (" + err.Error() + ")")
		return []say.Row{timerFollowUp(noTimerLine)}
	}
	changed, err := hostconf.Install(ctx, units, path, record, grant, values)
	if err != nil {
		out.Detail("box's update timer is not running: " + err.Error())
		return []say.Row{timerFollowUp("box's update timer is not running: " + err.Error())}
	}
	if changed {
		out.Muted(fmt.Sprintf("box checks for a newer release every %d minutes: %s, and update.timer in %s turns that off",
			hostconf.WakeInterval, hostconf.TimerFile, hostconf.Path(record)))
		return nil
	}
	out.Muted(fmt.Sprintf("%s is already installed and up to date", hostconf.TimerFile))
	return nil
}

// timerFollowUp is the row a host with no update timer gets: what is missing,
// and the command cron runs in its place. The words are the same whatever the
// reason -- root's run on a host with systemd is the only one that installs it
// -- and the reason itself goes to the log with the row.
func timerFollowUp(string) say.Row {
	return say.Row{
		Mark:    say.MarkNeeds,
		Name:    "updates",
		Facts:   []string{"run this from cron every 15 minutes"},
		Command: "box update --timer",
	}
}
