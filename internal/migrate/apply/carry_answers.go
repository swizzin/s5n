package apply

// carry_answers.go is cutover's half of settle.go's guided run: an answer an
// earlier read of the same source already gave.
//
// A cutover reads the source again, so it meets the same addresses discover met,
// and asking about them a second time is asking a person to repeat themselves
// (docs/output.md, rule 10: an answer collapses to subject -> answer, and is not
// asked again). What the earlier read answered is carried into this plan as an
// answer, not a default: the rule keeps the answer, the value leaves the
// called-out list, and the screen shows one line per carried answer.

import (
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// KeptAnswer is one answer an earlier read gave, as this run meets it again.
type KeptAnswer struct {
	// Value is what the answer is about: the address as stored.
	Value string
	// App is what the earlier answer points it at, empty for "leave it as it
	// is".
	App string
	// Holds is where the address is stored, one entry per app and place.
	Holds []SettledHold
}

// Answer is the answer as one line reads it: the app, or that the value stays.
func (c KeptAnswer) Answer() string {
	if c.App == "" {
		return "left as it is"
	}
	return c.App
}

// CarryAnswers writes an earlier read's answers into this run's plan, and returns
// the called-out list without them: an answered value is not a default any more,
// so it is not asked about and not listed as something box decided alone.
//
// An answer is matched by the address it was given for -- the host and the port,
// which is what both plans' rules match -- and only a default this run wrote is
// replaced. A rule this plan already had an answer for is the operator's own and
// is left alone.
func CarryAnswers(now *plan.Plan, s Settled, prior *plan.Plan) (Settled, []KeptAnswer) {
	if now == nil || prior == nil {
		return s, nil
	}
	answers := map[string]plan.HostRule{}
	for _, r := range prior.Hosts {
		if r.Default || r.Service != "" || r.Host == "" {
			continue
		}
		answers[addressKey(r.Host, r.Port)] = r
	}
	if len(answers) == 0 {
		return s, nil
	}
	var carried []KeptAnswer
	kept := Settled{}
	for _, g := range s {
		for _, it := range g.Items {
			if it.Host == "" {
				kept.add(g.Action, it)
				continue
			}
			a, ok := answers[addressKey(it.Host, it.Port)]
			if !ok {
				kept.add(g.Action, it)
				continue
			}
			// A rule may only name an app this plan has: an earlier read of a
			// different app list is an answer this plan cannot carry, so the
			// value stays a default and is asked about.
			if a.App != "" {
				if _, ok := now.Apps[a.App]; !ok {
					kept.add(g.Action, it)
					continue
				}
			}
			if err := setAddressRule(now, it.Host, it.Port, a.App); err != nil {
				kept.add(g.Action, it)
				continue
			}
			carried = append(carried, KeptAnswer{Value: it.Value, App: a.App, Holds: it.Holds})
		}
	}
	kept.sort()
	return kept, carried
}
