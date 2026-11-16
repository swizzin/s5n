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

// timerRun is `box update --timer`: the run systemd's box-update.timer makes
// every fifteen minutes.
//
// The unit wakes box on a fixed schedule and box decides what a wake is worth.
// That keeps root's box.yaml the only switch: turning the timer off, or making
// it check less often, is an edit in one file and takes effect on the next wake
// without touching systemd.
type timerRun struct {
	// Conf is root's config, already parsed: an error in it stopped the command
	// before this value was built.
	Conf hostconf.Config
	// Path is where that config lives, so the one line a switched-off or
	// not-due run says can name the file.
	Path string
	// State is update.json, which holds the last check and the last health.
	State update.StateStore
	// Now is the clock, so a test decides due or not due without waiting.
	Now func() time.Time
	Out say.Reporter

	// Update is the whole update: box's own replacement and then the charts. It
	// runs only when a check is due, so a wake between checks asks the registry
	// nothing and replaces nothing.
	Update func(context.Context) error
	// Health is the reading a wake makes: after Update on a due wake, whatever
	// Update returned, and alone on a wake that is not due (no registry contact,
	// no self-update, LastCheck stays where it was). On a due wake that replaced
	// box, this process never returns and the new binary makes its own reading.
	// Nil skips it, which is what a caller that only wants the update says.
	Health func(context.Context)
}

func (t timerRun) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t timerRun) report() say.Reporter { return say.Or(t.Out, nil) }

// run is one wake of the timer: off writes nothing, a wake that is not due reads
// health only, and a due one updates and then reads health.
func (t timerRun) run(ctx context.Context) error {
	if !t.Conf.Timer {
		// One line and nothing else: no state write, no cluster client, no
		// registry. The unit keeps waking box, and box keeps saying this, so an
		// operator who turned the timer off can see that it is off.
		t.report().Line(fmt.Sprintf("box does not check for updates by itself: %s says update.timer: off", t.Path))
		return nil
	}
	state, err := t.State.Load()
	if err != nil {
		return err
	}
	if state.Due(t.now(), t.Conf.Interval) {
		// The reading happens whatever the update did, and it happens here
		// rather than at the end of the chart half: a plan, a registry listing
		// or a step that stopped returns an error, and a failed check leaves
		// LastCheck unwritten -- so every wake would be due and fail again, and
		// a registry outage would silence every health notice for as long as it
		// lasted. The wake still exits 1; the reading is not its answer.
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

// unattended is what --timer means for the two questions a run answers: nobody
// is watching, so it says yes and it asks with plain lines. It is a method so
// the mapping is tested without a run.
func (o *updateFlags) unattended() (yes, plain bool) {
	if *o.timer {
		return true, true
	}
	return *o.yes, *o.plain
}

// noTimerLine is what bootstrap says on a host where box cannot schedule its own
// update: one line, naming the command cron would run, because a box that
// silently never updates is the failure this feature exists to prevent.
const noTimerLine = "box will not check for updates by itself: run `box update --timer` from cron every 15 minutes"

// systemdRunning says this host's systemd is up: /run/systemd/system exists for
// as long as it is. root on a host without it (a container, a PID 1 that is not
// systemd) would get unit files nothing ever reads.
func systemdRunning() bool {
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}

// timerUnits is where bootstrap puts the timer's files on this host, and the one
// line it says when there is nowhere to put them.
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
		return "", fmt.Errorf("find the box binary to schedule: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

// scheduleUpdates writes the timer's units for the files this run installed, and
// says what it did. A failure is a warning, not a failed bootstrap: the seedbox
// is installed and serving, and the line names the cron command that does the
// same job.
func scheduleUpdates(ctx context.Context, unitDir, record, grant, values string, out say.Reporter) {
	units, why := timerUnits(unitDir)
	if units == nil {
		out.Warn(why)
		return
	}
	path, err := boxPath()
	if err != nil {
		out.Warn(noTimerLine + " (" + err.Error() + ")")
		return
	}
	changed, err := hostconf.Install(ctx, units, path, record, grant, values)
	if err != nil {
		out.Warn("box's update timer is not running: " + err.Error())
		return
	}
	if changed {
		out.Muted(fmt.Sprintf("box checks for a newer release every %d minutes: %s, and update.timer in %s turns that off",
			hostconf.WakeInterval, hostconf.TimerFile, hostconf.Path(record)))
		return
	}
	out.Muted(fmt.Sprintf("%s is already installed and up to date", hostconf.TimerFile))
}
