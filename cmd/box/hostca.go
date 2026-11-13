package main

import (
	"context"
	"fmt"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/login"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

// hostCA is root's host-CA step as `box init` hands it to internal/release: the
// write is platform's HostCADecision, and the issuer it follows is the one this
// run's per-host step writes the box's own Certificate with.
//
// It is not per host: it is one ConfigMap for the whole seedbox, and the
// certificate it carries is the issuer's, not a name's. That is also why a
// `box apply` does not get it: the anchor follows the Certificate the host is
// actually served with, apply never moves a host from one issuer to another,
// and a user's apply cannot read cert-manager's namespace at all.
//
// caFile is a development run's --ca-file: the e2e's Pebble is an ACME server
// whose certificates have no ca.crt to copy, so the test hands box the root it
// would otherwise read out of the box root's Secret. It feeds the same writer,
// so what the e2e proves is the product path with a different source.
type hostCAStep struct {
	kubeconfig string
	out        say.Reporter
	caFile     string
}

// decision builds the platform decision for one plan. The issuer plan.Issuer
// carries is init's own answer for the box's host name, so the anchor and the
// Certificate the per-host step writes can never disagree -- and on a first
// init, where no Certificate exists yet to read, it is the only input there is.
func (a hostCAStep) decision(plan release.AnchorPlan) *platform.HostCADecision {
	return &platform.HostCADecision{
		Namespace: plan.Namespace,
		Host:      plan.Domain,
		Issuer:    plan.Issuer,
		CAFile:    a.caFile,
		Open:      func() (platform.HostCACluster, error) { return platform.NewCluster(a.kubeconfig) },
		Say:       a.out,
	}
}

// login is the cluster client the OIDC clients' Secrets are written with: the
// same kubeconfig, in the seedbox's namespace.
func (a hostCAStep) login() *login.Decision {
	return &login.Decision{
		Open: func() (login.Cluster, error) { return platform.NewCluster(a.kubeconfig) },
		Say:  a.out,
	}
}

// Plan is the read apply makes with its diff: the anchor, and the OIDC clients'
// Secrets this run would write beside it. Both live in the seedbox's namespace,
// which is why they are one step.
func (a hostCAStep) Plan(ctx context.Context, plan release.AnchorPlan) (string, error) {
	line, err := a.decision(plan).Plan(ctx)
	if err != nil {
		return "", err
	}
	missing, err := a.login().PlanClients(ctx, plan.Namespace, plan.OIDC)
	if err != nil {
		return "", err
	}
	for _, app := range missing {
		more := fmt.Sprintf("write Secret %s/%s", plan.Namespace, authelia.OIDCClientSecretName(app))
		if line == "" {
			line = more
		} else {
			line += "; " + more
		}
	}
	return line, nil
}

// Ensure writes or removes the anchor, then the OIDC clients' Secrets, after
// the confirm and before the release moves. The second write is why this is the
// step for it: the seedbox's namespace exists by now (apply created it), and an
// app whose client secret is missing does not start -- so a failure here stops
// the run before Helm, with one sentence, rather than through a crash-loop.
func (a hostCAStep) Ensure(ctx context.Context, plan release.AnchorPlan) error {
	if _, err := a.decision(plan).Apply(ctx); err != nil {
		return err
	}
	if len(plan.OIDC) == 0 {
		return nil
	}
	_, err := a.login().EnsureClients(ctx, plan.Namespace, plan.OIDC)
	return err
}

// hostCA is the step init's hand-over sets on its apply.
func hostCA(kubeconfig string, out say.Reporter, caFile string) release.Anchor {
	return hostCAStep{kubeconfig: kubeconfig, out: out, caFile: caFile}
}
