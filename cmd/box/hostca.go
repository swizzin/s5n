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

// hostCA is root's host-CA step for init: one ConfigMap, written by platform's
// HostCADecision, following this run's issuer. A bare apply does not get it: it
// moves no host between issuers, and a user cannot read cert-manager's
// namespace. caFile is the e2e's --ca-file, feeding the same writer.
type hostCAStep struct {
	kubeconfig string
	out        say.Reporter
	caFile     string
}

// decision is platform's decision; plan.Issuer is init's answer, the only input
// a first init has.
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

// login writes OIDC client Secrets in the seedbox's namespace.
func (a hostCAStep) login() *login.Decision {
	return &login.Decision{
		Open: func() (login.Cluster, error) { return platform.NewCluster(a.kubeconfig) },
		Say:  a.out,
	}
}

// Plan reads the anchor and OIDC client Secrets; both live in the namespace.
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

// Ensure writes the anchor, then OIDC Secrets, after the confirm and before the
// release: a missing client would otherwise crash-loop through Helm.
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

// hostCA is the step init sets on its apply.
func hostCA(kubeconfig string, out say.Reporter, caFile string) release.Anchor {
	return hostCAStep{kubeconfig: kubeconfig, out: out, caFile: caFile}
}
