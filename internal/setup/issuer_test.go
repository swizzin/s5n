package setup

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyAddresses(t *testing.T) {
	cases := []struct {
		name   string
		addrs  []string
		issuer string
	}{
		{"public IPv4", []string{"203.0.113.10"}, IssuerLetsEncrypt},
		{"public IPv6", []string{"2606:4700:4700::1111"}, IssuerLetsEncrypt},
		{"RFC 1918 10/8", []string{"10.0.0.5"}, IssuerSelfSigned},
		{"RFC 1918 172.16/12", []string{"172.16.4.2"}, IssuerSelfSigned},
		{"RFC 1918 192.168/16", []string{"192.168.1.10"}, IssuerSelfSigned},
		{"CGNAT 100.64/10", []string{"100.64.0.1"}, IssuerSelfSigned},
		{"CGNAT upper half", []string{"100.127.255.254"}, IssuerSelfSigned},
		{"just outside CGNAT", []string{"100.128.0.1"}, IssuerLetsEncrypt},
		{"IPv6 ULA fd00::/8", []string{"fd00::1"}, IssuerSelfSigned},
		{"loopback", []string{"127.0.0.1", "::1"}, IssuerSelfSigned},
		{"link-local", []string{"169.254.10.1", "fe80::1"}, IssuerSelfSigned},
		{"mixed public and private", []string{"10.0.0.5", "203.0.113.10"}, IssuerLetsEncrypt},
		{"no addresses", nil, IssuerSelfSigned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer, why := classifyAddresses(tc.addrs)
			if issuer != tc.issuer {
				t.Fatalf("issuer = %q (%s), want %q", issuer, why, tc.issuer)
			}
			if why == "" {
				t.Fatal("no reason")
			}
		})
	}
}

func TestChooseIssuerResolves(t *testing.T) {
	cases := []struct {
		name   string
		addrs  []string
		err    error
		issuer string
		says   []string
	}{
		{
			name:   "a public name gets Let's Encrypt",
			addrs:  []string{"203.0.113.10"},
			issuer: IssuerLetsEncrypt,
			says:   []string{"box.example.test resolves to 203.0.113.10", "Let's Encrypt"},
		},
		{
			name:   "a private name is self-signed",
			addrs:  []string{"192.168.1.10"},
			issuer: IssuerSelfSigned,
			says:   []string{"resolves only to private addresses (192.168.1.10)", "self-signed"},
		},
		{
			name:   "NXDOMAIN is self-signed",
			err:    errors.New("lookup box.internal.example: no such host"),
			issuer: IssuerSelfSigned,
			says:   []string{"does not resolve", "no such host", "self-signed"},
		},
		{
			name:   "a resolver timeout is self-signed",
			err:    context.DeadlineExceeded,
			issuer: IssuerSelfSigned,
			says:   []string{"does not resolve", "deadline exceeded"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := stubResolver{
				addrs: map[string][]string{"box.example.test": tc.addrs},
				errs:  map[string]error{"box.example.test": tc.err},
			}
			issuer, line := chooseIssuer(context.Background(), r, "box.example.test", "")
			if issuer != tc.issuer {
				t.Fatalf("issuer = %q (%s), want %q", issuer, line, tc.issuer)
			}
			for _, want := range tc.says {
				if !strings.Contains(line, want) {
					t.Errorf("line %q does not say %q", line, want)
				}
			}
		})
	}
}

// tls.issuer forces either issuer, whatever the name resolves to, and says so.
func TestChooseIssuerForced(t *testing.T) {
	r := stubResolver{addrs: map[string][]string{"box.example.test": {"203.0.113.10"}}}

	issuer, line := chooseIssuer(context.Background(), r, "box.example.test", IssuerSelfSigned)
	if issuer != IssuerSelfSigned || !strings.Contains(line, "tls.issuer=selfsigned forces") {
		t.Fatalf("forced selfsigned = %q (%s)", issuer, line)
	}
	issuer, line = chooseIssuer(context.Background(), r, "lan.example.test", IssuerLetsEncrypt)
	if issuer != IssuerLetsEncrypt || !strings.Contains(line, "tls.issuer=letsencrypt forces") {
		t.Fatalf("forced letsencrypt = %q (%s)", issuer, line)
	}
}

func TestChooseIssuerWithoutAResolver(t *testing.T) {
	issuer, line := chooseIssuer(context.Background(), nil, "box.example.test", "")
	if issuer != IssuerSelfSigned || !strings.Contains(line, "no DNS resolver") {
		t.Fatalf("issuer = %q (%s)", issuer, line)
	}
}

// The e2e's resolver: a hosts-style file read on every call, so changing the
// file between two init runs is what a new DNS record looks like, and a name
// the file does not list is a not-found answer rather than a mismatch.
func TestFileResolverReadsTheAnswerEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns")
	if err := os.WriteFile(path, []byte("# test\n203.0.113.10 box.example.test seerr.box.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := FileResolver{Path: path}
	addrs, err := r.LookupHost(context.Background(), "seerr.box.example.test")
	if err != nil || len(addrs) != 1 || addrs[0] != "203.0.113.10" {
		t.Fatalf("lookup = %v, %v", addrs, err)
	}
	if _, err := r.LookupHost(context.Background(), "plex.box.example.test"); err == nil {
		t.Fatal("a name the file does not list resolved")
	} else if err.(*net.DNSError) == nil || !err.(*net.DNSError).IsNotFound {
		t.Fatalf("error = %T %v, want a not-found DNS answer", err, err)
	}
	if err := os.WriteFile(path, []byte("198.51.100.4 box.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if issuer, line := chooseIssuer(context.Background(), r, "seerr.box.example.test", ""); issuer != IssuerSelfSigned || !strings.Contains(line, "does not resolve") {
		t.Fatalf("a removed record still resolved: %q (%s)", issuer, line)
	}
}
