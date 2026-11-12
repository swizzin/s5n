package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/login"
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

// applyHosts is `box apply`'s per-host step (docs/box.md, "No run leaves an
// app host without its listener"): the shared setup decision over the
// platform's writes, plus the three questions only a cluster can answer.
//
// Missing asks whether each name is served at all -- its login rule, its
// listener and its Certificate -- so apply does this work only for a host
// nothing has answered on before. Ensure asks whether this identity may write
// them at all, which is what tells a user's run to ask root instead of failing
// on its first write. Rules writes the login's half through the login's own
// writer, so the rule an apply adds is the rule init adds.
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

// Missing reads the Gateway, the Certificates and the login's configuration: a
// host without all three is one this run has to write.
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
// before anything is written, then the login's rule for them, then each one's
// listener and Certificate.
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
		Rules:    a.rules(cl, plan),
		Say:      a.out,
	}
	return step.Ensure(ctx, plan.Hosts)
}

// rules is the login's half of the per-host step: the access_control rule for
// every name this run serves, written by internal/login from the inputs init
// gives it. The rule is what the Gateway asks the login about, so a host
// without one answers 403 whatever its listener and Certificate say.
func (a applyHosts) rules(cl login.Cluster, plan release.HostPlan) setup.RuleFunc {
	return func(ctx context.Context, hosts []setup.Host) error {
		names := make([]string, 0, len(hosts))
		for _, h := range hosts {
			names = append(names, h.Host)
		}
		d := &login.Decision{
			Open: func() (login.Cluster, error) { return cl, nil },
			Say:  a.out,
		}
		_, err := d.EnsureRules(ctx, login.RulesOptions{
			Host:   plan.Domain,
			Login:  plan.Rule.Login,
			Hosts:  names,
			Image:  plan.Rule.Image,
			Values: plan.Rule.Values,
			Hosted: plan.Rule.Hosted,
		})
		if err != nil {
			return fmt.Errorf("the login's rule for %s: %w", strings.Join(names, ", "), err)
		}
		return nil
	}
}

// Pending reports whether the login's page differs from the cluster's, and
// Reconcile writes it when it does. They are the release.Login half of this
// step, and they are separate from Missing and Ensure because none of it is per
// host: a theme is a new face for one page, not a name that needs a listener, a
// Certificate and a DNS record.
//
// Both are root's. The login serves every seedbox on the host, so it is written
// once for the host and its theme is root's `theme:`, not a user's: an identity
// that cannot apply a ConfigMap in the platform's namespace gets no step at all
// rather than a refusal in the middle of its own apply. That is also why a
// `theme:` in a user's values file does nothing to the login -- it is the
// palette that user's own box draws its dialogs in, which box reads from the
// file it was started with (cmd/box/settings.go), and the chart renders nothing
// from it.
func (a applyHosts) Pending(ctx context.Context, plan release.HostPlan) (bool, error) {
	d, ok, err := a.loginStep(ctx, plan)
	if err != nil || !ok {
		return false, err
	}
	return d.Pending(ctx, loginRules(plan))
}

// Reconcile is the write half, and it applies only when the rendering differs:
// a run whose login is already the one the values name writes nothing and rolls
// nothing.
func (a applyHosts) Reconcile(ctx context.Context, plan release.HostPlan) error {
	d, ok, err := a.loginStep(ctx, plan)
	if err != nil || !ok {
		return err
	}
	_, err = d.Reconcile(ctx, loginRules(plan))
	return err
}

// loginStep is the login as this identity may write it. A box whose values name
// no domain has no portal to draw, and one whose cluster this identity may not
// write the login in is not this run's to reconcile.
func (a applyHosts) loginStep(ctx context.Context, plan release.HostPlan) (*login.Decision, bool, error) {
	if plan.Domain == "" || plan.Rule.Login == "" {
		return nil, false, nil
	}
	cl, err := a.cluster()
	if err != nil {
		return nil, false, err
	}
	ok, err := login.MayWrite(ctx, cl)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &login.Decision{
		Open: func() (login.Cluster, error) { return cl, nil },
		Say:  a.out,
	}, true, nil
}

// loginRules is what internal/login needs for both halves: the box's domain,
// the account, the image the Deployment runs and the document the rules come
// from. Hosts is the names the login's configuration covers, for the one line a
// rule write prints.
func loginRules(plan release.HostPlan) login.RulesOptions {
	return login.RulesOptions{
		Host:   plan.Domain,
		Login:  plan.Rule.Login,
		Hosts:  []string{plan.Domain},
		Image:  plan.Rule.Image,
		Values: plan.Rule.Values,
		Hosted: plan.Rule.Hosted,
	}
}
