package main

import (
	"context"
	"fmt"
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

	// Update is the whole update: box's own replacement, then the charts, then
	// the health read. It runs only when a check is due, so a wake between
	// checks asks the registry nothing and replaces nothing.
	Update func(context.Context) error
	// Health is the reading a wake that is not due makes: no registry contact,
	// no self-update, and LastCheck stays where it was. Nil is a run that only
	// updates, which is what part of this command's build is.
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
		return t.Update(ctx)
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
