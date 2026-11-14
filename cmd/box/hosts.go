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

// certificateStep is the run's TLS: every host's port-80 listener and
// Certificate first, one wait for all of them, then the HTTPS listener and the
// redirect for each host that is Ready. Both box commands write their hosts
// through this one function, so init's names and an apply's new app host cannot
// be served differently.
func certificateStep(kubeconfig string, out say.Reporter) setup.HostsFunc {
	return func(ctx context.Context, hosts []setup.Host) error {
		reqs := make([]platform.CertificateRequest, 0, len(hosts))
		for _, h := range hosts {
			reqs = append(reqs, platform.CertificateRequest{Host: h.Host, Issuer: h.Issuer})
		}
		certs := &platform.Certificates{
			Hosts: reqs,
			Open:  func() (platform.CertificateCluster, error) { return platform.NewCluster(kubeconfig) },
			Say:   out,
		}
		_, err := certs.Apply(ctx)
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
	// installs says this run installs the login after the confirm: `box init`
	// does, and its apply plans before that. A cluster without a login is then
	// a page this run will write, not a refusal.
	installs bool
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

// Stale is Missing's other half: the hosts that answer already but whose
// Certificate is not the one this run would issue -- the issuer changed under
// it, or a self-signed one is not on the box root. Only a run that owns the
// whole box asks: `box init` re-issues, a bare apply leaves a served host
// alone.
func (a applyHosts) Stale(ctx context.Context, plan release.HostPlan) ([]string, error) {
	if len(plan.Hosts) == 0 {
		return nil, nil
	}
	cl, err := a.cluster()
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, host := range plan.Hosts {
		// The issuer this run decided for this name. Forced is only the file's
		// answer, and it is empty on the run whose issuer came from DNS --
		// the run that can find a Certificate naming an issuer it no longer
		// wants.
		want := plan.Issuers[host]
		if want == "" {
			want = plan.Forced
		}
		needs, err := platform.CertificateNeedsWork(ctx, cl, host, want)
		if err != nil {
			return nil, err
		}
		if needs {
			stale = append(stale, host)
		}
	}
	return stale, nil
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
		Decided:  plan.Issuers,
		Command:  "apply",
		Resolver: a.resolver,
		Allowed:  func(ctx context.Context) (bool, error) { return platform.HostAccess(ctx, cl) },
		Apply:    certificateStep(a.kubeconfig, a.out),
		Rules:    a.rules(cl, plan),
		Say:      a.out,
	}
	names := plan.Hosts
	if len(plan.All) > 0 {
		// Every name this run decided, not only the ones nothing answers on:
		// init re-issues a self-signed host under the box root, and that is a
		// host the plan has nothing new to show for.
		names = plan.All
	}
	return step.Ensure(ctx, names)
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
			Host:      plan.Domain,
			Login:     plan.Rule.Login,
			Hosts:     names,
			Image:     plan.Rule.Image,
			Values:    plan.Rule.Values,
			Hosted:    plan.Rule.Hosted,
			OwnLogin:  plan.Rule.OwnLogin,
			Namespace: plan.Rule.Namespace,
			OIDC:      plan.Rule.OIDC,
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
	opts := loginRules(plan)
	opts.Installs = a.installs
	return d.Pending(ctx, opts)
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
		Host:      plan.Domain,
		Login:     plan.Rule.Login,
		Hosts:     []string{plan.Domain},
		Image:     plan.Rule.Image,
		Values:    plan.Rule.Values,
		Hosted:    plan.Rule.Hosted,
		OwnLogin:  plan.Rule.OwnLogin,
		Namespace: plan.Rule.Namespace,
		OIDC:      plan.Rule.OIDC,
	}
}
