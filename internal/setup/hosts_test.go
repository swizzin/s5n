package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
)

// testDomain is the box's own name; the app hosts hang under it.
const testDomain = "box.example.com"

// hostRecorder is the per-host write a step is given: which names, which issuer,
// and in what order.
type hostRecorder struct {
	hosts []Host
	err   error
}

func (r *hostRecorder) apply(_ context.Context, hosts []Host) error {
	if r.err != nil {
		return r.err
	}
	r.hosts = append(r.hosts, hosts...)
	return nil
}

// hostStep is the step over the one domain every case here uses.
func hostStep(t *testing.T, r *hostRecorder) (*TLSHosts, *strings.Builder) {
	t.Helper()
	out := &strings.Builder{}
	return &TLSHosts{
		Domain:  testDomain,
		Command: "apply",
		Apply:   r.apply,
		Say:     &say.Plain{W: out, Err: out},
	}, out
}

// countingResolver counts the DNS questions, so a test proves a decision is not
// retaken.
type countingResolver struct{ calls *int }

func (c countingResolver) LookupHost(context.Context, string) ([]string, error) {
	*c.calls++
	return nil, nil
}

// init decides every name's issuer before it hands over, and the step writing the
// Certificates must use the same one: a second DNS answer could disagree with what
// the operator confirmed, so a Decided name is written and never resolved again.
func TestTLSHostsWritesTheIssuerTheRunDecided(t *testing.T) {
	rec := &hostRecorder{}
	step, out := hostStep(t, rec)
	step.Command = "init"
	step.Decided = map[string]string{
		"box.example.com":       IssuerLetsEncrypt,
		"gamma.box.example.com": IssuerSelfSigned,
	}
	var calls int
	step.Resolver = countingResolver{calls: &calls}

	if err := step.Ensure(context.Background(), []string{"box.example.com", "gamma.box.example.com"}); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	if calls != 0 {
		t.Fatalf("the step resolved %d names, want none: init already decided them", calls)
	}
	if len(rec.hosts) != 2 {
		t.Fatalf("wrote %+v, want both decided hosts", rec.hosts)
	}
	if rec.hosts[0].Issuer != IssuerLetsEncrypt || rec.hosts[1].Issuer != IssuerSelfSigned {
		t.Fatalf("issuers = %q, %q; want this run's decisions", rec.hosts[0].Issuer, rec.hosts[1].Issuer)
	}
	if rec.hosts[1].Host != "gamma.box.example.com" {
		t.Fatalf("host = %q, want gamma's name", rec.hosts[1].Host)
	}
}

// Both names answered where the domain did, so both are written with their own
// issuer.
func TestTLSHostsWritesEveryNameThatMatchesTheDomain(t *testing.T) {
	rec := &hostRecorder{}
	step, out := hostStep(t, rec)
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	if err := step.Ensure(context.Background(), []string{"gamma.box.example.com"}); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	if len(rec.hosts) != 1 {
		t.Fatalf("wrote %+v, want gamma's host once", rec.hosts)
	}
	got := rec.hosts[0]
	if got.Host != "gamma.box.example.com" || got.Issuer != IssuerLetsEncrypt {
		t.Fatalf("host = %+v, want gamma's name and Let's Encrypt", got)
	}
	if !strings.Contains(out.String(), "tls: gamma.box.example.com resolves to 203.0.113.7: using Let's Encrypt") {
		t.Fatalf("apply did not explain the issuer:\n%s", out)
	}
}

// A name answering elsewhere can never issue, so the step stops before a write,
// names the record to add in both forms, and names the command to run again.
func TestTLSHostsStopsOnAMismatchBeforeWriting(t *testing.T) {
	rec := &hostRecorder{}
	step, _ := hostStep(t, rec)
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"198.51.100.9"},
	}}
	err := step.Ensure(context.Background(), []string{"gamma.box.example.com"})
	if err == nil {
		t.Fatal("the step wrote a host that answers somewhere else")
	}
	want := "app hosts: gamma.box.example.com resolves to 198.51.100.9, but box.example.com resolves to 203.0.113.7; " +
		"point gamma.box.example.com at the same addresses (a wildcard *.box.example.com, or gamma.box.example.com), then run box apply again"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant    %q", err, want)
	}
	if len(rec.hosts) != 0 {
		t.Fatalf("the step wrote %+v before stopping", rec.hosts)
	}
}

// A test box with no records is not a mismatch: self-signed, with the record.
func TestTLSHostsProceedsSelfSignedWhenNothingResolves(t *testing.T) {
	rec := &hostRecorder{}
	step, out := hostStep(t, rec)
	if err := step.Ensure(context.Background(), []string{"gamma.box.example.com"}); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	if len(rec.hosts) != 1 {
		t.Fatalf("wrote %+v, want gamma's host", rec.hosts)
	}
	got := rec.hosts[0]
	if got.Issuer != IssuerSelfSigned || !got.NoDNS {
		t.Fatalf("host = %+v, want self-signed because it has no record", got)
	}
	wantRecord := "add either a wildcard *.box.example.com or a record for gamma.box.example.com"
	if got.Record != wantRecord {
		t.Fatalf("record = %q, want %q", got.Record, wantRecord)
	}
	// One sentence naming the host, the record to add and the command to move it.
	wantLine := "gamma.box.example.com has no DNS record, so its certificate is self-signed; " +
		"add a record for it, then run box apply again to move it to Let's Encrypt"
	if !strings.Contains(out.String(), wantLine) {
		t.Fatalf("apply did not explain the self-signed certificate:\n%s", out)
	}
}

// A user's run cannot write a listener: it stops naming root's box init.
func TestTLSHostsStopsWhenThisIdentityCannotWriteTheListener(t *testing.T) {
	rec := &hostRecorder{}
	step, _ := hostStep(t, rec)
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	step.Allowed = func(context.Context) (bool, error) { return false, nil }
	err := step.Ensure(context.Background(), []string{"gamma.box.example.com"})
	if err == nil {
		t.Fatal("the step wrote a listener this identity may not write")
	}
	want := "gamma.box.example.com has no listener yet, and only root can write one: run `box init` as root for this seedbox, then apply again"
	if err.Error() != want {
		t.Fatalf("error = %q\nwant    %q", err, want)
	}
	if len(rec.hosts) != 0 {
		t.Fatalf("the step wrote %+v before stopping", rec.hosts)
	}
}

// An access question that cannot be answered is an error, not a "no".
func TestTLSHostsReportsAFailedAccessQuestion(t *testing.T) {
	rec := &hostRecorder{}
	step, _ := hostStep(t, rec)
	step.Allowed = func(context.Context) (bool, error) { return false, errors.New("the API server did not answer") }
	err := step.Ensure(context.Background(), []string{"gamma.box.example.com"})
	if err == nil || !strings.Contains(err.Error(), "the API server did not answer") {
		t.Fatalf("error = %v, want the failed review", err)
	}
	if len(rec.hosts) != 0 {
		t.Fatal("a failed access question still wrote a host")
	}
}

// The login's rule is written before any listener: a rule the running login has not
// read is a 403 for the host the step exists to serve.
func TestTLSHostsWritesTheLoginRuleBeforeTheListener(t *testing.T) {
	rec := &hostRecorder{}
	step, out := hostStep(t, rec)
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	var order []string
	var ruled []Host
	step.Rules = func(_ context.Context, hosts []Host) error {
		order = append(order, "rules")
		ruled = hosts
		return nil
	}
	step.Apply = func(ctx context.Context, hosts []Host) error {
		order = append(order, "host "+hosts[0].Host)
		return rec.apply(ctx, hosts)
	}
	if err := step.Ensure(context.Background(), []string{"gamma.box.example.com"}); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	if len(ruled) != 1 || ruled[0].Host != "gamma.box.example.com" {
		t.Fatalf("the rule was written for %+v, want gamma's name", ruled)
	}
	want := []string{"rules", "host gamma.box.example.com"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("order = %v, want the rule before the listener", order)
	}
}

// A failed rule write stops the step: a listener with no rule is the 403 this step
// prevents.
func TestTLSHostsStopsWhenTheLoginRuleCannotBeWritten(t *testing.T) {
	rec := &hostRecorder{}
	step, _ := hostStep(t, rec)
	step.Rules = func(context.Context, []Host) error {
		return errors.New("the login is not installed")
	}
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	err := step.Ensure(context.Background(), []string{"gamma.box.example.com"})
	if err == nil || !strings.Contains(err.Error(), "the login is not installed") {
		t.Fatalf("error = %v, want the rule write's failure", err)
	}
	if len(rec.hosts) != 0 {
		t.Fatalf("the step wrote %+v after the rule failed", rec.hosts)
	}
}

// A name answering elsewhere is refused before either write, so the login's
// configuration is untouched.
func TestTLSHostsDoesNotWriteARuleForANameItRefuses(t *testing.T) {
	rec := &hostRecorder{}
	step, _ := hostStep(t, rec)
	called := false
	step.Rules = func(context.Context, []Host) error {
		called = true
		return nil
	}
	step.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"198.51.100.9"},
	}}
	if err := step.Ensure(context.Background(), []string{"gamma.box.example.com"}); err == nil {
		t.Fatal("the step served a name that answers somewhere else")
	}
	if called {
		t.Fatal("the step wrote the login's configuration for a name it refused")
	}
}

// No names is not an error, and a host without an issuer is skipped.
func TestApplyHostsSkipsNamesWithoutAnIssuer(t *testing.T) {
	rec := &hostRecorder{}
	err := ApplyHosts(context.Background(), rec.apply, []Host{
		{Host: "", Issuer: IssuerSelfSigned},
		{Host: "gamma.box.example.com"},
		{Host: "gamma.box.example.com", Issuer: IssuerLetsEncrypt},
	})
	if err != nil {
		t.Fatalf("apply hosts: %v", err)
	}
	if len(rec.hosts) != 1 || rec.hosts[0].Issuer != IssuerLetsEncrypt {
		t.Fatalf("wrote %+v, want only the host with an issuer", rec.hosts)
	}
	if err := ApplyHosts(context.Background(), nil, []Host{{Host: "gamma.box.example.com", Issuer: IssuerSelfSigned}}); err == nil {
		t.Fatal("a step with nothing to write a host with was accepted")
	}
}

// Names with no record are one sentence naming all of them, not one per host.
func TestOneDNSWarningNamesEveryHost(t *testing.T) {
	rec := &hostRecorder{}
	step, out := hostStep(t, rec)
	hosts := []string{"gamma.box.example.com", "delta.box.example.com", "epsilon.box.example.com"}
	if err := step.Ensure(context.Background(), hosts); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	if got := strings.Count(out.String(), "has no DNS record"); got > 0 {
		t.Fatalf("a run with several host names printed the warning in the singular %d time(s):\n%s", got, out)
	}
	if got := strings.Count(out.String(), "have no DNS record"); got != 1 {
		t.Fatalf("the warning is printed %d times, want one for the whole run:\n%s", got, out)
	}
	want := "gamma.box.example.com, delta.box.example.com and epsilon.box.example.com have no DNS record, so their certificates are self-signed; add a record for each, then run box apply again to move them to Let's Encrypt"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("the one warning does not name every host:\n%s", out)
	}
	// Every name is written once, in one step.
	if len(rec.hosts) != len(hosts) {
		t.Fatalf("wrote %d hosts, want all %d", len(rec.hosts), len(hosts))
	}
}
