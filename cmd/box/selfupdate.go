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

// `box update` replaces box before chart work, then re-executes with the same
// arguments, so the chart always runs under the newest box -- including the
// timer's unattended run. Every self-update failure is one line naming the
// cause, never a stopped chart update; the binary is replaced only after the
// release verified.

// geteuid is a variable so a test can be root without being root.
var geteuid = os.Geteuid

// boxUpdate is what happens to box itself before chart work.
type boxUpdate struct {
	// Options is the self-update's inputs: version, package, hold and page.
	Options selfupdate.Options

	// Args and Env are the process's own, re-executed under the new binary.
	Args []string
	Env  []string

	// RollbackBox puts the previous binary back and does no chart work.
	RollbackBox bool

	// CheckOnly says what is out and installs nothing.
	CheckOnly bool

	Out say.Reporter

	// Entry points as fields, so a test watches instead of replacing itself.
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

// run applies the box decision and says whether chart work still runs here.
func (b *boxUpdate) run(ctx context.Context) (bool, error) {
	if b.RollbackBox {
		return false, b.rollback()
	}
	if release.IsDevVersion(b.Options.Version) {
		// A development build refuses by name as a line, not an error, and the
		// chart work runs either way.
		b.Out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{fmt.Sprintf("this is a development build of box (version %q), so there is no released version to compare it with: box does not replace itself", b.Options.Version)}}})
		return true, nil
	}
	if selfupdate.Reexecuted(b.Options.Version) {
		// This is already the installed box; asking again would replace the
		// running binary.
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

// rollback puts the one kept previous binary back.
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
	b.Out.Rows([]say.Row{{Mark: say.MarkDone, Name: boxName, Facts: []string{"put the previous one back at " + path}, Command: "box version"}})
	return nil
}

// reportNewer says once what a run that cannot install has waiting. who is the
// rest of the sentence.
// this one does not.
//
// A release the seedbox would move to that needs a newer box is the chart
// source's refusal, not this one's: it names both versions and stops before
// anything is written (internal/release's box.s5n/min-box rule).
func (b *boxUpdate) reportNewer(ctx context.Context, who string) {
	found, err := b.Check(ctx, b.Options)
	if err != nil {
		b.Out.Rows([]say.Row{{Mark: say.MarkNeeds, Name: boxName, Facts: []string{"not checked: " + err.Error()}}})
		return
	}
	if !found.Newer {
		return
	}
	b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: boxName, Facts: []string{found.Latest + " is out", who}}})
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
		b.Out.Rows([]say.Row{{Mark: say.MarkNeeds, Name: boxName, Facts: []string{"not updated", err.Error()}}})
		return true, nil
	}
	if b.Options.Hold {
		// A hold says so on every run: the operator set it, and a silent run
		// reads as one that found nothing newer.
		if result.To != result.From {
			b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: boxName, Facts: []string{result.To + " is out", "--hold-box is set, so it is not installed"}}})
		} else {
			b.Out.Rows([]say.Row{{Mark: say.MarkNote, Name: boxName, Facts: []string{result.From + " is the newest release", "--hold-box is set, so box does not replace itself"}}})
		}
		return true, nil
	}
	if !result.Changed {
		return true, nil
	}
	b.Out.Rows([]say.Row{{Mark: say.MarkDone, Name: boxName, Facts: []string{result.From + " → " + result.To, "signature ok"}}})
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
