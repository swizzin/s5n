package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/catalog"
)

// Host is one certificate host init decided: the box's domain, and one per enabled
// app served at a name of its own (`exposure: host`). App is empty for the domain;
// Why is the sentence init prints; NoDNS says the name did not answer and the
// self-signed issuer was chosen, which the final card repeats as a fix to make and
// re-run rather than a failure.
type Host struct {
	Host   string
	App    string
	Issuer string
	Why    string
	NoDNS  bool
	// Record is the DNS record to add, in both forms for an app host.
	Record string
	// Detail is the resolver's own answer, for the run log: the screen says "does not
	// resolve", one fact for every name with no record.
	Detail string
}

// appHostsFor resolves every name an enabled app is served at, in catalog order, and
// checks it answers where the domain does: a certificate for a name that does not
// point at the box can never issue, so the run ends naming the record to add rather
// than leaving a box half served.
//
// A plain function because apply runs the same decision for a host with no listener
// yet (TLSHosts.Ensure), and the two commands must not disagree about which name is
// servable; command is the one box command the stop names.
//
// The two empty answers are a match on purpose: a test box with a made-up name
// resolves nothing, and so does every name under it, so it gets self-signed
// certificates until its records exist (docs/box.md). A real mismatch is one name
// answering and the other not, or two answering with different addresses.
func appHostsFor(ctx context.Context, resolver Resolver, cat *catalog.Catalog, domain string, dom hostAnswer, forced, command string, apps []string) ([]Host, error) {
	var out []Host
	for _, app := range apps {
		entry, ok := cat.App(app)
		if !ok || entry.Exposure != catalog.ExposureHost {
			continue
		}
		host := app + "." + domain
		ans := resolveHost(ctx, resolver, host)
		if err := appHostProblem(domain, dom, host, ans, command); err != nil {
			return nil, err
		}
		issuer, why := issuerFor(host, ans, forced)
		noDNS := !ans.resolves() && issuer == IssuerSelfSigned
		record := ""
		if noDNS {
			record = appHostRecord(domain, host)
		}
		out = append(out, Host{
			Host:   host,
			App:    app,
			Issuer: issuer,
			Why:    why,
			NoDNS:  noDNS,
			Record: record,
			Detail: ans.detail(host),
		})
	}
	return out, nil
}

// appHostProblem is the comparison as one sentence naming the record to add. Both
// forms are named every time: the wildcard covers every app in one line, and the
// choice of one is the operator's.
func appHostProblem(domain string, dom hostAnswer, host string, ans hostAnswer, command string) error {
	domOK, appOK := dom.resolves(), ans.resolves()
	if !domOK && !appOK {
		return nil
	}
	if domOK && appOK && sameAddresses(dom.addresses(), ans.addresses()) {
		return nil
	}
	wildcard := "*." + domain
	switch {
	case domOK && !appOK:
		return fmt.Errorf("app hosts: %s resolves, but %s does not; add a DNS record for %s (a wildcard %s, or %s), then run box %s again",
			domain, host, host, wildcard, host, command)
	case !domOK && appOK:
		return fmt.Errorf("app hosts: %s resolves, but %s does not; add a DNS record for %s and one for %s (a wildcard %s covers both), then run box %s again",
			host, domain, domain, host, wildcard, command)
	default:
		return fmt.Errorf("app hosts: %s resolves to %s, but %s resolves to %s; point %s at the same addresses (a wildcard %s, or %s), then run box %s again",
			host, strings.Join(ans.addresses(), ", "), domain, strings.Join(dom.addresses(), ", "), host, wildcard, host, command)
	}
}

// sameAddresses compares two normalised address sets.
func sameAddresses(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hostIssuers is the per-name decision init hands its apply, so the plan compares
// every answering Certificate with this run's answer rather than with tls.issuer
// (empty when DNS decided) or a second resolution.
func hostIssuers(hosts []Host) map[string]string {
	out := make(map[string]string, len(hosts))
	for _, h := range hosts {
		if h.Host != "" && h.Issuer != "" {
			out[h.Host] = h.Issuer
		}
	}
	return out
}

// appHostApps is the catalog names of the apps that left the path, for the login's
// rules.
func appHostApps(hosts []Host) []string {
	var out []string
	for _, h := range hosts {
		if h.App != "" {
			out = append(out, h.App)
		}
	}
	return out
}
