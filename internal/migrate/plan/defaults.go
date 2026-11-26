package plan

// defaults.go is what a plan decided by itself: a default is never silent, so
// the screen before the confirm names each one and what it did.

import "strconv"

// Default is one rule the plan decided by itself, in the form a grouped screen
// needs.
type Default struct {
	// App is the instance the address was rewritten to; empty for a rule that
	// leaves the value alone and for a path rule, which names it in What.
	App string
	// What is the address and port box found, or "<instance>: <path>".
	What string
	// Why is the line saying what box does and why; plan validation refuses a
	// default without one, so it is never empty.
	Why string
	// Kind is "address", "path" or "folder".
	Kind string
}

// DefaultRules is every default rule, hosts first, then each app's path
// rules.
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
	// A library directory discovery had to choose is a default too: it renames
	// the operator's own data.
	if p.Data != nil {
		for _, f := range p.Data.Folders {
			if !f.Default {
				continue
			}
			out = append(out, Default{What: "media/" + f.From, Why: f.Why, Kind: "folder"})
		}
	}
	return out
}

// Defaults is every default rule as a report line: what box decided about and,
// in parentheses, what it will do and why.
func (p *Plan) Defaults() []string {
	rules := p.DefaultRules()
	out := make([]string, 0, len(rules))
	for _, d := range rules {
		out = append(out, d.What+" ("+d.Why+")")
	}
	return out
}
