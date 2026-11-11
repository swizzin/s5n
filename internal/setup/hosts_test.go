package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
)

// testDomain is the box's own name in every case here; the app hosts hang under
// it.
const testDomain = "box.example.com"

// hostRecorder is the per-host write a TLSHosts step is given: what names it
// was asked for, with which issuer, and in what order.
type hostRecorder struct {
	hosts []Host
	err   error
}

func (r *hostRecorder) apply(_ context.Context, h Host) error {
	if r.err != nil {
		return r.err
	}
	r.hosts = append(r.hosts, h)
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

// The apply step with matching answers: both names answered where the domain
// did, so both are written, each with the issuer its own answer picked.
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
	if got.Host != "gamma.box.example.com" || got.App != "gamma" || got.Issuer != IssuerLetsEncrypt {
		t.Fatalf("host = %+v, want gamma's name, its app and Let's Encrypt", got)
	}
	if !strings.Contains(out.String(), "tls: gamma.box.example.com resolves to 203.0.113.7: using Let's Encrypt") {
		t.Fatalf("apply did not explain the issuer:\n%s", out)
	}
}

// A name that answers somewhere else can never get a certificate through this
// box, so the step stops before a single write and names the record to add in
// both forms — and names the command that has to run again, which is apply's.
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

// A test box with no records at all is not a mismatch: the step proceeds
// self-signed and says so, with the record to add.
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
	wantLine := "gamma.box.example.com has no DNS record, so its certificate is self-signed; " + wantRecord +
		", then run box apply again to move it to Let's Encrypt"
	if !strings.Contains(out.String(), wantLine) {
		t.Fatalf("apply did not explain the self-signed certificate:\n%s", out)
	}
}

// A user's run cannot write a listener, so it stops before a write naming the
// command that can: root's box init for this seedbox.
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

// An access question that cannot be answered is an error, not a "no": a run
// that cannot tell whether it may write has to stop rather than guess.
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

// ApplyHosts is the shared write: no names is not an error, and a host without
// an issuer is skipped rather than written with an empty one.
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
