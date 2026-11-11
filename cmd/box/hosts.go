package main

import (
	"context"

	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// certificateStep is one host's TLS: its port-80 listener, its Certificate and,
// once that is Ready, its HTTPS listener and the redirect. Both box commands
// write a host through this one function, so init's names and an apply's new
// app host cannot be served differently.
func certificateStep(kubeconfig string, out say.Reporter) setup.HostFunc {
	return func(ctx context.Context, h setup.Host) error {
		cert := &platform.CertificateDecision{
			Host:   h.Host,
			Issuer: h.Issuer,
			Open:   func() (platform.CertificateCluster, error) { return platform.NewCluster(kubeconfig) },
			Say:    out,
		}
		_, err := cert.Apply(ctx)
		return err
	}
}

// applyHosts is `box apply`'s per-host TLS step (docs/box.md, "No run leaves an
// app host without its listener"): the shared setup decision over the
// platform's writes, plus the two questions only a cluster can answer.
//
// Missing asks whether each name already has its listener and Certificate, so
// apply does this work only for a host it is about to serve and nothing has
// answered on before. Ensure asks whether this identity may write them at all,
// which is what tells a user's run to ask root instead of failing on its first
// write.
type applyHosts struct {
	kubeconfig string
	out        say.Reporter
	resolver   setup.Resolver
}

// cluster opens the client for one question. It is lazy the way every other
// client box opens is: a run refused before this point never contacts the
// cluster.
func (a applyHosts) cluster() (*platform.Kubernetes, error) {
	return platform.NewCluster(a.kubeconfig)
}

// Missing reads the Gateway and the Certificates: a host with neither is one
// this run has to write.
func (a applyHosts) Missing(ctx context.Context, plan release.HostPlan) ([]string, error) {
	if len(plan.Hosts) == 0 {
		return nil, nil
	}
	cl, err := a.cluster()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, host := range plan.Hosts {
		served, err := cl.HostServed(ctx, host)
		if err != nil {
			return nil, err
		}
		if !served {
			missing = append(missing, host)
		}
	}
	return missing, nil
}

// Ensure runs the step: every name is DNS-checked against the box's domain
// before anything is written, then each gets its listener and Certificate.
func (a applyHosts) Ensure(ctx context.Context, plan release.HostPlan) error {
	cl, err := a.cluster()
	if err != nil {
		return err
	}
	step := &setup.TLSHosts{
		Domain:   plan.Domain,
		Forced:   plan.Forced,
		Command:  "apply",
		Resolver: a.resolver,
		Allowed:  func(ctx context.Context) (bool, error) { return platform.HostAccess(ctx, cl) },
		Apply:    certificateStep(a.kubeconfig, a.out),
		Say:      a.out,
	}
	return step.Ensure(ctx, plan.Hosts)
}
