package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// The two issuer names tls.issuer accepts, and the ClusterIssuers root
// installs. They are the values a provider name in a file may hold.
const (
	IssuerLetsEncrypt = "letsencrypt"
	IssuerSelfSigned  = "selfsigned"
)

// Resolver answers what a host name resolves to. It is an interface so init's
// decision is testable from a stub: a public answer, a private one, NXDOMAIN
// and a timeout are four different lines and the same certificate for the last
// three.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// NetResolver is the real resolver: the host's own DNS, exactly as any other
// program on the machine sees it.
type NetResolver struct{}

// LookupHost resolves through the system resolver.
func (NetResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// LookupTimeout bounds one resolution. A box with no DNS at all should say so
// in five seconds, not hang init.
const LookupTimeout = 5 * time.Second

// classifyAddresses is the whole decision, as a pure function of what DNS
// answered: a globally routable address picks Let's Encrypt; a private,
// loopback, link-local or CGNAT address, or no answer at all, picks the
// self-signed issuer.
//
// It deliberately says nothing about the node's own address. A box behind NAT
// has a private node address and a public name that still validates at Let's
// Encrypt, so comparing the two would pick the wrong issuer for the common
// case.
func classifyAddresses(addrs []string) (issuer, why string) {
	var public []string
	var others []string
	for _, raw := range addrs {
		ip := net.ParseIP(strings.Trim(raw, "[]"))
		if ip == nil {
			continue
		}
		if isPublic(ip) {
			public = append(public, ip.String())
			continue
		}
		others = append(others, ip.String())
	}
	switch {
	case len(public) > 0:
		return IssuerLetsEncrypt, "resolves to " + strings.Join(public, ", ")
	case len(others) > 0:
		return IssuerSelfSigned, "resolves only to private addresses (" + strings.Join(others, ", ") + ")"
	default:
		return IssuerSelfSigned, "does not resolve to an address"
	}
}

// isPublic reports whether an address is one Let's Encrypt could validate:
// not loopback, private (RFC 1918 or IPv6 ULA), link-local, multicast,
// unspecified, or in the 100.64.0.0/10 carrier-grade NAT range.
func isPublic(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		// 100.64.0.0/10 (RFC 6598), which net.IP.IsPrivate does not cover.
		return false
	}
	return true
}

// hostAnswer is one name's resolution: the addresses it answered with, or why
// it answered none. It is kept apart from the lookup so the same answer decides
// both the issuer and whether an app's name has a record at all, without a
// second query that could disagree with the first.
type hostAnswer struct {
	addrs []string
	err   error
}

// errNoResolver is a build without a resolver (a test, or a host with no system
// resolver configured), not a DNS answer: init says so and picks the safe
// certificate rather than inventing a lookup.
var errNoResolver = errors.New("no DNS resolver is available")

// resolves reports whether the name answered with at least one address.
func (a hostAnswer) resolves() bool { return len(a.addrs) > 0 }

// addresses is the answer normalised for comparison: parsed addresses render
// canonically (203.0.113.7 and 203.0.113.007 are one address), duplicated and
// unparsable entries dropped, sorted so one set has one spelling.
func (a hostAnswer) addresses() []string {
	seen := make(map[string]bool, len(a.addrs))
	out := make([]string, 0, len(a.addrs))
	for _, raw := range a.addrs {
		ip := net.ParseIP(strings.Trim(raw, "[]"))
		if ip == nil {
			continue
		}
		s := ip.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// resolveHost asks the resolver, with init's one bound on the question.
func resolveHost(ctx context.Context, resolver Resolver, host string) hostAnswer {
	if resolver == nil {
		return hostAnswer{err: errNoResolver}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	addrs, err := resolver.LookupHost(lookupCtx, host)
	return hostAnswer{addrs: addrs, err: err}
}

// chooseIssuer is init's answer for one host name: the forced value when
// tls.issuer names one, otherwise what the resolution says. It returns the
// issuer and the sentence init prints, so the operator sees why.
func chooseIssuer(ctx context.Context, resolver Resolver, host, forced string) (issuer, line string) {
	return issuerFor(host, resolveHost(ctx, resolver, host), forced)
}

// issuerFor is the decision over an answer already taken, so init can resolve
// one name once and use the answer for both the certificate and the app-host
// check.
func issuerFor(host string, ans hostAnswer, forced string) (issuer, line string) {
	forced = strings.TrimSpace(forced)
	switch forced {
	case IssuerLetsEncrypt:
		return IssuerLetsEncrypt, "tls.issuer=letsencrypt forces Let's Encrypt"
	case IssuerSelfSigned:
		return IssuerSelfSigned, "tls.issuer=selfsigned forces a self-signed certificate"
	case "":
	default:
		// The chart schema rejects this before init runs; keep the message
		// honest if it ever gets here anyway.
		return IssuerSelfSigned, fmt.Sprintf("tls.issuer %q is not %s or %s: using a self-signed certificate", forced, IssuerLetsEncrypt, IssuerSelfSigned)
	}
	if errors.Is(ans.err, errNoResolver) {
		return IssuerSelfSigned, "no DNS resolver is available: using a self-signed certificate"
	}
	if ans.err != nil {
		return IssuerSelfSigned, fmt.Sprintf("%s does not resolve (%v): using a self-signed certificate", host, ans.err)
	}
	issuer, why := classifyAddresses(ans.addrs)
	if issuer == IssuerLetsEncrypt {
		return issuer, fmt.Sprintf("%s %s: using Let's Encrypt", host, why)
	}
	return issuer, fmt.Sprintf("%s %s: using a self-signed certificate", host, why)
}

// FileResolver answers from a hosts-style file: one address and one or more
// names per line, the format every system resolver already reads. It exists
// because an e2e cannot write /etc/hosts: the test host's own DNS is not the
// test's to change, and init's certificate decision is exactly what the e2e has
// to move from "no record" to "a public one" between two runs. A name the file
// does not list is a not-found answer, the way a resolver answers a name with no
// record, so init's wording and its decision are the same as on a real host.
type FileResolver struct{ Path string }

// LookupHost reads the file on every call: a test changes the answer between
// runs, and init keeps no earlier answer, which is what makes a re-run move a
// host from self-signed to Let's Encrypt.
func (f FileResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, name := range fields[1:] {
			if strings.EqualFold(name, host) {
				addrs = append(addrs, fields[0])
				break
			}
		}
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}
