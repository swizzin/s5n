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

// The two issuer names tls.issuer accepts, and the ClusterIssuers root installs.
const (
	IssuerLetsEncrypt = "letsencrypt"
	IssuerSelfSigned  = "selfsigned"
)

// Resolver answers what a host name resolves to. An interface so init's decision is
// testable from a stub: a public answer, a private one, NXDOMAIN and a timeout are
// four lines and the same certificate for the last three.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// NetResolver is the host's own DNS, as any other program on the machine sees it.
type NetResolver struct{}

// LookupHost resolves through the system resolver.
func (NetResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// LookupTimeout bounds one resolution: a box with no DNS says so in five seconds,
// rather than hanging init.
const LookupTimeout = 5 * time.Second

// classifyAddresses is the decision as a pure function of what DNS answered: a routable
// address picks Let's Encrypt, a private, loopback, link-local or CGNAT one, or no
// answer, the self-signed issuer.
//
// It says nothing about the node's own address: a box behind NAT has a private node
// address and a public name that still validates.
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

// isPublic reports whether Let's Encrypt could validate an address: not loopback,
// private, link-local, multicast, unspecified, or carrier-grade NAT.
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

// hostAnswer is one name's resolution, kept apart from the lookup so one answer
// decides both the issuer and whether an app's name has a record, with no second
// query to disagree with the first.
type hostAnswer struct {
	addrs []string
	err   error
}

// errNoResolver is a build without a resolver, not a DNS answer: init says so and
// picks the safe certificate rather than inventing a lookup.
var errNoResolver = errors.New("no DNS resolver is available")

// detail is the resolver's own answer, for the run log: the screen says one short
// fact about a name that did not answer, and this is the rest.
func (a hostAnswer) detail(host string) string {
	if a.err == nil {
		return ""
	}
	return fmt.Sprintf("dns %s: %v", host, a.err)
}

// resolves reports whether the name answered with at least one address.
func (a hostAnswer) resolves() bool { return len(a.addrs) > 0 }

// addresses is the answer normalised for comparison: parsed canonically (203.0.113.7
// and 203.0.113.007 are one address), duplicates and unparsable entries dropped,
// sorted so one set has one spelling.
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

// ChooseIssuer is init's answer for one host name: tls.issuer when forced, else what
// the resolution says, with the sentence init prints so the operator sees why.
//
// Exported for root's host-CA step, which is not init but must agree with it: the
// anchor a self-signed box carries into its namespace is the certificate of the
// issuer this returns, and a second implementation would be a second answer.
func ChooseIssuer(ctx context.Context, resolver Resolver, host, forced string) (issuer, line string) {
	return issuerFor(host, resolveHost(ctx, resolver, host), forced)
}

// issuerFor is the decision over an answer already taken, so init resolves a name
// once for both the certificate and the app-host check.
func issuerFor(host string, ans hostAnswer, forced string) (issuer, line string) {
	forced = strings.TrimSpace(forced)
	switch forced {
	case IssuerLetsEncrypt:
		return IssuerLetsEncrypt, "tls.issuer=letsencrypt forces Let's Encrypt"
	case IssuerSelfSigned:
		return IssuerSelfSigned, "tls.issuer=selfsigned forces a self-signed certificate"
	case "":
	default:
		// The chart schema rejects this before init runs; keep the message honest.
		return IssuerSelfSigned, fmt.Sprintf("tls.issuer %q is not %s or %s: using a self-signed certificate", forced, IssuerLetsEncrypt, IssuerSelfSigned)
	}
	if errors.Is(ans.err, errNoResolver) {
		return IssuerSelfSigned, "no DNS resolver is available: using a self-signed certificate"
	}
	if ans.err != nil {
		// The resolver's own words go to the log (hostAnswer.detail); the screen says
		// "does not resolve" for every name that does not answer, which lets four
		// names be one row (rules 2 and 4).
		return IssuerSelfSigned, fmt.Sprintf("%s does not resolve: using a self-signed certificate", host)
	}
	issuer, why := classifyAddresses(ans.addrs)
	if issuer == IssuerLetsEncrypt {
		return issuer, fmt.Sprintf("%s %s: using Let's Encrypt", host, why)
	}
	return issuer, fmt.Sprintf("%s %s: using a self-signed certificate", host, why)
}

// FileResolver answers from a hosts-style file, because an e2e cannot write
// /etc/hosts: the test host's DNS is not the test's to change, and init's decision
// is what it must move from "no record" to "a public one" between runs. A name the
// file does not list is a not-found answer, so init's wording and decision match a
// real host.
type FileResolver struct{ Path string }

// LookupHost reads the file on every call, since a test changes the answer between
// runs and init keeps none: that is what lets a re-run move a host to Let's Encrypt.
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
