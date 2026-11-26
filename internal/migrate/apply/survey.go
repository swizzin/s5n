package apply

// survey.go is the summary before there is a box: Preflight, staging and Check as
// Run makes them, with the app whose rewriter needs its own image deferred to the
// run after the install. The question must come before a host installs anything,
// so it is asked of what that run would do. The pass writes only inside the config
// root the caller throws away.

import (
	"context"
	"fmt"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// Survey is the pre-install pass from the same plan and snapshot, without a
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
func deferredSentence(app string) string {
	if app == appPlex {
		// Plex's check needs its own image, not a read of the file.
		return "its databases are checked once Plex's image runs"
	}
	return "its config is checked once the app's image runs"
}

// Holds reports whether s is the summary agreed to, ignoring what the question
// could not know: a deferred check's rows and an app the disk forced smaller. Any
// other difference names the first.
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

// linesFor reduces the named apps to what the comparison may know: that they are
// there and what they carry, dropping a check that ran on one side and a cut-down
// carry the question could not see.
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
