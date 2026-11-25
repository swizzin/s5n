package apply

// survey.go is the summary before there is a box: the same Preflight, staging
// copy and Check pass Run makes, with the one thing a host with no seedbox cannot
// do left to the run after the install.
//
// A migration on a host with no box installs a cluster, a platform layer and an
// empty release, and only then adopts into it. Asking its one question after all
// that is asking about writes that already happened, so the question comes first
// and is asked of what the run would do: this pass. What it cannot know is the
// one app whose rewriter runs the app's own image -- there is no release to run
// one in yet -- so that app's check is deferred, its row says so, and the run
// after the install makes it before anything of the snapshot moves (Run).
//
// The pass writes only inside the config root it is given, which the caller
// throws away: staging is how a copy is measured, and the bytes it measures are
// not a seedbox.

import (
	"context"
	"fmt"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// Survey is the pre-install pass: the summary Run would show, from the same plan
// and the same snapshot, without a cluster.
//
// env.ConfigRoot must be a directory the caller owns and removes: the staging
// copy, the state record and everything else this pass writes live inside it, and
// nothing else on the host is touched. env.Run, env.Secret, env.Owned and
// env.Values are not used: the apps that would need them are the ones whose
// check is deferred, and this pass writes no key, adopts no connection and
// switches nothing on.
func Survey(ctx context.Context, p *plan.Plan, planBytes []byte, env Env) (Summary, error) {
	env.deferred = true
	m := &migration{p: p, env: env, id: PlanID(planBytes), checks: map[string]rewrite.Report{}, deferred: map[string]bool{}}
	if err := Preflight(p, planBytes, env); err != nil {
		return Summary{}, err
	}
	m.dir = stateDir(env.ConfigRoot, m.id)
	st, err := loadState(m.dir)
	if err != nil {
		return Summary{}, err
	}
	st.Plan = PlanDigest(planBytes)
	m.st = st
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if err := m.stage(ctx); err != nil {
		return Summary{}, err
	}
	if err := m.check(ctx); err != nil {
		return Summary{}, err
	}
	if err := m.readKeys(); err != nil {
		return Summary{}, err
	}
	return m.summary(), nil
}

// deferredSentence is the row's sentence about a check a pass could not make, and
// the sentence is on the summary (InstanceSummary.Deferred) so the same row reads
// the same way in the grouped screen and the plain list.
func deferredSentence(app string) string {
	if app == appPlex {
		// Plex is the app whose config is a database only Plex's own binary may
		// write, so its check is a query through the pinned image rather than a
		// read of the file.
		return "its databases are checked once Plex's image runs"
	}
	return "its config is checked once the app's image runs"
}

// Holds reports whether s is the summary the operator agreed to: the same lines,
// except the rows of the apps the agreement said would be checked once their
// image runs, and of an app that this run finds no room for at the config root.
// Both sides of those exceptions are dropped, because neither is something the
// question could know: the check runs after the install, and the room is the
// config path's, which a host with no box is asked for later.
//
// The snapshot and the plan are read twice, so a difference that is not one of
// those is a fault: the operator agreed to one summary, and the run would do
// another. The returned sentence names the first difference, and is empty when
// the two summaries are the same.
func (s Summary) Holds(agreed Summary) string {
	deferred := map[string]bool{}
	short := map[string]bool{}
	for _, in := range agreed.Instances {
		if in.Deferred != "" {
			deferred[in.App] = true
		}
		if in.Short != "" {
			short[in.App] = true
		}
	}
	for _, in := range s.Instances {
		if in.Deferred != "" {
			deferred[in.App] = true
		}
		if in.Short != "" {
			short[in.App] = true
		}
	}
	was, now := agreed.linesFor(deferred, short), s.linesFor(deferred, short)
	for i := 0; i < len(was) || i < len(now); i++ {
		switch {
		case i >= len(now):
			return fmt.Sprintf("the run would no longer %s", was[i])
		case i >= len(was):
			return fmt.Sprintf("the run would also %s", now[i])
		case was[i] != now[i]:
			return fmt.Sprintf("the summary that was agreed to says %q and this run would say %q", was[i], now[i])
		}
	}
	return ""
}

// linesFor is the summary's lines with the named apps reduced to the one thing this
// comparison is allowed to know about them: that they are there, and what they
// carry. A deferred app's check has not run on one side and has on the other, so
// everything its check reports is dropped from both; an app whose carry a run
// had to cut down for room is dropped the same way, down to the bytes it staged,
// because the question could not see the disk it lands on.
func (s Summary) linesFor(deferred, short map[string]bool) []string {
	if len(deferred) == 0 && len(short) == 0 {
		return s.Lines()
	}
	trimmed := s
	trimmed.Instances = append([]InstanceSummary(nil), s.Instances...)
	for i := range trimmed.Instances {
		in := &trimmed.Instances[i]
		if deferred[in.App] {
			in.Changes = nil
			in.Accounts = nil
			in.Notes = nil
			in.Left = nil
			in.Owned = nil
			in.Stopped = 0
			in.Deferred = ""
		}
		if short[in.App] {
			in.Staged = Staged{App: in.App}
			in.Carry = ""
			in.Short = ""
		}
	}
	trimmed.Stopped = 0
	for _, in := range trimmed.Instances {
		trimmed.Stopped += in.Stopped
	}
	// The line about what a shortened app's config holds is the same decision,
	// said once above the rows: it goes with them.
	if len(short) > 0 {
		kept := make([]Carried, 0, len(trimmed.Carried))
		for _, c := range trimmed.Carried {
			if !short[c.App] {
				kept = append(kept, c)
			}
		}
		trimmed.Carried = kept
	}
	return trimmed.Lines()
}
