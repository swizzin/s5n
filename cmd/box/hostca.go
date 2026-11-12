package main

import (
	"context"

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

// Plan is the read apply makes with its diff.
func (a hostCAStep) Plan(ctx context.Context, plan release.AnchorPlan) (string, error) {
	return a.decision(plan).Plan(ctx)
}

// Ensure writes or removes the anchor, after the confirm and before the
// release moves.
func (a hostCAStep) Ensure(ctx context.Context, plan release.AnchorPlan) error {
	_, err := a.decision(plan).Apply(ctx)
	return err
}

// hostCA is the step init's hand-over sets on its apply.
func hostCA(kubeconfig string, out say.Reporter, caFile string) release.Anchor {
	return hostCAStep{kubeconfig: kubeconfig, out: out, caFile: caFile}
}
