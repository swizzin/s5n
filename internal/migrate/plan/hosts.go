package plan

import (
	"fmt"
	"net/netip"
	"strings"
)

// HostRule classifies one address a source app stored for another app.
//
// Only addresses plainly inside the source are rewritten (docs/box.md,
// "Only local addresses are rewritten"): a localhost address, which Resolve
// settles through the apps' Ports without a rule, or a Service discovery found
// in the source cluster. Anything else, a LAN address, a domain or another
// machine, stays as it is unless the plan maps it by hand, and every one apply
// meets has to be classified here, or apply refuses.
type HostRule struct {
	// Service is a Service found in the source cluster, as <name>.<namespace>.
	// It matches the short name (from an app in the same namespace),
	// <name>.<namespace>, <name>.<namespace>.svc and
	// <name>.<namespace>.svc.cluster.local.
	Service string `yaml:"service,omitempty"`
	// Host is any other address, matched exactly (case-insensitively).
	Host string `yaml:"host,omitempty"`
	// Port narrows the rule to one port. Zero matches any port.
	Port int `yaml:"port,omitempty"`
	// App is the box instance the address now means. Exactly one of App and
	// Leave is set.
	App string `yaml:"app,omitempty"`
	// Leave keeps the address as it is: it points outside the source.
	Leave bool `yaml:"leave,omitempty"`
}

// Verdict is what Resolve decided about an address.
type Verdict int

const (
	// Unclassified: no rule covers it. Apply collects it and refuses.
	Unclassified Verdict = iota
	// Rewrite: it means App, and becomes App's address on the box.
	Rewrite
	// Leave: it is kept as stored.
	Leave
)

// Resolution is Resolve's answer.
type Resolution struct {
	Verdict Verdict
	App     string // set when Verdict is Rewrite
}

// Resolve classifies an address that app fromApp stored, host and port as the
// app stored them (port 0 when it stored none).
//
// A localhost address is resolved by its port against every app's Ports: the
// port is the only thing that says which app it was. Two apps claiming one port
// is ambiguous and reads as Unclassified, so the operator settles it.
func (p *Plan) Resolve(fromApp, host string, port int) Resolution {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if isLocalhost(h) {
		var match string
		for _, name := range p.appNames() {
			for _, ap := range p.Apps[name].Ports {
				if ap == port && port != 0 {
					if match != "" {
						return Resolution{}
					}
					match = name
				}
			}
		}
		if match == "" {
			return Resolution{}
		}
		return Resolution{Verdict: Rewrite, App: match}
	}
	ns := p.Apps[fromApp].Namespace
	for _, r := range p.Hosts {
		if r.Port != 0 && r.Port != port {
			continue
		}
		if (r.Service != "" && serviceMatches(r.Service, h, ns)) || (r.Host != "" && strings.EqualFold(r.Host, h)) {
			if r.Leave {
				return Resolution{Verdict: Leave}
			}
			return Resolution{Verdict: Rewrite, App: r.App}
		}
	}
	return Resolution{}
}

func isLocalhost(h string) bool {
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// serviceMatches reports whether host names the Service svc ("name.ns") in one
// of the forms cluster DNS answers, the short form only from inside ns.
func serviceMatches(svc, host, fromNS string) bool {
	svc = strings.ToLower(svc)
	name, ns, _ := strings.Cut(svc, ".")
	switch host {
	case svc, svc + ".svc", svc + ".svc.cluster.local":
		return true
	case name:
		return fromNS != "" && fromNS == ns
	}
	return false
}

// checkHosts enforces that every rule says one thing about one address.
func (p *Plan) checkHosts() []string {
	var problems []string
	for i, r := range p.Hosts {
		at := fmt.Sprintf("hosts[%d]", i)
		if (r.Service == "") == (r.Host == "") {
			problems = append(problems, at+": set exactly one of service and host")
		}
		if r.Service != "" {
			if name, ns, ok := strings.Cut(r.Service, "."); !ok || name == "" || ns == "" || strings.Contains(ns, ".") {
				problems = append(problems, fmt.Sprintf("%s: service %q is not <name>.<namespace>", at, r.Service))
			}
		}
		if r.Host != "" && isLocalhost(strings.ToLower(strings.Trim(r.Host, "[]"))) {
			problems = append(problems, fmt.Sprintf("%s: host %q is localhost, which is resolved by the apps' ports, not by a rule", at, r.Host))
		}
		if r.Port < 0 || r.Port > 65535 {
			problems = append(problems, fmt.Sprintf("%s: port %d is not a port", at, r.Port))
		}
		switch {
		case (r.App == "") == !r.Leave:
			problems = append(problems, at+": set exactly one of app and leave")
		case r.App != "":
			if _, ok := p.Apps[r.App]; !ok {
				problems = append(problems, fmt.Sprintf("%s: app %q is not in apps", at, r.App))
			}
		}
	}
	return problems
}
