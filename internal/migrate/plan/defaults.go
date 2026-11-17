package plan

// defaults.go is what a plan says it decided by itself: the rules discovery
// wrote as a default and the operator never answered. A default is never
// silent, so the screen before the one confirm names every one of them.

import "strconv"

// Defaults is every default rule in the plan, as the lines a confirm screen
// shows: the address or path box decided about and, in parentheses, what it
// will do and why. A rule with Default set but no Why is refused by the plan
// validation, so a line here never reads as an unexplained decision.
func (p *Plan) Defaults() []string {
	var out []string
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
		out = append(out, at+" ("+r.Why+")")
	}
	for _, name := range p.appNames() {
		for _, r := range p.Apps[name].Paths {
			if !r.Default {
				continue
			}
			out = append(out, name+": "+r.From+" ("+r.Why+")")
		}
	}
	return out
}
