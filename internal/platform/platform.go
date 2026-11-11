// Package platform applies the platform layer box owns: the pieces k0s does
// not ship (PLAN.md, "platform layer"). `box bootstrap` decides, through
// internal/bootstrap, which of the four capabilities is already there and
// which one box must install; this package is the half that writes.
//
// It applies only the components the decision says "install". A component it
// uses is left alone -- box never touches someone else's Gateway or
// LoadBalancer -- and a component the decision refuses (a conflict) stops the
// run before the first write. cert-manager is not implemented yet: when the
// decision says to install it, box says so out loud and carries on with the
// rest, rather than silently leaving a TLS hole.
//
// Like internal/release the flow is a decision over small interfaces
// (Releases, Cluster, Confirmer), with the Helm SDK and client-go
// implementations in this package beside it, so the plan, the ordering, the
// refusal paths and "a refusal writes nothing" are tested from stubs without a
// cluster. The versions are the chart release's (release.Versions, its
// platform.yaml) and are passed in; the chart references below are not
// versions, and box fetches the charts at run time -- no chart tarball is
// embedded.
package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/wait"
)

// The platform releases: where each chart comes from and what box calls the
// result. The versions themselves are not here -- every version box installs
// lives in the chart release's platform.yaml (release.Versions), so a bump
// moves one line of the chart and this package installs it.
const (
	// EnvoyGatewayChart is the Envoy Gateway chart, which ships the Gateway
	// API CRDs at the experimental channel it is built against. The chart owns
	// the CRDs on purpose: a separately pinned standard channel can drift from
	// (or conflict with) the set the controller expects.
	EnvoyGatewayChart     = "gateway-helm"
	EnvoyGatewayRef       = "oci://docker.io/envoyproxy/gateway-helm"
	EnvoyGatewayRelease   = "envoy-gateway"
	EnvoyGatewayNamespace = "envoy-gateway-system"

	// MetalLBChart is the MetalLB chart, in L2 mode: it makes
	// `type: LoadBalancer` work on bare metal with no extra addresses.
	MetalLBChart     = "metallb"
	MetalLBRef       = "https://metallb.github.io/metallb"
	MetalLBRelease   = "metallb"
	MetalLBNamespace = "metallb-system"

	// CertManagerChart is the cert-manager chart. Its CRDs and Gateway API
	// support are part of the install: the CRDs are what a Certificate is,
	// and config.gatewayAPI.enabled is what lets the controller solve an
	// HTTP-01 challenge through the Gateway's HTTPRoute.
	CertManagerChart     = "cert-manager"
	CertManagerRef       = "oci://quay.io/jetstack/charts/cert-manager"
	CertManagerRelease   = "cert-manager"
	CertManagerNamespace = "cert-manager"

	// The Gateway the chart's HTTPRoutes attach to (install/platform/gateway/gateway.yaml,
	// which the kind e2e applies and box embeds).
	GatewayNamespace = "s5n-gateway"
	GatewayName      = "s5n"

	// MetalLBPoolName and MetalLBL2Name are box's pool and advertisement.
	MetalLBPoolName = "s5n-pool"
	MetalLBL2Name   = "s5n-l2"
)

// Condition names what has to be false for a step to run. A plan is printed
// before the confirm, so a question that can only be settled by a write is
// printed as a question (`?`) rather than guessed at.
type Condition string

const (
	// UnlessGatewayAddress: the step runs only if the Gateway box applied
	// gets no address of its own. The Gateway's status answers it, after the
	// Gateway exists, which is why it is not known when the plan is printed.
	UnlessGatewayAddress Condition = "unless the Gateway gets an address without it"
)

// Step kinds, in the order box applies them.
type StepKind string

const (
	// StepRelease is a Helm release: install-or-upgrade, with Helm's wait.
	StepRelease StepKind = "release"
	// StepManifests is an embedded manifest applied with server-side apply.
	StepManifests StepKind = "manifests"
	// StepPool is MetalLB's IPAddressPool/L2Advertisement, built from the
	// node's primary InternalIP once the cluster is open.
	StepPool StepKind = "metallb-pool"
	// StepIssuers is cert-manager's two ClusterIssuers. They are retried like
	// the MetalLB pool: the webhook's endpoint can be Ready while the server
	// still refuses its certificate, and the first apply is what waits it out.
	StepIssuers StepKind = "cert-manager-issuers"
	// StepGatewayAddress is the question the plan could not answer before the
	// confirm: does the Gateway get an address without MetalLB? It waits, and
	// what it finds decides the conditional steps that follow.
	StepGatewayAddress StepKind = "gateway-address"
	// StepReachable is the Gateway's address answering HTTP from this host.
	StepReachable StepKind = "gateway-reachable"
)

// WaitKind is what readiness means for one object.
type WaitKind string

const (
	WaitDeployment  WaitKind = "deployment"
	WaitDaemonSet   WaitKind = "daemonset"
	WaitCertificate WaitKind = "certificate"
)

// Wait is one readiness check after a step. The cluster implementation bounds
// it; the decision only says what has to be true.
type Wait struct {
	Kind      WaitKind
	Namespace string
	Name      string
	// Timeout bounds this wait when non-zero; otherwise the client's own
	// bound applies. A caller with a shorter budget than the platform's five
	// minutes (a certificate that is not issued yet) uses it.
	Timeout time.Duration
}

// String is how box names the object in its output.
func (w Wait) String() string {
	switch w.Kind {
	case WaitDaemonSet:
		return fmt.Sprintf("DaemonSet %s/%s", w.Namespace, w.Name)
	case WaitCertificate:
		return fmt.Sprintf("Certificate %s/%s", w.Namespace, w.Name)
	default:
		return fmt.Sprintf("Deployment %s/%s", w.Namespace, w.Name)
	}
}

// Release is one Helm release box owns.
type Release struct {
	Name      string
	Namespace string
	// Ref is the repository: an oci:// reference or an https chart-repo URL.
	Ref     string
	Chart   string
	Version string
	Values  map[string]any
}

// Was is how the plan names a release.
func (r Release) String() string {
	return fmt.Sprintf("%s %s (Helm, %s)", r.Name, r.Version, r.Namespace)
}

// Step is one item of the plan, in application order.
type Step struct {
	Component bootstrap.PlatformComponent
	Kind      StepKind
	What      string
	Release   Release
	// Documents is the objects of a StepManifests step, already parsed and
	// (for the Gateway) pointed at the chosen GatewayClass. Parsing at plan
	// time keeps the apply path free of YAML.
	Documents []map[string]any
	Wait      []Wait
	// Unless is set when the step only happens if the question the plan
	// cannot answer before the confirm comes back one way: MetalLB and its
	// pool are skipped when the Gateway has an address without them.
	Unless Condition
	// Mode is the ownership a step that asks the cluster a question acts
	// under: in external mode the Gateway must get an address from something
	// box does not manage, in auto mode its absence is box's to fix.
	Mode bootstrap.PlatformMode
	// InstalledVersion is filled in by resolution: the chart version a
	// deployed release already has. AtPin(InstalledVersion, Release.Version)
	// means the step is verification only, and no Helm call happens.
	InstalledVersion string
	// Foreign marks a deployed release of the same name that box did not
	// install (it carries no ownership label). It is never upgraded; a
	// conditional step that would have to run stops instead.
	Foreign bool
}

// Plan is what bootstrap will apply, in order.
type Plan struct {
	Steps []Step
	// NotHandled names decided installs this version of box does not do, with
	// the reason. It is printed, never silently dropped.
	NotHandled []string
}

var (
	actionInstall = "install"
	actionProbe   = "probe"
	actionVerify  = "verify"
	actionStop    = "stop"

	// ErrCancelled says the operator answered no. Nothing was written.
	ErrCancelled = errors.New("cancelled")
)

// Reads is the read side of the cluster.
type Cluster interface {
	// Allowed answers one SelfSubjectAccessReview.
	Allowed(ctx context.Context, a Access) (bool, error)
	// PrimaryInternalIP returns the node's primary InternalIP.
	PrimaryInternalIP(ctx context.Context) (string, error)
	// Apply server-side-applies the documents with field manager box.
	Apply(ctx context.Context, docs []map[string]any) error
	// Await blocks until the object is ready, or the bound expires.
	Await(ctx context.Context, w Wait) error
}

// Releases is the Helm side.
type Releases interface {
	// Version returns the chart version of a deployed release, whether box
	// installed it (its ownership label), and false when the release is not
	// installed.
	Version(ctx context.Context, name, namespace string) (version string, owned, exists bool, err error)
	// Install installs or upgrades the release and waits for its workloads.
	// It always stamps box's ownership label.
	Install(ctx context.Context, spec Release) error
}

// Confirmer asks the one question before the first write.
type Confirmer interface {
	Ask(question string) (bool, error)
}

// Decision is one `box bootstrap`'s platform half.
type Decision struct {
	// Open builds the cluster client. It does not contact the cluster, and it
	// is only called when something will be read or written.
	Open     func() (Cluster, error)
	Releases Releases
	// Probe builds the Gateway probe. It is only called by a step that needs
	// it -- after the confirm, never before -- and it does not contact the
	// cluster either.
	Probe   func() (GatewayProbe, error)
	Confirm Confirmer
	// Say is every line the decision writes. A nil reporter writes nothing;
	// cmd/box always gives the plain or the live one, and policy stays here —
	// only how a plan, a wait and a finished step look is the reporter's.
	Say say.Reporter
	// GatewayClass is the class bootstrap chose (bootstrap.Result.Gateway.
	// Class). The Gateway box applies attaches to it; the GatewayClass itself
	// is created only when this run installs its implementation.
	GatewayClass string
	// Pins is the platform pins of the chart release bootstrap pulled
	// (release.Versions): the chart versions this plan installs. A plan
	// cannot be built without them, and they come from the release rather
	// than from this package, so a platform patch is a chart release.
	Pins release.Versions
	// ACMEServer points the s5n-letsencrypt issuer at another ACME directory.
	// Empty in every real run: the manifest's Let's Encrypt production URL is
	// the only one box ships. It exists for the e2e's Pebble server, which
	// cannot be reached at the production URL and whose own certificate is
	// self-signed.
	ACMEServer string

	// AddressTimeout is how long the Gateway gets to say it has no address
	// before MetalLB is installed. Zero means AddressTimeout (15s).
	AddressTimeout time.Duration
	// ProbeTimeout is the whole wait for the Gateway to be Programmed with an
	// address. Zero means ProbeTimeout (60s).
	ProbeTimeout time.Duration
	// ReachTimeout is how long that address gets to answer HTTP. Zero means
	// ReachTimeout (30s).
	ReachTimeout time.Duration
	// PoolTimeout bounds how long the MetalLB webhook may refuse its pool.
	// Zero means two minutes.
	PoolTimeout time.Duration
	// WebhookTimeout bounds how long cert-manager's webhook may refuse the
	// first ClusterIssuer apply. Zero means two minutes.
	WebhookTimeout time.Duration
	// Poll is how often a refused pool is retried. Zero means two seconds.
	Poll time.Duration
}

// Result is what happened, for the caller and for tests.
type Result struct {
	Plan       Plan
	Installed  []string
	Verified   []string
	NotHandled []string
}

// report is the one place the flow writes.
func (d *Decision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply plans, asks and applies the platform layer.
func (d *Decision) Apply(ctx context.Context, components []bootstrap.ComponentResult) (Result, error) {
	var res Result
	plan, err := BuildPlan(d.Pins, components, d.GatewayClass, d.ACMEServer)
	if err != nil {
		return res, err
	}
	res.Plan = plan
	res.NotHandled = plan.NotHandled

	if err := d.resolve(ctx, &plan); err != nil {
		return res, err
	}
	res.Plan = plan
	r := d.report()
	d.printPlan(r, plan)

	if len(plan.Steps) == 0 {
		return res, nil
	}

	cl, err := d.Open()
	if err != nil {
		return res, err
	}

	// Every check below is a read, and they are the last thing before the
	// confirm: a kubeconfig that cannot create a CRD is refused before the
	// operator is asked, and before anything is written.
	if err := preflight(ctx, cl, preflightAccess(plan)); err != nil {
		return res, err
	}

	ok, err := d.Confirm.Ask("install the platform layer?")
	if err != nil {
		return res, err
	}
	if !ok {
		r.Line("oh heck, nvm")
		return res, ErrCancelled
	}

	// answered is what the plan's question found, once it has been asked. It
	// is only known after the Gateway is applied, so the steps carry it: every
	// step with Unless runs or not depending on it.
	answered := ""
	for _, s := range plan.Steps {
		if s.Unless != "" && answered != "" {
			continue
		}
		switch s.Kind {
		case StepRelease:
			if s.Foreign {
				return res, fmt.Errorf("a Helm release named %s already exists in %s, but box did not install it (no %s=%s label): box will not upgrade another operator's release. Remove it, or use the implementation it provides",
					s.Release.Name, s.Release.Namespace, kube.ManagedByLabel, kube.ManagedBy)
			}
			if AtPin(s.InstalledVersion, s.Release.Version) {
				step := r.Start(say.Op{Label: "verifying " + s.Release.Name})
				step.DoneAs(
					fmt.Sprintf("= %s in %s: already at %s", s.Release.Name, s.Release.Namespace, s.Release.Version),
					fmt.Sprintf("%s in %s is already at %s", s.Release.Name, s.Release.Namespace, s.Release.Version))
				res.Verified = append(res.Verified, s.Release.Name)
			} else {
				step := r.Start(say.Op{
					Label: "installing " + s.Release.String(),
					Plain: "installing " + s.Release.String(),
				})
				if err := d.Releases.Install(ctx, s.Release); err != nil {
					step.Failed(err)
					return res, err
				}
				step.Done(fmt.Sprintf("installed %s in %s", s.Release.Name, s.Release.Namespace))
				res.Installed = append(res.Installed, s.Release.Name)
			}
			for _, w := range s.Wait {
				if err := d.await(ctx, cl, w); err != nil {
					return res, err
				}
			}
		case StepManifests:
			step := r.Start(say.Op{Label: "applying " + s.What})
			if err := cl.Apply(ctx, s.Documents); err != nil {
				step.Failed(err)
				return res, fmt.Errorf("%s: %w", s.What, err)
			}
			step.Done("applied " + s.What)
			res.Installed = append(res.Installed, s.What)
			for _, w := range s.Wait {
				if err := d.await(ctx, cl, w); err != nil {
					return res, err
				}
			}
		case StepPool:
			if err := d.applyPool(ctx, cl); err != nil {
				return res, err
			}
			res.Installed = append(res.Installed, "MetalLB address pool")
		case StepIssuers:
			if err := d.applyIssuers(ctx, cl, s.Documents); err != nil {
				return res, err
			}
			res.Installed = append(res.Installed, s.What)
		case StepGatewayAddress:
			answer, err := d.gatewayAddress(ctx, s.Mode)
			if err != nil {
				return res, err
			}
			answered = answer.Address
		case StepReachable:
			address, err := d.reachable(ctx)
			if err != nil {
				return res, err
			}
			res.Verified = append(res.Verified, "Gateway "+address)
		}
	}
	r.Success("platform ready")
	return res, nil
}

// resolve reads which releases are already deployed, so the printed plan says
// install or verify instead of guessing. A release at another version is an
// upgrade: Helm's own action, one write.
func (d *Decision) resolve(ctx context.Context, plan *Plan) error {
	boxMetalLB := false
	for i := range plan.Steps {
		s := &plan.Steps[i]
		if s.Kind != StepRelease {
			continue
		}
		v, owned, ok, err := d.Releases.Version(ctx, s.Release.Name, s.Release.Namespace)
		if err != nil {
			return err
		}
		if ok && owned {
			s.InstalledVersion = v
			if s.Release.Name == MetalLBRelease {
				boxMetalLB = true
			}
			continue
		}
		if ok {
			// A same-named release box did not install is the operator's.
			// A conditional step may never run (the Gateway's own address
			// decides), so it is only marked here; an unconditional one is
			// refused before the confirm.
			s.Foreign = true
			if s.Unless == "" {
				return fmt.Errorf("a Helm release named %s already exists in %s, but box did not install it (no %s=%s label): box will not upgrade another operator's release. Remove it, or use the implementation it provides",
					s.Release.Name, s.Release.Namespace, kube.ManagedByLabel, kube.ManagedBy)
			}
		}
	}
	if boxMetalLB {
		// A MetalLB release box installed is box's, at the pinned version or
		// one upgrade behind: verify or upgrade it as before. There is no
		// install left to decide, so the Gateway question goes -- and keeping
		// it would skip box's own upgrade, since box's own MetalLB is exactly
		// what gives the Gateway an address.
		plan.settle()
	}
	return nil
}

// settle drops the Gateway question and makes every conditional step
// unconditional: box's MetalLB is already there, so nothing is being decided.
func (p *Plan) settle() {
	steps := make([]Step, 0, len(p.Steps))
	for _, s := range p.Steps {
		if s.Kind == StepGatewayAddress {
			continue
		}
		s.Unless = ""
		steps = append(steps, s)
	}
	p.Steps = steps
}

// gatewayAddress asks whether the Gateway got an address without box's
// MetalLB. In auto mode no address is the answer that installs MetalLB; in
// external mode it is a refusal, because the operator said the address layer
// is theirs.
func (d *Decision) gatewayAddress(ctx context.Context, mode bootstrap.PlatformMode) (GatewayAnswer, error) {
	p, err := d.probe()
	if err != nil {
		return GatewayAnswer{}, err
	}
	r := d.report()
	quick := d.AddressTimeout
	if quick <= 0 {
		quick = AddressTimeout
	}
	timeout := d.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	if quick > timeout || mode == bootstrap.ModeExternal {
		// In external mode the operator said something else hands out
		// addresses, and a slow LoadBalancer is still a working one, so it
		// gets the whole wait.
		quick = timeout
	}
	wait := r.Start(say.Op{
		Label:  "waiting for the Gateway to report an address",
		Plain:  fmt.Sprintf("waiting up to %s for Gateway %s/%s to report an address", quick, GatewayNamespace, GatewayName),
		Budget: quick,
	})
	answer, err := p.Address(ctx, quick)
	wait.Done("")
	if err != nil {
		return answer, err
	}
	if settling(answer) && timeout > quick {
		// Only "no address, AddressNotAssigned" is an answer at the short
		// deadline. Anything else is still moving, so it gets the rest of
		// the whole wait before box judges it.
		wait = r.Start(say.Op{
			Label:  "waiting for the Gateway to settle",
			Plain:  fmt.Sprintf("the Gateway is still settling (%s); waiting up to another %s", reasonOr(answer.Reason), timeout-quick),
			Budget: timeout - quick,
		})
		answer, err = p.Address(ctx, timeout-quick)
		wait.Done("")
		if err != nil {
			return answer, err
		}
	} else {
		timeout = quick
	}
	switch {
	case answer.Address != "" && answer.Programmed() && mode == bootstrap.ModeExternal:
		r.Success(fmt.Sprintf("the Gateway has an address from the external LoadBalancer: %s", answer.Address))
	case answer.Address != "" && answer.Programmed():
		r.Success(fmt.Sprintf("the Gateway already has an address: %s; MetalLB and its address pool are not installed", answer.Address))
	case answer.Address != "" || !deferredReason(answer.Reason):
		// An address the Gateway will not call Programmed, or no address for a
		// reason an address layer cannot answer: both are failures at the
		// deadline, and installing MetalLB over one would only hide it.
		return answer, gatewayFailure(answer, timeout)
	case mode == bootstrap.ModeExternal:
		return answer, fmt.Errorf("platform.loadbalancer.mode is external, but Gateway %s/%s got no address within %s (%s): something else has to assign one (MetalLB, k3s servicelb or a cloud provider). Install it and run `box bootstrap` again, or set platform.loadbalancer.mode=auto to let box install MetalLB",
			GatewayNamespace, GatewayName, timeout, reasonOr(answer.Reason))
	default:
		r.Line(fmt.Sprintf("the Gateway has no address (%s) after waiting up to %s; installing MetalLB", reasonOr(answer.Reason), timeout))
	}
	return answer, nil
}

// reachable is the last check on every path: the address the Gateway reports
// has to answer HTTP from this host, not only in the API server's opinion.
func (d *Decision) reachable(ctx context.Context) (string, error) {
	p, err := d.probe()
	if err != nil {
		return "", err
	}
	timeout := d.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	// The same rule as the question, with one difference: MetalLB has been
	// installed and its pool applied by now, so a second AddressNotAssigned is
	// the end of what box can do.
	r := d.report()
	wait := r.Start(say.Op{
		Label:  "waiting for the Gateway to be Programmed with its address",
		Plain:  fmt.Sprintf("waiting up to %s for Gateway %s/%s to be Programmed with its address", timeout, GatewayNamespace, GatewayName),
		Budget: timeout,
	})
	answer, err := p.Address(ctx, timeout)
	wait.Done("")
	if err != nil {
		return "", err
	}
	switch {
	case answer.Address == "" && deferredReason(answer.Reason):
		return "", fmt.Errorf("Gateway %s/%s still has no address after another %s (%s): its Service has no LoadBalancer address, and box has nothing left to install for it",
			GatewayNamespace, GatewayName, timeout, reasonOr(answer.Reason))
	case answer.Address == "" || !answer.Programmed():
		return "", gatewayFailure(answer, timeout)
	}
	retry := d.ReachTimeout
	if retry <= 0 {
		retry = ReachTimeout
	}
	reach := r.Start(say.Op{
		Label: fmt.Sprintf("checking that the Gateway answers on %s:80", answer.Address),
		Plain: fmt.Sprintf("checking that the Gateway answers on %s:80 from this host", answer.Address),
	})
	code, err := p.Reachable(ctx, answer.Address, retry)
	if err != nil {
		reach.Failed(err)
		return "", fmt.Errorf("the Gateway has address %s, but nothing answers on %s:80 from this host: %w", answer.Address, answer.Address, err)
	}
	reach.Done(fmt.Sprintf("the Gateway answers on %s:80 (HTTP %d)", answer.Address, code))
	return answer.Address, nil
}

// probe builds the Gateway probe, or says the binary has none.
func (d *Decision) probe() (GatewayProbe, error) {
	if d.Probe == nil {
		return nil, errNoProbe
	}
	p, err := d.Probe()
	if err != nil {
		return nil, fmt.Errorf("Gateway probe: %w", err)
	}
	return p, nil
}

// gatewayFailure is box's refusal when the Gateway is not going to be fixed by
// an address layer: it has an address but will not call itself Programmed, or
// it has none and says why in a way MetalLB cannot answer.
func gatewayFailure(answer GatewayAnswer, timeout time.Duration) error {
	what := "has no address and is not Programmed"
	if answer.Address != "" {
		what = "has address " + answer.Address + " but is not Programmed"
	}
	reason := reasonOr(answer.Reason)
	if answer.Message != "" {
		reason += ": " + answer.Message
	}
	return fmt.Errorf("Gateway %s/%s %s after %s (%s)", GatewayNamespace, GatewayName, what, timeout, reason)
}

// settling says an answer at the short deadline is not yet one to act on: the
// Gateway is neither Programmed with an address nor saying plainly that no
// address was assigned, and it exists. Envoy Gateway reports NoResources with
// its address already assigned while the proxy pod rolls out, which is the
// common case on a cluster that already has a LoadBalancer.
func settling(a GatewayAnswer) bool {
	if a.missing {
		return false
	}
	if a.Address != "" && a.Programmed() {
		return false
	}
	return a.Address != "" || a.Reason != gatewayAddressNotAssigned
}

// deferredReason is a reason that says nothing about the address layer: the
// Gateway's Service has no address yet, or the Gateway has not been reconciled
// at all. Anything else (NoResources, AddressNotUsable) is about the data
// plane, and box stops on it rather than installing MetalLB over it.
func deferredReason(reason string) bool {
	return reason == "" || reason == gatewayAddressNotAssigned
}

// reasonOr is the Gateway's own word for why it has no address, for box's
// line: the field is empty on a Gateway that has not been reconciled yet.
func reasonOr(reason string) string {
	if reason == "" {
		return "not reconciled yet"
	}
	return reason
}

func (d *Decision) await(ctx context.Context, cl Cluster, w Wait) error {
	step := d.report().Start(say.Op{
		Label: fmt.Sprintf("waiting for %s", w),
		Plain: fmt.Sprintf("waiting for %s", w),
	})
	if err := cl.Await(ctx, w); err != nil {
		step.Failed(err)
		return fmt.Errorf("%s: %w", w, err)
	}
	step.Done(fmt.Sprintf("%s: rolled out", w))
	return nil
}

// retryApply applies docs until the API server accepts them or the bound
// expires. Both webhooks box talks to -- MetalLB's pool and cert-manager's
// issuers -- are served by pods that are Ready a moment before they can accept
// a connection (they load their certificate first), and the chart's wait only
// proves the pod is Ready. The caller keeps its own words for the refusal, and
// the last one is what it says: a webhook that refused for its own reason is
// the thing the operator has to fix.
func (d *Decision) retryApply(ctx context.Context, cl Cluster, docs []map[string]any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	var last error
	waited, err := wait.Until(ctx, timeout, poll, func(ctx context.Context) (bool, error) {
		if applyErr := cl.Apply(ctx, docs); applyErr != nil {
			last = applyErr
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !waited.Done {
		return last
	}
	return nil
}

// applyPool writes MetalLB's pool and advertisement, retrying until the
// webhook answers.
func (d *Decision) applyPool(ctx context.Context, cl Cluster) error {
	r := d.report()
	step := r.Start(say.Op{Label: "applying the MetalLB address pool"})
	ip, err := cl.PrimaryInternalIP(ctx)
	if err != nil {
		step.Failed(err)
		return fmt.Errorf("MetalLB address pool: %w", err)
	}
	docs, err := ParseManifests(poolManifest(ip))
	if err != nil {
		step.Failed(err)
		return fmt.Errorf("MetalLB address pool: %w", err)
	}
	wait := d.PoolTimeout
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	if err := d.retryApply(ctx, cl, docs, wait); err != nil {
		step.Failed(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("MetalLB refused its address pool for %s: %w (the chart is installed; fix the cause and run `box bootstrap` again)", wait, err)
	}
	step.Done(fmt.Sprintf("applied IPAddressPool %s (%s/32) and L2Advertisement %s", MetalLBPoolName, ip, MetalLBL2Name))
	return nil
}

func (d *Decision) applyIssuers(ctx context.Context, cl Cluster, docs []map[string]any) error {
	r := d.report()
	step := r.Start(say.Op{Label: "applying the cert-manager ClusterIssuers"})
	wait := d.WebhookTimeout
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	if err := d.retryApply(ctx, cl, docs, wait); err != nil {
		step.Failed(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cert-manager's webhook refused its ClusterIssuers for %s: %w (cert-manager is installed; fix the cause and run `box bootstrap` again)", wait, err)
	}
	step.Done(fmt.Sprintf("applied ClusterIssuers %s and %s", LetsEncryptIssuer, SelfSignedIssuer))
	return nil
}

func (d *Decision) printPlan(r say.Reporter, plan Plan) {
	if len(plan.Steps) == 0 && len(plan.NotHandled) == 0 {
		return
	}
	var items []say.Item
	for _, s := range plan.Steps {
		switch s.Kind {
		case StepRelease:
			if s.Foreign {
				// Only a conditional foreign release reaches the plan: the
				// unconditional one is refused in resolve.
				items = append(items, say.Item{Mark: say.Question, Text: fmt.Sprintf("%s, %s (a release box did not install already exists; box will not upgrade it)", s.Release, s.Unless)})
				continue
			}
			if AtPin(s.InstalledVersion, s.Release.Version) {
				items = append(items, say.Item{Mark: say.Verify, Text: fmt.Sprintf("%s: already at %s; verifying", s.Release.Name, s.Release.Version)})
				continue
			}
			// A conditional release is never an upgrade: resolve() drops the
			// condition as soon as the release is there, so `?` never has to
			// carry an install-or-upgrade mark.
			if s.Unless != "" {
				items = append(items, say.Item{Mark: say.Question, Text: fmt.Sprintf("%s, %s", s.Release, s.Unless)})
				continue
			}
			mark := say.Install
			text := s.Release.String()
			if s.InstalledVersion != "" {
				mark = say.Change
				// The upgrade is a write, so the plan says what it is
				// replacing -- including a deployed version box cannot read.
				if why := notAtPin(s.InstalledVersion, s.Release.Version); why != "" {
					text += ", " + why
				}
			}
			items = append(items, say.Item{Mark: mark, Text: text})
		case StepGatewayAddress:
			// A question the plan cannot answer yet: box finds out after the
			// Gateway is applied, and the `?` steps below depend on the answer.
			items = append(items, say.Item{Mark: say.Question, Text: s.What})
		default:
			if s.Unless != "" {
				items = append(items, say.Item{Mark: say.Question, Text: fmt.Sprintf("%s, %s", s.What, s.Unless)})
				continue
			}
			items = append(items, say.Item{Mark: say.Install, Text: s.What})
		}
	}
	for _, line := range plan.NotHandled {
		items = append(items, say.Item{Mark: say.Warning, Text: line})
	}
	r.Plan("platform plan", items)
}

// BuildPlan turns the decision's component results into the ordered list of
// writes. It is pure: no cluster, no clock. Only `install` produces a release;
// `use` and `verify` leave an implementation alone, and `stop` refuses the
// whole run. pins are the chart release's platform.yaml, so every chart version
// in the plan is the release's rather than a constant of this package.
//
// gatewayClass is the class bootstrap chose (bootstrap.Result.Gateway). The
// Gateway box owns is written on every run, on that class: the chart's routes
// attach to box's Gateway, so it has to exist whether box installed the
// implementation or found one. The GatewayClass is written only when box
// installs Envoy Gateway, because a class someone else's controller serves is
// the operator's and must not be rewritten.
func BuildPlan(pins release.Versions, components []bootstrap.ComponentResult, gatewayClass, acmeServer string) (Plan, error) {
	var plan Plan
	byComponent := make(map[bootstrap.PlatformComponent]bootstrap.ComponentResult, len(components))
	for _, c := range components {
		if c.Action == actionStop {
			return Plan{}, fmt.Errorf("platform conflict: %s: %s", c.Component, c.Reason)
		}
		byComponent[c.Component] = c
	}
	if gatewayClass == "" {
		return Plan{}, errors.New("no GatewayClass: box bootstrap chooses one before planning the Gateway")
	}

	gwAPI := byComponent[bootstrap.ComponentGatewayAPI]
	gw := byComponent[bootstrap.ComponentGateway]
	lb := byComponent[bootstrap.ComponentLoadBalancer]
	cm := byComponent[bootstrap.ComponentCertManager]

	// Envoy Gateway is both the Gateway API CRDs and the controller, so an
	// install of either capability is one Helm release -- and that release is
	// exactly the case where box also creates the GatewayClass.
	installEnvoy := gwAPI.Action == actionInstall || gw.Action == actionInstall
	if installEnvoy {
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentGatewayAPI,
			Kind:      StepRelease,
			Release: Release{
				Name:      EnvoyGatewayRelease,
				Namespace: EnvoyGatewayNamespace,
				Ref:       EnvoyGatewayRef,
				Chart:     EnvoyGatewayChart,
				Version:   pins.EnvoyGateway,
			},
			Wait: []Wait{{Kind: WaitDeployment, Namespace: EnvoyGatewayNamespace, Name: EnvoyGatewayRelease}},
		})
	}

	// cert-manager is the TLS half of the platform. It is installed only when
	// the decision says so; an existing install box can use is verified
	// instead (the probe already refused one it cannot use). Its CRDs ship
	// with the chart -- a separately pinned CRD bundle could drift from the
	// controller -- and config.gatewayAPI.enabled is the controller setting
	// that makes the Gateway HTTP-01 solver work at all.
	if cm.Action == actionInstall {
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentCertManager,
			Kind:      StepRelease,
			Release: Release{
				Name:      CertManagerRelease,
				Namespace: CertManagerNamespace,
				Ref:       CertManagerRef,
				Chart:     CertManagerChart,
				Version:   pins.CertManager,
				Values: map[string]any{
					"crds": map[string]any{"enabled": true},
					"config": map[string]any{
						"gatewayAPI": map[string]any{"enabled": true},
					},
				},
			},
			Wait: []Wait{
				{Kind: WaitDeployment, Namespace: CertManagerNamespace, Name: CertManagerRelease},
				{Kind: WaitDeployment, Namespace: CertManagerNamespace, Name: CertManagerRelease + "-webhook"},
				{Kind: WaitDeployment, Namespace: CertManagerNamespace, Name: CertManagerRelease + "-cainjector"},
			},
		})
	}

	// MetalLB is the address layer, and the Gateway is what needs it: in
	// s5n the torrent port is a NodePort, so a LoadBalancer Service exists
	// only because the Gateway implementation creates one for the Gateway.
	// That is what makes the Gateway's own status the capability probe.
	//
	// managed mode owns the address layer, so it installs it first and never
	// asks anything. auto mode applies the Gateway and then asks whether it
	// got an address on its own; only no address installs MetalLB. external
	// mode owns nothing: the same question verifies the operator's answer.
	managedLB := lb.Action == actionInstall
	if managedLB {
		plan.Steps = append(plan.Steps, metalLBSteps(pins, "")...)
	}

	// Box's Gateway, on every path. On `use` it is still box's object: the
	// chart's HTTPRoutes name it, and nothing else on the cluster does.
	objects, err := gatewayObjects(gatewayClass, installEnvoy)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentGateway,
		Kind:      StepManifests,
		What:      gatewayStepName(gatewayClass, installEnvoy),
		Documents: objects,
	})

	// The issuers are root's, and every seedbox's Certificate names one. They
	// are applied after the Gateway they solve through, on every run: a
	// re-apply is the same bytes, and it is what repairs a cluster whose
	// ClusterIssuers were removed. They are only meaningful when cert-manager
	// is going to be there, which the decision has already settled.
	issuers, err := issuerObjects(acmeServer)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentCertManager,
		Kind:      StepIssuers,
		What:      fmt.Sprintf("ClusterIssuers %s and %s", LetsEncryptIssuer, SelfSignedIssuer),
		Documents: issuers,
	})

	switch lb.Action {
	case actionProbe:
		// auto mode: the Gateway's own status decides whether MetalLB is box's
		// to install. The question is asked on every run -- a re-run asks the
		// Gateway that is already there, and a cluster with none answers at
		// once rather than waiting a minute.
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepGatewayAddress,
			Mode:      lb.Mode,
			What:      fmt.Sprintf("Gateway %s/%s getting an address", GatewayNamespace, GatewayName),
		})
		plan.Steps = append(plan.Steps, metalLBSteps(pins, UnlessGatewayAddress)...)
	case actionVerify:
		// external mode: no MetalLB ever. The question verifies the operator's
		// address layer against the Gateway box writes on this run.
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepGatewayAddress,
			Mode:      lb.Mode,
			What:      fmt.Sprintf("Gateway %s/%s getting an address", GatewayNamespace, GatewayName),
		})
	}

	// Every path ends the same way: the address the Gateway reports has to
	// answer HTTP from this host, whether it came from box's MetalLB, the
	// operator's, or a cloud provider's.
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentLoadBalancer,
		Kind:      StepReachable,
		What:      "the Gateway answering HTTP on 80",
	})

	// The admission policy is the platform layer's tenancy contract: root
	// installs it once, `box adduser` binds it per namespace. Applying it on
	// every run is safe (server-side apply, same bytes, no change) and it is
	// what repairs a platform whose policy was removed.
	policy, err := ParseManifests(admissionPolicy)
	if err != nil {
		return Plan{}, fmt.Errorf("the admission policy manifest: %w", err)
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentGatewayAPI,
		Kind:      StepManifests,
		What:      "ValidatingAdmissionPolicies in s5n-system (the grant, enforced by the cluster)",
		Documents: policy,
	})

	return plan, nil
}

// gatewayStepName is how the plan names the one manifest step that is now on
// every path: with the class box creates, or with the class it found.
func gatewayStepName(class string, withClass bool) string {
	if withClass {
		return fmt.Sprintf("GatewayClass %s and Gateway %s/%s", class, GatewayNamespace, GatewayName)
	}
	return fmt.Sprintf("Gateway %s/%s on GatewayClass %s", GatewayNamespace, GatewayName, class)
}

// metalLBSteps are the two writes the address layer needs: MetalLB itself and
// the pool its speaker advertises. The condition is empty when box owns the
// layer outright and non-empty when the Gateway may get an address without it.
func metalLBSteps(pins release.Versions, unless Condition) []Step {
	return []Step{
		{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepRelease,
			Unless:    unless,
			Release: Release{
				Name:      MetalLBRelease,
				Namespace: MetalLBNamespace,
				Ref:       MetalLBRef,
				Chart:     MetalLBChart,
				Version:   pins.MetalLB,
			},
			Wait: []Wait{
				{Kind: WaitDeployment, Namespace: MetalLBNamespace, Name: MetalLBRelease + "-controller"},
				{Kind: WaitDaemonSet, Namespace: MetalLBNamespace, Name: MetalLBRelease + "-speaker"},
			},
		},
		{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepPool,
			Unless:    unless,
			What:      fmt.Sprintf("IPAddressPool %s (the node's InternalIP as a /32) and L2Advertisement %s", MetalLBPoolName, MetalLBL2Name),
		},
	}
}

// preflightAccess is the RBAC box needs for what the plan will write. It is a
// fixed list rather than an enumeration of every chart object: these are the
// cluster-scoped rights and the per-namespace writes a chart install needs, and
// any identity that has them is cluster-admin enough for the platform.
//
// Every entry is an apply, not a create. box writes with server-side apply, and
// the API server authorizes that as create for an object it has to make and as
// patch for one that already exists -- which on every run after the first is
// what the Gateway, the Certificate and the admission policy are. Asking only
// for create passed an identity that could not patch, and the write failed
// after the operator had confirmed the plan.
func preflightAccess(plan Plan) []Access {
	access := []Access{
		{Verb: kube.VerbApply, Resource: "namespaces", What: "namespaces"},
		{Verb: kube.VerbApply, Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", What: "custom resource definitions"},
		{Verb: kube.VerbApply, Group: "rbac.authorization.k8s.io", Resource: "clusterroles", What: "cluster roles"},
		{Verb: kube.VerbApply, Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings", What: "cluster role bindings"},
		{Verb: kube.VerbApply, Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies", What: "validating admission policies"},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "gatewayclasses", What: "gateway classes"},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: GatewayNamespace, What: "gateways"},
		{Verb: kube.VerbApply, Group: "metallb.io", Resource: "ipaddresspools", Namespace: MetalLBNamespace, What: "MetalLB address pools"},
		{Verb: kube.VerbApply, Group: "metallb.io", Resource: "l2advertisements", Namespace: MetalLBNamespace, What: "MetalLB L2 advertisements"},
		{Verb: kube.VerbApply, Group: "cert-manager.io", Resource: "clusterissuers", What: "cert-manager ClusterIssuers"},
	}
	seen := map[string]bool{}
	namespaces := []string{}
	for _, s := range plan.Steps {
		if s.Kind != StepRelease {
			continue
		}
		if !seen[s.Release.Namespace] {
			seen[s.Release.Namespace] = true
			namespaces = append(namespaces, s.Release.Namespace)
		}
	}
	for _, ns := range namespaces {
		for _, r := range []string{"deployments", "daemonsets", "services", "secrets", "configmaps", "serviceaccounts"} {
			access = append(access, Access{Verb: kube.VerbApply, Resource: r, Namespace: ns, What: r + " in " + ns})
		}
	}
	return access
}

// preflight is the gate before the confirm and the first write. The question,
// the verbs and the refusal live in internal/kube: `box apply`'s storage check
// and root's per-host TLS write ask the same one.
func preflight(ctx context.Context, cl Cluster, access []Access) error {
	return kube.Preflight(ctx, cl, access)
}
