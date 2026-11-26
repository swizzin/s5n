package plan

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// HostRule classifies one address a source app stored for another. Only
// addresses plainly inside the source are rewritten (docs/box.md, "Only local
// addresses are rewritten"): localhost, settled by port, or a Service in the
// source cluster. Everything else apply meets must be classified here or it
// refuses.
type HostRule struct {
	// Service is a Service found in the source cluster, as <name>.<namespace>,
	// matched in every cluster-DNS form, the short one only inside namespace.
	Service string `yaml:"service,omitempty"`
	// Host is any other address, matched case-insensitively.
	Host string `yaml:"host,omitempty"`
	// Path narrows a host rule to one URL path prefix, on a "/" boundary; empty
	// matches every path. Only a host has a path.
	Path string `yaml:"path,omitempty"`
	// Port narrows the rule to one port; zero matches any.
	Port int `yaml:"port,omitempty"`
	// App is the box instance the address now means; exactly one of App and
	// Leave is set.
	App string `yaml:"app,omitempty"`
	// Leave keeps the address as it is: it points outside the source.
	Leave bool `yaml:"leave,omitempty"`
	// Why is one line saying why discovery settled this address, required when
	// the rule is a default.
	Why string `yaml:"why,omitempty"`
	// Default says discovery chose this rule, unconfirmed.
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

// Resolve classifies an address that app fromApp stored, host and port as stored
// (0 when none). A localhost address is resolved by its port across every app's
// Ports; two apps on one port is ambiguous and reads Unclassified. A path rule is
// not considered -- ResolveURL is what a caller holding a path uses -- so a host
// that only has path rules stays Unclassified rather than guessing.
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

// ResolveURL classifies an address with the URL path stored with it ("" when
// none). One host can serve several apps -- one domain, each app under a path --
// and then the path is the only thing that says which app an address meant: the
// longest prefix wins, on a "/" boundary. An address with no path on such a host
// stays Unclassified, because a guess points one app's connection at another.
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
			// Two rules disagree about one address: the operator settles it.
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

// matches reports whether the rule is about the host, normalised, from an app in
// namespace fromNS.
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

// pathCovers reports whether prefix covers urlPath on a segment boundary
// (/qbittorrentx is not under /qbittorrent); "/" covers every path, and "" only
// an address with no path.
func pathCovers(prefix, urlPath string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	return urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/")
}

// normaliseHost is a stored host lower-cased, without brackets or a trailing
// dot.
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

// serviceMatches reports whether host names the Service svc ("name.ns") in a
// cluster-DNS form, the short one only from inside ns.
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
		// One address is said once, or the answer is the list's order; a host at
		// several paths is several rules.
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

// ruleAt is a rule's address as a refusal names it.
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
