package plan

// defaults.go is what a plan says it decided by itself: the rules discovery
// wrote as a default and the operator never answered. A default is never
// silent, so the screen before the one confirm names what they did with them;
// the ones that changed an app's own address are counted in that app's row, and
// the ones that leave a stored value alone are said out loud.

import "strconv"

// Default is one rule the plan decided by itself, in the form a grouped screen
// needs: what box decided about, in what words, and which app it settled as.
type Default struct {
	// App is the instance the address was rewritten to, "" for a rule that
	// leaves the stored value alone and for a path rule, which names its
	// instance in What.
	App string
	// What is the subject of the line: the address and port box found, or
	// "<instance>: <path>".
	What string
	// Why is the one line saying what box does with it and why. A rule with
	// Default set but no Why is refused by the plan validation, so this is
	// never empty.
	Why string
	// Kind is "address" or "path": what sort of stored value the rule is
	// about, which is what a count of them is a count of.
	Kind string
}

// DefaultRules is every default rule in the plan, in the order Defaults prints
// them: the host rules first, then each app's path rules.
func (p *Plan) DefaultRules() []Default {
	var out []Default
	for _, r := range p.Hosts {
		if !r.Default {
			continue
		}
		at := r.Host
		if at == "" {
			at = r.Service
		}
		if r.Port != 0 {
			at += ":" + strconv.Itoa(r.Port)
		}
		out = append(out, Default{App: r.App, What: at, Why: r.Why, Kind: "address"})
	}
	for _, name := range p.appNames() {
		for _, r := range p.Apps[name].Paths {
			if !r.Default {
				continue
			}
			out = append(out, Default{What: name + ": " + r.From, Why: r.Why, Kind: "path"})
		}
	}
	return out
}

// Defaults is every default rule as the line a report says it in: what box
// decided about and, in parentheses, what it will do and why. A rule with
// Default set but no Why is refused by the plan validation, so a line here
// never reads as an unexplained decision.
func (p *Plan) Defaults() []string {
	rules := p.DefaultRules()
	out := make([]string, 0, len(rules))
	for _, d := range rules {
		out = append(out, d.What+" ("+d.Why+")")
	}
	return out
}
