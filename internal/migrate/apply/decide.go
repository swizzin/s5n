package apply

// decide.go is settle.go's guided half: the operator answers each address default
// and the answer becomes the rule. A default is never silent, so each is asked and
// written into the rule it replaces; an unattended run takes them all and says so.

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

// Decide asks about every address default and writes the answers in; ask returns
func Decide(p *plan.Plan, s Settled, ask func(Choice) (string, error)) (Settled, error) {
	if p == nil {
		return nil, fmt.Errorf("decide: no plan: there is nothing to decide")
	}
	out := Settled{}
	for _, g := range s {
		for _, it := range g.Items {
			if it.Host == "" {
				// A path or a check has no alternative: it stays in the list.
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

// answerWhy says the decision is the operator's, keeping the reason asked about.
func answerWhy(app string) string {
	if app == "" {
		return "you chose to leave it as stored"
	}
	return "you chose " + app
}

// setAddressRule writes one answer into Settle's rule; only a default is ever
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

// sameAddress compares hosts as a rule does: case-insensitive, brackets and a
func sameAddress(a, b string) bool {
	key := func(h string) string { return strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), ".")) }
	return key(a) == key(b)
}
