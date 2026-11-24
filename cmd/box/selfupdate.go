package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/selfupdate"
)

// `box update` replaces box before it plans any chart work, then re-runs itself
// as the new binary with the arguments it was given. A release that raises
// box.s5n/min-box would otherwise leave an unattended box unable to install
// anything, and a box that can only install the release it already knows is a
// box someone has to replace by hand. Because the update re-executes, the chart
// work always runs under the newest box -- including the timer's, which is the
// run nobody is watching.
//
// Nothing here can stop the chart update. A network failure, a build with no
// signing key, a package-managed binary, a signature that does not verify: each
// is one line naming the cause, and the chart work continues under the box that
// is running -- which is what it would have done before this step existed. The
// binary is only ever replaced after the release verified, so no failure path
// leaves a box on disk that will not run.

// geteuid is os.Geteuid. It is a variable so a test can be root, or not, without
// being either: who may replace the binary is the first question this step asks.
var geteuid = os.Geteuid

// boxUpdate is `box update`'s own step: what happens to the box it is running as
// before the chart work starts.
type boxUpdate struct {
	// Options is what selfupdate needs to know, and nothing this command decides
	// on its own: the version now running, the package that owns the binary if
	// one does, the operator's hold, and the release page (a development e2e
	// points it at a release page of its own).
	Options selfupdate.Options

	// Args and Env are the process's own. A box that replaces itself re-executes
	// with these, so the command that triggered the update runs again under the
	// new binary rather than finishing under the old one.
	Args []string
	Env  []string

	// RollbackBox is --rollback-box: put back the binary an update replaced, and
	// do no chart work at all.
	RollbackBox bool

	// CheckOnly is --check: say what newer box is out and install nothing. A
	// check that replaced the binary would be the one thing an operator asking
	// what would happen must not get.
	CheckOnly bool

	Out say.Reporter

	// selfupdate's entry points, as fields so a test can watch a decision
	// instead of replacing the binary the test runs from.
	Update     func(context.Context, selfupdate.Options) (selfupdate.Result, error)
	Check      func(context.Context, selfupdate.Options) (selfupdate.Available, error)
	Rollback   func(string) error
	Executable func(selfupdate.Options) (string, error)
	Reexec     func(string, []string, []string, string) error
}

func newBoxUpdate(out say.Reporter, args, env []string, opts selfupdate.Options) *boxUpdate {
	return &boxUpdate{
		Options:    opts,
		Args:       args,
		Env:        env,
		Out:        out,
		Update:     selfupdate.Update,
		Check:      selfupdate.Check,
		Rollback:   selfupdate.Rollback,
		Executable: selfupdate.ExecutablePath,
		Reexec:     selfupdate.Reexec,
	}
}

// run carries out this run's decision about box itself, and reports whether the
// chart work should now run in this process. It does not when a newer binary
// took the process over, and not when the run is over.
func (b *boxUpdate) run(ctx context.Context) (bool, error) {
	if b.RollbackBox {
		return false, b.rollback()
	}
	if release.IsDevVersion(b.Options.Version) {
		// A development build has no version to compare a release with, and
		// selfupdate refuses it by name. Saying it as a line rather than an
		// error is what keeps an e2e reading its own transcript from looking
		// broken, and the chart work runs either way.
		b.Out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{fmt.Sprintf("this is a development build of box (version %q), so there is no released version to compare it with: box does not replace itself", b.Options.Version)}}})
		return true, nil
	}
	if selfupdate.Reexecuted(b.Options.Version) {
		// This process is already the box an update installed a moment ago, and
		// the process that installed it said so. Asking again would replace the
		// binary this process is running.
		return true, nil
	}
	if b.CheckOnly {
		b.reportNewer(ctx, "--check, so this run installs nothing")
		return true, nil
	}
	if geteuid() != 0 {
		b.reportNewer(ctx, "root's next `box update` installs it")
		return true, nil
	}
	return b.updateAsRoot(ctx)
}

// rollback puts the previous binary back: the one step back an update keeps.
func (b *boxUpdate) rollback() error {
	if geteuid() != 0 {
		return errors.New("--rollback-box puts root's box back, and this process is not root: run it as root")
	}
	path, err := b.Executable(b.Options)
	if err != nil {
		return err
	}
	if err := b.Rollback(path); err != nil {
		return err
	}
	b.Out.Rows([]say.Row{{Mark: say.MarkDone, Name: "box", Facts: []string{"put the previous one back at " + path}, Command: "box version"}})
	return nil
}

// reportNewer says what a run that cannot or must not install has waiting for
// it, once. who is the rest of the sentence: whose update installs it, or why
// this one does not.
//
// A release the seedbox would move to that needs a newer box is the chart
// source's refusal, not this one's: it names both versions and stops before
// anything is written (internal/release's box.s5n/min-box rule).
func (b *boxUpdate) reportNewer(ctx context.Context, who string) {
	found, err := b.Check(ctx, b.Options)
	if err != nil {
		b.Out.Rows([]say.Row{{Mark: say.MarkNeeds, Name: "box", Facts: []string{"not checked: " + err.Error()}}})
		return
	}
	if !found.Newer {
		return
	}
	b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: "box", Facts: []string{found.Latest + " is out", who}}})
}

// updateAsRoot replaces the binary when a newer release verifies, and reports
// what it did. It returns whether this process still has chart work: a box that
// installed a newer binary and re-executed it has none, because the new one does
// the work.
func (b *boxUpdate) updateAsRoot(ctx context.Context) (bool, error) {
	result, err := b.Update(ctx, b.Options)
	if err != nil {
		// The one line an operator needs: the refusal already names its cause
		// in selfupdate's own words (no key, a package manager, a signature
		// that does not verify, the network).
		b.Out.Rows([]say.Row{{Mark: say.MarkNeeds, Name: "box", Facts: []string{"not updated", err.Error()}}})
		return true, nil
	}
	if b.Options.Hold {
		// A hold says so on every run: the operator set it, and a silent run
		// reads as one that found nothing newer.
		if result.To != result.From {
			b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: "box", Facts: []string{result.To + " is out", "--hold-box is set, so it is not installed"}}})
		} else {
			b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: "box", Facts: []string{result.From + " is the newest release", "--hold-box is set, so box does not replace itself"}}})
		}
		return true, nil
	}
	if !result.Changed {
		return true, nil
	}
	b.Out.Rows([]say.Row{{Mark: say.MarkDone, Name: "box", Facts: []string{result.From + " → " + result.To, "signature ok"}}})
	b.Out.Detail("re-running the update as the new box, from " + result.Path)
	// An exec replaces the process, so cmd/box's deferred Close never runs on
	// this path: the line above could still be buffered, the spinner drawn and
	// the terminal's background painted. Close first; the reporter still prints
	// after Close, so the failure below is reported either way.
	b.Out.Close()
	if err := b.Reexec(result.Path, b.Args, b.Env, result.To); err != nil {
		// The new binary is on disk but this process could not become it, and
		// carrying on would move the seedbox under the box that was just
		// replaced -- the one thing the order exists to prevent. The run stops
		// with nothing moved; the next one starts on the new box.
		return false, fmt.Errorf("box %s is installed at %s, but starting it failed (%v): nothing was moved, so run `box update` again", result.To, result.Path, err)
	}
	return false, nil
}

// updateRun is `box update` in order: box's own replacement first, then the
// chart work, which runs in this process only when no newer binary took it over.
// The order is the point -- the release an update settles on is the one its
// min-box accepts, which means deciding it with the box that does the work -- so
// it is one value a test can run with both halves swapped for fakes.
//
// The chart half's error comes back rather than ending the process there: the
// timer's wake reads health after it either way, and the caller is the one place
// that prints and picks an exit code.
type updateRun struct {
	Box    *boxUpdate
	Charts func(context.Context) error
}

func (r updateRun) run(ctx context.Context) error {
	proceed, err := r.Box.run(ctx)
	if err != nil || !proceed {
		return err
	}
	return r.Charts(ctx)
}
