// Command issue-cert writes one host's Certificate and its Gateway listeners
// the way `box init` does after an apply: the same platform.CertificateDecision,
// over an explicit kubeconfig. It exists for the chart's e2e, which installs
// with Helm and has no `box init` run to carry the per-host objects; the
// self-signed and Pebble-issued paths are proven there, and box's own e2e
// proves the same code through init.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/say"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "the cluster to write to")
	host := flag.String("host", "", "the host name to certify (a Certificate, two listeners and a redirect)")
	issuer := flag.String("issuer", "letsencrypt", "letsencrypt or selfsigned")
	flag.Parse()

	if *kubeconfig == "" || *host == "" {
		fmt.Fprintln(os.Stderr, "issue-cert: --kubeconfig and --host are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dec := &platform.CertificateDecision{
		Host:   *host,
		Issuer: *issuer,
		Open:   func() (platform.CertificateCluster, error) { return platform.NewCluster(*kubeconfig) },
		Say:    &say.Plain{W: os.Stdout, Err: os.Stderr},
	}
	res, err := dec.Apply(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "issue-cert: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("certificate %s (%s): ready=%t https=%t redirect=%t\n", res.Name, res.Issuer, res.Ready, res.HTTPS, res.Redirect)
}
