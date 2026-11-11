package setup

import (
	"strings"
	"testing"
)

// hostedOptions is an unattended run with alpha (a path app) and gamma (the
// fixture's app served at a host of its own) selected.
func hostedOptions(h *harness) Options {
	opts := h.unattended()
	opts.Apps = []string{"alpha", "gamma"}
	return opts
}

// Both record shapes are the same answer to init: a wildcard record and one
// record per app both make gamma.<domain> resolve where the domain does, so
// the check is about the addresses, not about which DNS feature produced them.
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
			if app.Issuer != IssuerLetsEncrypt || app.NoDNS {
				t.Fatalf("gamma's host = %+v, want Let's Encrypt", app)
			}
			if !strings.Contains(h.out.String(), "gamma.box.example.com resolves to 203.0.113.7: using Let's Encrypt") {
				t.Fatalf("init did not explain gamma's issuer:\n%s", h.out)
			}
			if len(h.logins) != 1 || strings.Join(h.logins[0].Hosted, ",") != "gamma" {
				t.Fatalf("the login was not told about gamma's host: %+v", h.logins)
			}
		})
	}
}

// A test box with a made-up name resolves nothing under it either: that is a
// match, init proceeds self-signed, and each name carries the record the final
// card repeats — both forms for an app host.
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

// An app name that answers somewhere else can never get a certificate through a
// listener on this box, so init stops before it writes anything and names the
// record to add, in both forms.
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

// The domain answering and the app's name not is the other real mismatch: a
// wildcard that does not cover this name, or a record nobody added yet.
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

// tls.issuer forces every host, the app's included: the operator's file wins
// over what DNS says, and no host gets a "no record" line because the record
// is not why the certificate is self-signed.
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
