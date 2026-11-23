package plan

import (
	"fmt"
	"net/netip"
	"strconv"
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
	// Path narrows a host rule to one URL path prefix: the path itself, and
	// anything under it, on a "/" boundary. Empty matches every path. Only a
	// host has a path -- a Service address is the app's own, and an address
	// stored for it carries no path worth matching.
	Path string `yaml:"path,omitempty"`
	// Port narrows the rule to one port. Zero matches any port.
	Port int `yaml:"port,omitempty"`
	// App is the box instance the address now means. Exactly one of App and
	// Leave is set.
	App string `yaml:"app,omitempty"`
	// Leave keeps the address as it is: it points outside the source.
	Leave bool `yaml:"leave,omitempty"`
	// Why is one line saying why discovery settled this address this way,
	// written when the rule is a default rather than the operator's own.
	Why string `yaml:"why,omitempty"`
	// Default says discovery chose this rule, and the operator has not
	// confirmed it. A default without a Why is a decision with no reason.
	Default bool `yaml:"default,omitempty"`
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
//
// A rule that names a path is not considered: an address read without a path is
// no evidence for one of the apps a host serves under paths (ResolveURL is what
// a caller holding the path uses). A host that has only path rules therefore
// stays Unclassified, rather than naming the app at "/" or whichever rule comes
// first.
func (p *Plan) Resolve(fromApp, host string, port int) Resolution {
	h := normaliseHost(host)
	if isLocalhost(h) {
		return p.resolveLocal(port)
	}
	ns := p.Apps[fromApp].Namespace
	for _, r := range p.Hosts {
		if r.Path != "" {
			continue
		}
		if r.Port != 0 && r.Port != port {
			continue
		}
		if r.matches(h, ns) {
			return r.verdict()
		}
	}
	return Resolution{}
}

// ResolveURL classifies an address that app fromApp stored, with the URL path it
// stored when it has one ("" when the address names no path).
//
// It is Resolve with the path, because one host can serve several apps -- box's
// own shape is one domain with each app under a path -- and the path is then the
// only thing that says which app an address meant. The rule whose path is the
// longest prefix of urlPath wins, on a "/" boundary, and a rule with no path
// covers every path. A host that has only path rules settles an address with no
// path as Unclassified: never one of its apps, because that would be a guess,
// and a guess points one app's connection at another.
func (p *Plan) ResolveURL(fromApp, host string, port int, urlPath string) Resolution {
	h := normaliseHost(host)
	if isLocalhost(h) {
		return p.resolveLocal(port)
	}
	ns := p.Apps[fromApp].Namespace
	best := -1
	var out Resolution
	for _, r := range p.Hosts {
		if r.Port != 0 && r.Port != port {
			continue
		}
		if !r.matches(h, ns) {
			continue
		}
		if r.Path != "" && !pathCovers(r.Path, urlPath) {
			continue
		}
		switch n := len(r.Path); {
		case n > best:
			best, out = n, r.verdict()
		case n == best && r.verdict() != out:
			// Two rules say different things about one address: the
			// operator settles it.
			return Resolution{}
		}
	}
	return out
}

// resolveLocal is the app whose port is port, when exactly one app claims it.
func (p *Plan) resolveLocal(port int) Resolution {
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

// matches reports whether the rule is about the host, as normaliseHost wrote it,
// from an app in namespace fromNS.
func (r HostRule) matches(h, fromNS string) bool {
	return (r.Service != "" && serviceMatches(r.Service, h, fromNS)) || (r.Host != "" && strings.EqualFold(r.Host, h))
}

// verdict is what the rule says about an address it matches.
func (r HostRule) verdict() Resolution {
	if r.Leave {
		return Resolution{Verdict: Leave}
	}
	return Resolution{Verdict: Rewrite, App: r.App}
}

// pathCovers reports whether prefix covers urlPath: the same path, or one under
// it, on a segment boundary -- /qbittorrentx is not under /qbittorrent. A
// prefix of "/" covers every path, and an address with no path is covered only
// by that.
func pathCovers(prefix, urlPath string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	return urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/")
}

// normaliseHost is a stored host as rules are matched against it: lower case,
// no brackets and no trailing dot.
func normaliseHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
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
	seen := make(map[string]bool, len(p.Hosts))
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
		if r.Path != "" {
			if r.Service != "" {
				problems = append(problems, fmt.Sprintf("%s: path %q is only for a host: a Service address carries no path", at, r.Path))
			}
			if !strings.HasPrefix(r.Path, "/") {
				problems = append(problems, fmt.Sprintf("%s: path %q must start with /", at, r.Path))
			}
			if strings.ContainsAny(r.Path, "\r\n") {
				problems = append(problems, fmt.Sprintf("%s: path %q must be one line", at, r.Path))
			}
		}
		if r.Host != "" && isLocalhost(strings.ToLower(strings.Trim(r.Host, "[]"))) {
			problems = append(problems, fmt.Sprintf("%s: host %q is localhost, which is resolved by the apps' ports, not by a rule", at, r.Host))
		}
		if r.Port < 0 || r.Port > 65535 {
			problems = append(problems, fmt.Sprintf("%s: port %d is not a port", at, r.Port))
		}
		// One address is said once: two rules for the same host, port and
		// path (or the same Service and port) would leave the answer to the
		// order of the list. A host at several paths is several rules.
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s",
			strings.ToLower(r.Service), strings.ToLower(r.Host), r.Port, r.Path)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("%s: %s appears twice", at, ruleAt(r)))
		}
		seen[key] = true
		switch {
		case (r.App == "") == !r.Leave:
			problems = append(problems, at+": set exactly one of app and leave")
		case r.App != "":
			if _, ok := p.Apps[r.App]; !ok {
				problems = append(problems, fmt.Sprintf("%s: app %q is not in apps", at, r.App))
			}
		}
		problems = append(problems, whyProblems(at, r.Why, r.Default)...)
	}
	return problems
}

// ruleAt is a rule's address as a refusal names it: the host with its path, or
// the Service, and the port when the rule was narrowed to one.
func ruleAt(r HostRule) string {
	at := r.Service
	if at == "" {
		at = r.Host + r.Path
	}
	if at == "" {
		at = "an address"
	}
	if r.Port != 0 {
		at += ":" + strconv.Itoa(r.Port)
	}
	return at
}
