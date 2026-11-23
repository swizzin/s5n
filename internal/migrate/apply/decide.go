package apply

// decide.go is the guided run's half of settle.go: the operator answers each
// default, and the answers become the plan's rules.
//
// A default is a decision Settle made because nothing else could, and the
// maintainer's rule is that a default is never silent. So a guided run shows
// the same lines report.txt carries, asks one question per address, and writes
// every answer into the rule it replaces. An unattended run takes them all and
// says so; this file is what makes the answered default a rule the operator
// chose rather than a default nobody looked at.

import (
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// Choice is one default a guided run asks about: an address the plan did not
// settle, the decision box took about it, and where the value is stored.
type Choice struct {
	// Action is the decision box took: "left as it is" or "rewritten to <app>".
	Action string
	Value  string
	Why    string
	// Host and Port are the address the rule matches, as stored.
	Host string
	Port int
	// App is what the default points the address at, and empty for a leave.
	App string
	// Holds is where the address is stored, one entry per app and place.
	Holds []SettledHold
}

// Decide asks about every address Settle left as a default, and writes the
// answers into the plan. ask answers one choice with the app to point it at, or
// "" to leave it as it is.
//
// It returns the decisions as they now stand: an answered value moves to the
// group its answer names, carries the answer as its reason, and stops being a
// default. Nothing else in the list changes, and a plan with no address default
// is returned as it was.
func Decide(p *plan.Plan, s Settled, ask func(Choice) (string, error)) (Settled, error) {
	if p == nil {
		return nil, fmt.Errorf("decide: no plan: there is nothing to decide")
	}
	out := Settled{}
	for _, g := range s {
		for _, it := range g.Items {
			if it.Host == "" {
				// A path or a check: box has no alternative to offer. The
				// decision stands and stays in the list, so the report still
				// says what happens to it.
				out.add(g.Action, it)
				continue
			}
			app, err := ask(Choice{
				Action: g.Action, Value: it.Value, Why: it.Why,
				Host: it.Host, Port: it.Port, App: it.App, Holds: it.Holds,
			})
			if err != nil {
				return nil, err
			}
			if app != "" {
				if _, ok := p.Apps[app]; !ok {
					return nil, fmt.Errorf("%s is not an app in this plan, so %s cannot point at it", app, it.Value)
				}
			}
			if err := setAddressRule(p, it.Host, it.Port, app); err != nil {
				return nil, err
			}
			answered := it
			answered.Default = false
			answered.App = app
			answered.Why = answerWhy(app)
			if app == "" {
				out.add(actionLeave, answered)
				continue
			}
			out.add(actionRewrite+app, answered)
		}
	}
	out.sort()
	return out, nil
}

// answerWhy is what an answered value's line says: the decision is the
// operator's now, and the report keeps the reason it was asked about.
func answerWhy(app string) string {
	if app == "" {
		return "you chose to leave it as stored"
	}
	return "you chose " + app
}

// setAddressRule writes one answer into the rule Settle made for an address.
// Only a default is ever replaced: a rule the operator wrote is the answer
// already, and Settle never asks about one.
func setAddressRule(p *plan.Plan, host string, port int, app string) error {
	for i := range p.Hosts {
		r := &p.Hosts[i]
		if !r.Default || r.Service != "" || !sameAddress(r.Host, host) || r.Port != port {
			continue
		}
		r.Leave = app == ""
		r.App = app
		r.Default = false
		r.Why = answerWhy(app)
		return nil
	}
	return fmt.Errorf("%s:%d is not a default in this plan, so there is no rule for the answer", host, port)
}

// sameAddress compares two stored hosts the way a rule does: DNS names are
// case-insensitive, and brackets or a trailing dot are spelling.
func sameAddress(a, b string) bool {
	key := func(h string) string { return strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), ".")) }
	return key(a) == key(b)
}
