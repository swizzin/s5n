package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/catalog"
)

// Host is one certificate host init decided: the box's own domain, and one per
// enabled app whose catalog entry says it is served at a name of its own
// (`exposure: host`).
//
// App is empty for the box's domain. Why is the sentence init prints for the
// name ("resolves to 203.0.113.7: using Let's Encrypt"), and NoDNS says the
// name did not answer and the self-signed issuer was chosen because of it: it
// is what the final card repeats, because a record that does not exist yet is
// a fix the operator can make and re-run, not a failure.
type Host struct {
	Host   string
	App    string
	Issuer string
	Why    string
	NoDNS  bool
	// Record is the fix a NoDNS host names: the DNS record to add, in both
	// forms for an app host. The final card repeats it.
	Record string
}

// appHostsFor resolves every name an enabled app is served at (in catalog order) and checks
// it answers where the box's domain does. The check is the one thing that stops
// a run for a name: a certificate for a name that does not point at the box can
// never issue, so the run ends naming the record to add instead of leaving a
// box that is half served.
//
// It is one function rather than init's method because apply runs the same
// decision for a host that has no listener yet (TLSHosts.Ensure): the two
// commands must not disagree about which name is servable, and command is only
// the one box command the stop tells the operator to run again.
//
// The two empty answers are a match on purpose. A test box with a made-up name
// resolves nothing, and so does every name under it; that is not a mismatch,
// it is a box that will get self-signed certificates until its records exist
// (docs/box.md, "Which issuer a host gets"). A real mismatch is one name
// answering and the other not, or two names answering with different
// addresses.
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
		})
	}
	return out, nil
}

// appHostProblem is the comparison, as one sentence naming the record to add.
// Both forms are named every time: a wildcard record is one line in most DNS
// panels and covers every app, and a per-name record is what an operator who
// cannot use a wildcard needs, so the choice is theirs and the message never
// assumes one.
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

// appHostApps is the catalog names of the apps served at a name of their own,
// for the caller that only needs to know which apps left the path (the login's
// rules).
func appHostApps(hosts []Host) []string {
	var out []string
	for _, h := range hosts {
		if h.App != "" {
			out = append(out, h.App)
		}
	}
	return out
}
