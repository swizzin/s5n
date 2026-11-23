package main

// migrate_prior.go is cutover's second read of the source: the one already on
// record.
//
// A cutover that has never read this source is the first thing box has seen of
// it, so anything the operator deleted before that is invisible: a workload
// scaled to 0 is still there to read, a deleted one is gone. So a cutover with no
// earlier read says what it found and asks; an unattended run refuses instead and
// names `box migrate discover`, the read to take while the source is whole.
//
// With an earlier read of the same source on record it diffs instead: an app the
// earlier read had and this one cannot find is named and the cutover stops, and an
// app this read has and the earlier one did not is new, listed, and kept.
//
// The source is one host and one scope (plan.Source's host and namespaces), so the
// search is for the newest snapshot in the directory whose plan names both. Both
// the diff and the first-read refusals write their read as a survey first, exactly
// as the still-running refusal does: whatever stops the run, the next cutover
// carries this read.

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// errCutoverPrior says the cutover stopped on the read already on record: what it
// found is printed, and the read is saved as a survey first.
var errCutoverPrior = errors.New("cutover stopped on the earlier read of this source")

// priorRead is the newest earlier read of this source, if there is one.
type priorRead struct {
	// path is the snapshot file, named in what the run says.
	path string
	// mode is what the source was when it was read, so the operator knows
	// whether the earlier read was taken with the apps running.
	mode archive.Mode
	plan *plan.Plan
}

// findPriorRead is the newest snapshot in dir whose plan names the same source as
// this read: the same host, and the same namespaces. A plan that names no host --
// a source whose nodes could not be read -- matches another such plan by its
// namespaces alone, and the run says so rather than implying a host matched.
func findPriorRead(dir string, read migrationRead) (*priorRead, error) {
	reads, err := bundle.Reads(dir)
	if err != nil {
		return nil, err
	}
	host, namespaces := scopeOf(read.plan)
	for _, r := range reads {
		p, err := plan.Parse(r.Plan)
		if err != nil || p.Source == nil {
			// A plan box cannot read is not a read of anything: it stays for the
			// operator, and the search goes on with the next one.
			continue
		}
		priorHost, priorNamespaces := scopeOf(p)
		if priorHost != host || !sameList(priorNamespaces, namespaces) {
			continue
		}
		return &priorRead{path: r.Path, mode: r.Mode, plan: p}, nil
	}
	return nil, nil
}

// scopeOf is what makes two reads of one cluster the same source: the host it ran
// on and the namespaces it was read for.
func scopeOf(p *plan.Plan) (string, []string) {
	if p == nil || p.Source == nil {
		return "", nil
	}
	namespaces := append([]string(nil), p.Source.Namespaces...)
	sort.Strings(namespaces)
	return p.Source.Host, namespaces
}

// sameList reports whether two sorted lists are the same list.
func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// matchedOn says what this read and the earlier one were matched on, so the
// operator can check box compared the two reads it should have.
func (p *priorRead) matchedOn(host string, namespaces []string) string {
	scope := "every namespace"
	if len(namespaces) > 0 {
		scope = "namespaces " + strings.Join(namespaces, ", ")
	}
	if host == "" {
		return fmt.Sprintf("%s, %s; neither read names a source host", p.mode, scope)
	}
	return fmt.Sprintf("%s of %s, %s", p.mode, host, scope)
}

// priorApp is one app an earlier read had and this one cannot find.
type priorApp struct {
	name string
	app  plan.App
}

// diffApps is what changed between an earlier read and this one, by box's
// instance name: the apps that are gone, and the apps that are new.
func diffApps(prior, now *plan.Plan) (gone []priorApp, added []string) {
	for _, name := range sortedKeys(prior.Apps) {
		if _, ok := now.Apps[name]; !ok {
			gone = append(gone, priorApp{name: name, app: prior.Apps[name]})
		}
	}
	for _, name := range sortedKeys(now.Apps) {
		if _, ok := prior.Apps[name]; !ok {
			added = append(added, name)
		}
	}
	return gone, added
}

// checkPriorRead is what a cutover does about the read already on record. It
// returns nil when the run may go ahead, and the refusal to stop on otherwise;
// every refusal saves this read as a survey first, through keep, so following
// what box says is always safe.
//
// --no-prior-discover is the operator's own word that there is nothing earlier to
// compare against: it skips the first-read refusal, never the diff, because a read
// that does exist is evidence either way.
func checkPriorRead(prior *priorRead, read *migrationRead, o cutoverOptions, out say.Reporter, keep func() (string, error)) error {
	host, namespaces := scopeOf(read.plan)
	if prior != nil {
		gone, added := diffApps(prior.plan, read.plan)
		out.Line(fmt.Sprintf("earlier read: %s (%s)", filepath.Base(prior.path), prior.matchedOn(host, namespaces)))
		if len(added) > 0 {
			out.List("new since the earlier read (kept)", appLines(added, read.plan))
		}
		if len(gone) == 0 {
			return nil
		}
		kept, err := keep()
		if err != nil {
			return err
		}
		out.List("an earlier read had these, and this one cannot find them", goneLines(gone))
		out.Warn(fmt.Sprintf("%s had %s this read cannot find: cutover stops rather than migrate a source that lost one",
			filepath.Base(prior.path), appCount(len(gone))))
		out.Line(surveyLine(kept))
		out.Muted("box is not the one who deleted them: put each back the way it was and run cutover again, or run `box migrate discover` if they are gone for good.")
		return errCutoverPrior
	}
	if o.NoPriorDiscover {
		out.Line("no earlier read of this source on record; going ahead because --no-prior-discover was given")
		return nil
	}
	if o.Prompt != nil {
		out.List("this is the first read of this source", appLines(sortedKeys(read.plan.Apps), read.plan))
		// The safe answer is the question, not a guess: an empty enter sends the
		// operator to `box migrate discover`, which reads while the source is
		// whole.
		yes, err := o.Prompt.Confirm(setup.Confirm{
			Prompt:      "is this everything the source ran? an app deleted before now is invisible to this read",
			Default:     false,
			Affirmative: "yes, cut over",
			Negative:    "no, discover first",
		})
		if err != nil {
			return err
		}
		if yes {
			return nil
		}
		kept, err := keep()
		if err != nil {
			return err
		}
		out.Line("not cutting over, then.")
		out.Line(surveyLine(kept))
		out.Line(noPriorAdvice)
		return errCutoverPrior
	}
	kept, err := keep()
	if err != nil {
		return err
	}
	out.List("this is the first read of this source", appLines(sortedKeys(read.plan.Apps), read.plan))
	out.Warn("nothing read this source while it was whole, so anything deleted before now is invisible to this read")
	out.Line(surveyLine(kept))
	out.Line(noPriorAdvice)
	return errCutoverPrior
}

// noPriorAdvice is the better first step, said once: a read taken while the source
// is whole is what makes a cutover's diff mean something.
const noPriorAdvice = "run `box migrate discover` while the source is still whole, then run cutover again: it compares the two reads and stops if an app went missing. `--no-prior-discover` says you know there is nothing earlier to compare against."

// appCount is "1 app" or "3 apps", for a line about how many went missing.
func appCount(n int) string {
	if n == 1 {
		return "one app"
	}
	return fmt.Sprintf("%d apps", n)
}

// surveyLine is where a refusing cutover put the read it kept, and why that
// matters: the next cutover carries it.
func surveyLine(path string) string {
	return fmt.Sprintf("survey: %s (the read is kept, so the next cutover carries it)", path)
}

// appLines is each named app with the name it had in the source and where it ran.
func appLines(names []string, p *plan.Plan) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		app := p.Apps[name]
		out = append(out, fmt.Sprintf("%s (from %s, in %s)", name, app.Source, namespaceOf(app)))
	}
	return out
}

// goneLines is the apps an earlier read had and this one cannot find.
func goneLines(gone []priorApp) []string {
	out := make([]string, 0, len(gone))
	for _, g := range gone {
		out = append(out, fmt.Sprintf("%s (it was %s in %s)", g.name, g.app.Source, namespaceOf(g.app)))
	}
	return out
}

// namespaceOf is the namespace an app ran in, or the plan itself when the source
// was not a cluster.
func namespaceOf(app plan.App) string {
	if app.Namespace == "" {
		return "no namespace"
	}
	return app.Namespace
}

// priorDir is the directory a cutover looks in for the read already on record:
// beside the snapshot this run writes, which is the bundle directory unless --out
// named another one. A run that writes elsewhere is not asking about some other
// directory's history.
func priorDir(o cutoverOptions) string {
	return filepath.Dir(snapshotPath(o.discoverOptions, archive.ModeFinal))
}
