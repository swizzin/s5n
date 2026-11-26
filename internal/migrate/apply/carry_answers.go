package apply

// carry_answers.go carries an answer an earlier read of the same source gave, so a
// cutover does not ask a person to repeat themselves (docs/output.md, rule 10): the
// rule keeps the answer, the value leaves the called-out list, and the screen shows
// one line per carried answer.

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

// CarryAnswers writes answers into the plan and returns the list without them. An
// answer matches by host and port, and only a default this run wrote is replaced.
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
			// An answer naming an app this plan lacks cannot be carried: the value
			// stays a default.
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
