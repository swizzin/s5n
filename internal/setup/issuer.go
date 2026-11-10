package setup

import (
	"context"
	"fmt"
	"net"
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

// chooseIssuer is init's answer for one host name: the forced value when
// tls.issuer names one, otherwise what the resolution says. It returns the
// issuer and the sentence init prints, so the operator sees why.
func chooseIssuer(ctx context.Context, resolver Resolver, host, forced string) (issuer, line string) {
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
	if resolver == nil {
		return IssuerSelfSigned, "no DNS resolver is available: using a self-signed certificate"
	}
	lookupCtx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	addrs, err := resolver.LookupHost(lookupCtx, host)
	if err != nil {
		return IssuerSelfSigned, fmt.Sprintf("%s does not resolve (%v): using a self-signed certificate", host, err)
	}
	issuer, why := classifyAddresses(addrs)
	if issuer == IssuerLetsEncrypt {
		return issuer, fmt.Sprintf("%s %s: using Let's Encrypt", host, why)
	}
	return issuer, fmt.Sprintf("%s %s: using a self-signed certificate", host, why)
}
