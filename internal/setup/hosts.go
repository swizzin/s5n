package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/say"
)

// HostsFunc writes the decided hosts of one run: each host's listener and
// Certificate, one wait over all of them, then the hosts that came Ready. cmd/box
// builds it over platform.Certificates; this package only decides which names with
// which issuer, and refuses the names a certificate can never issue.
type HostsFunc func(ctx context.Context, hosts []Host) error

// RuleFunc writes the login's access_control rule for the hosts a step serves: the
// third thing an app host needs beside its listener and Certificate (docs/box.md).
// It is separate from HostsFunc because the login keeps one configuration for the
// whole box, so the rules are written once, by the login's own writer. Nil means
// the caller already wrote them: init installs the login before its per-host step.
type RuleFunc func(ctx context.Context, hosts []Host) error

// ApplyHosts is the one per-host write both commands use: every decided host,
// in order, handed to one function. init runs it after its hand-over, apply from
// TLSHosts.Ensure before the release moves. A name without a host or an issuer is
// skipped: there is nothing to build a Certificate from.
func ApplyHosts(ctx context.Context, apply HostsFunc, hosts []Host) error {
	if apply == nil {
		return errors.New("no per-host step: box has nothing that writes a listener and a certificate")
	}
	decided := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		if h.Host == "" || h.Issuer == "" {
			continue
		}
		decided = append(decided, h)
	}
	if len(decided) == 0 {
		return nil
	}
	return apply(ctx, decided)
}

// TLSHosts is root's per-host step: an app served at its own host gets the login's rule,
// its listener and its Certificate before anything else moves (docs/box.md). It makes
// init's decision over the same DNS check, so a name answering elsewhere stops before a
// write; two empty answers are a test box. The caller finds the names, which is why init
// and apply cannot disagree.
type TLSHosts struct {
	// Domain is the box's own host name, which every app host is compared with.
	Domain string
	// Forced is tls.issuer ("letsencrypt"/"selfsigned"), or empty to decide from
	// each name's answer.
	Forced string
	// Decided is the issuer this run already chose, by host, so the Certificate uses
	// the same issuer the plan compared it with. Those names are not resolved again;
	// a name not here is decided from its own DNS answer, as a bare apply does.
	Decided map[string]string
	// Command is what a stop tells the operator to run again: "init" or "apply".
	Command string
	// Resolver answers what a name resolves to; nil answers "nothing", the
	// self-signed path.
	Resolver Resolver
	// Allowed answers whether this identity may write a listener and a Certificate in
	// the Gateway's namespace. A user's kubeconfig may not, which is a stop naming
	// root's command rather than a failed write. Nil means yes (init runs as root).
	Allowed func(ctx context.Context) (bool, error)
	// Apply writes every host this step decided, in one run.
	Apply HostsFunc
	// Rules writes the login's rule for the hosts this step serves, once for all of
	// them and before the first listener: the configuration is one file, and a stop
	// after it leaves only rules the next run rewrites from the same values.
	Rules RuleFunc
	Say   say.Reporter
}

func (t *TLSHosts) report() say.Reporter { return say.Or(t.Say, nil) }

// Ensure decides and writes every host it is given. The DNS check covers all of
// them before the first write, so a mismatch on the second name leaves the first
// untouched too.
func (t *TLSHosts) Ensure(ctx context.Context, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	domain := strings.TrimSpace(t.Domain)
	if domain == "" {
		return errors.New("no domain: an app host is a name under this host's own")
	}
	r := t.report()
	// The box's own name is resolved only for a host this run has not decided:
	// resolving again could only disagree with the decision the plan showed.
	var domAnswer hostAnswer
	domResolved := false
	dom := func() hostAnswer {
		if !domResolved {
			domAnswer = resolveHost(ctx, t.Resolver, domain)
			domResolved = true
		}
		return domAnswer
	}
	decided := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		if issuer, ok := t.Decided[host]; ok && strings.TrimSpace(issuer) != "" {
			decided = append(decided, Host{
				Host:   host,
				App:    appName(host, domain),
				Issuer: issuer,
			})
			continue
		}
		ans := resolveHost(ctx, t.Resolver, host)
		if err := appHostProblem(domain, dom(), host, ans, t.Command); err != nil {
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
	// The permission question is last of the reads: a wrong DNS answer is the
	// operator's to fix whoever runs box, while a user's run cannot write the
	// listener at all and must hear whose job it is before any certificate.
	if t.Allowed != nil {
		ok, err := t.Allowed(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return t.noAccess(hosts)
		}
	}
	// The login's rule is written first: it is one file for the box, and neither a
	// listener nor a certificate is worth writing for a name the login refuses with
	// a 403.
	if t.Rules != nil {
		if err := t.Rules(ctx, decided); err != nil {
			return err
		}
	}
	r.Phase("certificate")
	if warning, ok := NoDNSWarning(decided, t.Command); ok {
		r.Warn(warning)
	}
	return ApplyHosts(ctx, t.Apply, decided)
}

// noAccess is the stop a user's run gets: the listener and Certificate are root's,
// so naming the command that can write them is the whole fix.
func (t *TLSHosts) noAccess(hosts []string) error {
	return fmt.Errorf("%s has no listener yet, and only root can write one: run `box init` as root for this seedbox, then apply again", strings.Join(hosts, ", "))
}

// NoDNSWarning is the one sentence a run whose names have no DNS record gets: every
// missing name, why those certificates are self-signed, and the command that moves
// them. init's card and apply's step print the same words for the same fact.
func NoDNSWarning(hosts []Host, command string) (string, bool) {
	var names []string
	for _, h := range hosts {
		if h.NoDNS && strings.TrimSpace(h.Host) != "" {
			names = append(names, h.Host)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	joined := joinNames(names)
	if len(names) == 1 {
		return fmt.Sprintf("%s has no DNS record, so its certificate is self-signed; add a record for it, then run box %s again to move it to Let's Encrypt", joined, command), true
	}
	return fmt.Sprintf("%s have no DNS record, so their certificates are self-signed; add a record for each, then run box %s again to move them to Let's Encrypt", joined, command), true
}

// joinNames is "a", "a and b", "a, b and c": the way a sentence lists names.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// appName is the app a host belongs to: gamma for gamma.box.example.com, "" for a
// name outside the domain.
func appName(host, domain string) string {
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// appHostRecord is the fix for an app host with no record, in both forms: a wildcard
// covers every app in one line, a per-name record is for whoever cannot use one.
func appHostRecord(domain, host string) string {
	return fmt.Sprintf("add either a wildcard *.%s or a record for %s", domain, host)
}

// certificatesRow is the one row a run's host certificates fold into.
const certificatesRow = "certificates"

// tlsRow is one host's certificate decision as a row: the issuer as a word, then
// the reason, with the name not said twice.
func tlsRow(h Host) say.Row {
	issuer := "self-signed"
	if h.Issuer == IssuerLetsEncrypt {
		issuer = "Let's Encrypt"
	}
	why := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h.Why), h.Host))
	why = strings.TrimSpace(strings.TrimPrefix(why, ":"))
	// The issuer is already a column, so a "using ..." tail is that fact twice.
	for _, tail := range []string{": using Let's Encrypt", ": using a self-signed certificate"} {
		why = strings.TrimSuffix(why, tail)
	}
	row := say.Row{Mark: say.MarkNote, Name: h.Host, Facts: []string{issuer}}
	if why != "" {
		row.Facts = append(row.Facts, why)
	}
	return row
}
