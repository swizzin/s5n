package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/login"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// certificateStep is the run's TLS: every host's port-80 listener and
// Certificate, one wait, then 443 and the redirect for each Ready host. Both
// commands use it, so a host is served one way.
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

// applyHosts is apply's per-host step (docs/box.md, "No run leaves an app host
// without its listener"). Missing checks rule, listener and Certificate; Ensure
// asks whether this identity may write them; Rules uses the login's own writer.
type applyHosts struct {
	kubeconfig string
	out        say.Reporter
	resolver   setup.Resolver
	// installs says the login comes after the confirm, so a missing one is this
	// run's page, not a refusal.
	installs bool
}

// cluster is lazy: a run refused earlier never contacts the cluster.
func (a applyHosts) cluster() (*platform.Kubernetes, error) {
	return platform.NewCluster(a.kubeconfig)
}

// Missing reads the Gateway, Certificates and login; a host without all three
// is this run's work.
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

// Stale is Missing's other half: answering hosts whose Certificate is not the
// one this run would issue. Only `box init` asks; a bare apply leaves them.
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
		// This run's decision. Forced alone misses the DNS-chosen issuer.
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

// Ensure DNS-checks every name, writes the login's rule, then listeners and
// Certificates.
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
		// Every decided name: init re-issues a self-signed host the plan shows
		// nothing new for.
		names = plan.All
	}
	return step.Ensure(ctx, names)
}

// Prune removes every host box wrote and this run does not serve. A hosted instance whose
// name changed, or whose app was switched off, would otherwise keep its listener and
// Certificate: the old name would answer and renew forever, and a redirect from it is not
// written.
//
// It is a Pruner rather than part of Ensure because a run with nothing to add still has
// this to do.
func (a applyHosts) Prune(ctx context.Context, plan release.HostPlan) error {
	if plan.Domain == "" {
		return nil
	}
	cl, err := a.cluster()
	if err != nil {
		return err
	}
	wrote, err := platform.BoxHosts(ctx, cl)
	if err != nil {
		return fmt.Errorf("read the hosts box serves: %w", err)
	}
	drop := platform.Unserved(wrote, plan.Served)
	if len(drop) == 0 {
		return nil
	}
	for _, host := range drop {
		if err := platform.DropHost(ctx, cl, host); err != nil {
			return err
		}
	}
	a.out.Rows([]say.Row{{Mark: say.MarkDone, Name: "hosts", State: "removed", Facts: []string{strings.Join(drop, ", ")}}})
	// Seerr's own Application URL is a setting inside it, written by whoever set it up:
	// box can serve the new name but not retarget the instance.
	for _, host := range drop {
		if seerrName(host) {
			a.out.Warn("Seerr's Application URL setting is yours to update: it names the address its own links are built from")
			break
		}
	}
	return nil
}

// seerrName reports whether a name looks like a Seerr instance's: box names one `seerr`,
// or `seerr-<suffix>` before the domain.
func seerrName(host string) bool {
	first, _, _ := strings.Cut(host, ".")
	return first == "seerr" || strings.HasPrefix(first, "seerr-")
}

// rules writes every served name's access_control rule: without it a host
// answers 403 whatever its listener and Certificate say.
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

// Pending reports whether the login page differs; Reconcile writes it. Both are
// root's: the login serves every seedbox, and a user's `theme:` is their own
// box's palette, never this page (cmd/box/settings.go).
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

// MayWrite is release.LoginAccess' first question for a run that enables an app
// signing in through the login: the same one that decides whether this run has
// a login half at all (login.MayWrite). A seedbox user's kubeconfig cannot
// apply a ConfigMap in the platform's namespace, so it cannot register a client
// either -- and the release refuses before the confirm rather than letting Helm
// wait on a pod whose Secret is missing.
func (a applyHosts) MayWrite(ctx context.Context) (bool, error) {
	cl, err := a.cluster()
	if err != nil {
		return false, err
	}
	return login.MayWrite(ctx, cl)
}

// ClientRegistered reads whether one app's client Secret is in the seedbox's
// namespace. It is a plain Get, which a user's kubeconfig may make in its own
// namespace: the refusal needs no permission the box itself does not have.
func (a applyHosts) ClientRegistered(ctx context.Context, namespace, app string) (bool, error) {
	cl, err := a.cluster()
	if err != nil {
		return false, err
	}
	_, found, err := cl.Get(ctx, "", "v1", "Secret", namespace, authelia.OIDCClientSecretName(app))
	return found, err
}

// The two answers above are the release's, beside Login: a user's step still
// gets them, and answers "no" to MayWrite. Prune is the third: a user cannot write a
// listener, so their run neither adds nor removes one.
var _ release.LoginAccess = applyHosts{}
var _ release.HostPruner = applyHosts{}

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
