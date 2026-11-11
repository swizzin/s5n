package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/say"
)

// HostFunc writes one host's listener and Certificate and waits for it. cmd/box
// builds it over platform.CertificateDecision; this package only decides which
// name, with which issuer, and refuses the names a certificate can never issue.
type HostFunc func(ctx context.Context, h Host) error

// ApplyHosts is the one per-host write both box commands use: every decided
// host, in order, through one function. init runs it after its hand-over;
// apply runs it from TLSHosts.Ensure, before the release moves.
func ApplyHosts(ctx context.Context, apply HostFunc, hosts []Host) error {
	if apply == nil {
		return errors.New("no per-host step: box has nothing that writes a listener and a certificate")
	}
	for _, h := range hosts {
		if h.Host == "" || h.Issuer == "" {
			continue
		}
		if err := apply(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// TLSHosts is root's per-host TLS step for names a run is about to serve: an
// app served at a host of its own gets that host's listener and Certificate
// before anything else moves (docs/box.md, "No run leaves an app host without
// its listener").
//
// It is the same decision init makes for every name, over the same DNS check:
// a name that does not answer where the box's domain does can never issue, so
// the run stops here naming the record to add, before a write. Both names
// answering nothing is a test box and proceeds self-signed.
//
// The cluster read that finds the missing hosts is the caller's: this type is
// given the names, and it is the reason init and apply cannot disagree about
// what a host needs.
type TLSHosts struct {
	// Domain is the box's own host name, the answer every app host is
	// compared with.
	Domain string
	// Forced is tls.issuer as the files say it ("letsencrypt"/"selfsigned"), or
	// empty to decide from each name's answer.
	Forced string
	// Command is the box command a stop tells the operator to run again, so the
	// message names a command that exists: "init" or "apply".
	Command string
	// Resolver answers what a name resolves to. Nil answers "nothing", which is
	// the self-signed path.
	Resolver Resolver
	// Allowed answers whether this identity may write a listener and a
	// Certificate in the Gateway's namespace. A user's kubeconfig may not, and
	// that is a stop naming root's command rather than a failed write. Nil
	// means yes (init runs as root).
	Allowed func(ctx context.Context) (bool, error)
	// Apply writes one host.
	Apply HostFunc
	Say   say.Reporter
}

func (t *TLSHosts) report() say.Reporter { return say.Or(t.Say, nil) }

// Ensure decides and writes every host it is given. The DNS check runs for all
// of them before the first write, so a mismatch on the second name leaves the
// first untouched as well.
func (t *TLSHosts) Ensure(ctx context.Context, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	domain := strings.TrimSpace(t.Domain)
	if domain == "" {
		return errors.New("no domain: an app host is a name under the box's own")
	}
	r := t.report()
	domAnswer := resolveHost(ctx, t.Resolver, domain)
	decided := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		ans := resolveHost(ctx, t.Resolver, host)
		if err := appHostProblem(domain, domAnswer, host, ans, t.Command); err != nil {
			return err
		}
		issuer, why := issuerFor(host, ans, t.Forced)
		h := Host{
			Host:   host,
			App:    appName(host, domain),
			Issuer: issuer,
			Why:    why,
			NoDNS:  !ans.resolves() && issuer == IssuerSelfSigned,
		}
		if h.NoDNS {
			h.Record = appHostRecord(domain, host)
		}
		r.Line("tls: " + why)
		decided = append(decided, h)
	}
	// The permission question is last of the reads: a name that answers
	// somewhere else is the operator's DNS to fix whoever runs the box, while a
	// user's run cannot write the listener at all and has to be told whose job
	// that is before it gets as far as a certificate.
	if t.Allowed != nil {
		ok, err := t.Allowed(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return t.noAccess(hosts)
		}
	}
	r.Phase("certificate")
	for _, h := range decided {
		if h.NoDNS {
			r.Warn(NoDNSWarning(h, t.Command))
		}
	}
	return ApplyHosts(ctx, t.Apply, decided)
}

// noAccess is the stop a user's run gets: the listener and the Certificate are
// root's, the way the grant keeps ports and images there, so naming the command
// that can write them is the whole fix.
func (t *TLSHosts) noAccess(hosts []string) error {
	return fmt.Errorf("%s has no listener yet, and only root can write one: run `box init` as root for this seedbox, then apply again", strings.Join(hosts, ", "))
}

// NoDNSWarning is the one sentence a host with no DNS record gets: why its
// certificate is self-signed, the record to add, and the command that moves it
// once the record exists. init's final card and apply's step print the same
// words, because they are the same fact.
func NoDNSWarning(h Host, command string) string {
	return fmt.Sprintf("%s has no DNS record, so its certificate is self-signed; %s, then run box %s again to move it to Let's Encrypt", h.Host, h.Record, command)
}

// appName is the app a host belongs to, for a name under domain: gamma for
// gamma.box.example.com. A name that is not under the domain keeps "".
func appName(host, domain string) string {
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// appHostRecord is the fix an app host with no record names, in both forms: a
// wildcard record covers every app in one line, and a per-name record is what
// an operator who cannot use a wildcard needs.
func appHostRecord(domain, host string) string {
	return fmt.Sprintf("add either a wildcard *.%s or a record for %s", domain, host)
}
