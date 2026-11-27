package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// hostedOptions is an unattended run with alpha (a path app) and gamma (served at a
// host of its own).
func hostedOptions(h *harness) Options {
	opts := h.unattended()
	opts.Apps = []string{"alpha", "gamma"}
	return opts
}

// A wildcard and one record per app are the same answer: the check is about the
// addresses, not which DNS feature produced them.
func TestAppHostAnswersMatchTheDomain(t *testing.T) {
	for _, name := range []string{"a wildcard record", "one record per app"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.decision.Resolver = stubResolver{addrs: map[string][]string{
				"box.example.com":       {"203.0.113.7"},
				"gamma.box.example.com": {"203.0.113.7"},
			}}

			res, err := h.run(hostedOptions(h))
			if err != nil {
				t.Fatalf("init: %v\n%s", err, h.out)
			}
			if len(res.Hosts) != 2 {
				t.Fatalf("hosts = %+v, want the domain and gamma's", res.Hosts)
			}
			domain, app := res.Hosts[0], res.Hosts[1]
			if domain.Host != "box.example.com" || domain.App != "" {
				t.Fatalf("hosts[0] = %+v, want the box's domain", domain)
			}
			if app.Host != "gamma.box.example.com" || app.App != "gamma" {
				t.Fatalf("hosts[1] = %+v, want gamma's own host", app)
			}
			if app.Issuer != IssuerLetsEncrypt || app.NoDNS || app.Record != "" {
				t.Fatalf("gamma's host = %+v, want Let's Encrypt and no record to add", app)
			}
			// The domain and gamma are two hosts with one decision: one folded row.
			if !strings.Contains(h.out.String(), "· certificates   Let's Encrypt for 2 hosts · resolves to 203.0.113.7\n") {
				t.Fatalf("init did not explain the hosts' issuer:\n%s", h.out)
			}
			if len(h.logins) != 1 || strings.Join(h.logins[0].Hosted, ",") != "gamma.box.example.com" {
				t.Fatalf("the login was not told about gamma's host: %+v", h.logins)
			}
			// gamma signs its own users in, so the login bypasses it.
			if len(h.logins) != 1 || strings.Join(h.logins[0].OwnLogin, ",") != "gamma" {
				t.Fatalf("the login was not told gamma signs its users in: %+v", h.logins)
			}
		})
	}
}

// A made-up name resolves nothing under it either, which is a match: init proceeds
// self-signed, each name carrying the record the card repeats.
func TestNoDNSAnywhereProceedsSelfSigned(t *testing.T) {
	h := newHarness(t)
	res, err := h.run(hostedOptions(h))
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(res.Hosts) != 2 {
		t.Fatalf("hosts = %+v, want the domain and gamma's", res.Hosts)
	}
	for _, got := range res.Hosts {
		if got.Issuer != IssuerSelfSigned || !got.NoDNS {
			t.Fatalf("%s = %+v, want self-signed because it has no DNS record", got.Host, got)
		}
	}
	if got := res.Hosts[0].Record; got != "add one" {
		t.Fatalf("the domain's record = %q", got)
	}
	wantRecord := "add either a wildcard *.box.example.com or a record for gamma.box.example.com"
	if got := res.Hosts[1].Record; got != wantRecord {
		t.Fatalf("gamma's record = %q, want %q", got, wantRecord)
	}
	// It proceeded: the grant was written, with the app on.
	if !h.exists(h.grantPath) {
		t.Fatal("init wrote no grant after the no-DNS match")
	}
	if !strings.Contains(h.read(h.grantPath), "gamma:\n    enabled: true") {
		t.Fatalf("the grant does not enable gamma:\n%s", h.read(h.grantPath))
	}
	if strings.Contains(h.out.String(), "app hosts:") {
		t.Fatalf("init printed a mismatch it did not stop on:\n%s", h.out)
	}
}

// An app name answering elsewhere can never issue, so init stops before writing and
// names the record to add, in both forms.
func TestAppHostAnsweringElsewhereStopsInit(t *testing.T) {
	h := newHarness(t)
	h.decision.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"198.51.100.9"},
	}}
	_, err := h.run(hostedOptions(h))
	if err == nil {
		t.Fatal("init accepted an app host that answers somewhere else")
	}
	want := "app hosts: gamma.box.example.com resolves to 198.51.100.9, but box.example.com resolves to 203.0.113.7; " +
		"point gamma.box.example.com at the same addresses (a wildcard *.box.example.com, or gamma.box.example.com), then run box init again"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant    %q", err, want)
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("init wrote a file before stopping on the app host")
	}
	if len(h.applied) != 0 {
		t.Fatal("init handed over to apply despite the app host mismatch")
	}
}

// The domain answering and the app's not is the other mismatch: a wildcard that does
// not cover this name, or a record not added yet.
func TestAppHostMissingWhileTheDomainAnswersStopsInit(t *testing.T) {
	h := newHarness(t)
	h.decision.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com": {"203.0.113.7"},
	}}
	_, err := h.run(hostedOptions(h))
	if err == nil {
		t.Fatal("init accepted an app host with no answer while the domain answers")
	}
	want := "app hosts: box.example.com resolves, but gamma.box.example.com does not; " +
		"add a DNS record for gamma.box.example.com (a wildcard *.box.example.com, or gamma.box.example.com), then run box init again"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant    %q", err, want)
	}
	if h.exists(h.grantPath) {
		t.Fatal("init wrote the grant before stopping on the app host")
	}
}

// tls.issuer forces every host: the operator's file wins over DNS, and no host gets
// a "no record" line, since the record is not why it is self-signed.
func TestForcedIssuerAppliesToAppHosts(t *testing.T) {
	h := newHarness(t)
	write(t, h.valuesPath, "tls:\n  issuer: selfsigned\n")
	h.decision.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	res, err := h.run(hostedOptions(h))
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	for _, got := range res.Hosts {
		if got.Issuer != IssuerSelfSigned || got.NoDNS {
			t.Fatalf("%s = %+v, want the forced self-signed issuer and no no-DNS note", got.Host, got)
		}
	}
	if !strings.Contains(h.out.String(), "tls.issuer=selfsigned forces a self-signed certificate") {
		t.Fatalf("init did not say the issuer was forced:\n%s", h.out)
	}
}

// Four names that answer nothing are one row: each resolver error names its own
// host, so the screen shows the same short fact for all of them and the resolver's
// own words go to the run log.
func TestTheHostsWithNoRecordAreOneRow(t *testing.T) {
	h := newHarness(t)
	opts := hostedOptions(h)
	opts.Apps = []string{"alpha", "gamma"}
	// Each name's error names that name: what a made-up domain sees.
	h.decision.Resolver = stubResolver{errs: map[string]error{
		"box.example.com":       errors.New("lookup box.example.com: no such host"),
		"gamma.box.example.com": errors.New("lookup gamma.box.example.com: no such host"),
	}}
	// A run log, where the resolver's own answer goes.
	var log strings.Builder
	h.decision.Say = &say.Plain{W: h.out, Log: &log}
	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(res.Hosts) != 2 || !res.Hosts[0].NoDNS || !res.Hosts[1].NoDNS {
		t.Fatalf("hosts = %+v, want both self-signed with no record", res.Hosts)
	}
	out := h.out.String()
	if got, want := strings.Count(out, "· certificates   self-signed for 2 hosts · does not resolve\n"), 1; got != want {
		t.Fatalf("the two names are %d rows, want one:\n%s", got, out)
	}
	for _, name := range []string{"box.example.com", "gamma.box.example.com"} {
		if !strings.Contains(log.String(), "dns "+name+": lookup "+name+": no such host") {
			t.Fatalf("the log does not carry the resolver's answer for %s:\n%s", name, log.String())
		}
	}
}

// A name is the instance's, not its app's: a second instance of an app served at a host
// of its own is reached under a name of its own, and App stays the app, which is what
// the login's own-sign-in list is keyed by.
func TestASecondInstanceIsServedAtItsOwnName(t *testing.T) {
	h := newHarness(t)
	instances := []seedbox.Instance{
		{Name: "gamma", App: "gamma"},
		{Name: "gamma-extra", App: "gamma"},
	}
	hosts, err := appHostsFor(context.Background(), h.decision.Resolver, h.decision.Catalog,
		"box.example.com", hostAnswer{}, "", "init", instances)
	if err != nil {
		t.Fatalf("appHostsFor: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v, want one per instance", hosts)
	}
	for i, want := range []string{"gamma.box.example.com", "gamma-extra.box.example.com"} {
		if hosts[i].Host != want {
			t.Fatalf("hosts[%d] = %q, want %q", i, hosts[i].Host, want)
		}
		if hosts[i].App != "gamma" {
			t.Fatalf("hosts[%d].App = %q, want the app both instances come from", i, hosts[i].App)
		}
	}
	// The login's rules are keyed by the names, portal included.
	if got := strings.Join(appHostNames(hosts), ","); got != "gamma.box.example.com,gamma-extra.box.example.com" {
		t.Fatalf("the login's hosted names = %q", got)
	}
}
