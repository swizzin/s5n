package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/seedbox"
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

// appHostsFor resolves every name an enabled workload is served at and checks it answers
// where the domain does: a certificate for a name not pointing at the box can never
// issue, so the run ends naming the record rather than leaving a box half served.
//
// A name is the instance's, not its app's (`s5n.appHost`): a second instance of an app
// that lives at a host of its own is reached at <instance>.<domain>, and App stays the app
// so the login's own rules can tell which instances sign their users in.
//
// A plain function because apply makes the same decision (TLSHosts.Ensure). Two empty
// answers are a match on purpose: a made-up name resolves nothing under it, so a test box
// goes self-signed until its records exist. A real mismatch is one answering and the
// other not.
func appHostsFor(ctx context.Context, resolver Resolver, cat *catalog.Catalog, domain string, dom hostAnswer, forced, command string, instances []seedbox.Instance) ([]Host, error) {
	var out []Host
	for _, inst := range instances {
		entry, ok := cat.App(inst.App)
		if !ok || entry.Exposure != catalog.ExposureHost {
			continue
		}
		host := inst.Name + "." + domain
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
			App:    inst.App,
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

// appHostNames is the names that left the path, for the login's rules: the rule is the
// whole <instance>.<domain>, so the name is what they are keyed by.
func appHostNames(hosts []Host) []string {
	var out []string
	for _, h := range hosts {
		if h.Host != "" {
			out = append(out, h.Host)
		}
	}
	return out
}
