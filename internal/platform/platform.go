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
// cluster. Version pinning lives in one place (the constants below) and box
// fetches charts at run time; no chart tarball is embedded.
package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/bootstrap"
)

// The pinned platform releases. hack/e2e-kind.sh and hack/e2e-box.sh install
// the same versions, so the platform the e2e accepts is the platform box
// installs. Bumping one of these means bumping the e2e's default too.
const (
	// EnvoyGatewayVersion is the Envoy Gateway chart, which ships the Gateway
	// API CRDs at the experimental channel it is built against. The chart owns
	// the CRDs on purpose: a separately pinned standard channel can drift from
	// (or conflict with) the set the controller expects.
	EnvoyGatewayVersion   = "1.9.3"
	EnvoyGatewayChart     = "gateway-helm"
	EnvoyGatewayRef       = "oci://docker.io/envoyproxy/gateway-helm"
	EnvoyGatewayRelease   = "envoy-gateway"
	EnvoyGatewayNamespace = "envoy-gateway-system"

	// MetalLBVersion is the MetalLB chart, in L2 mode: it makes
	// `type: LoadBalancer` work on bare metal with no extra addresses.
	MetalLBVersion   = "0.16.1"
	MetalLBChart     = "metallb"
	MetalLBRef       = "https://metallb.github.io/metallb"
	MetalLBRelease   = "metallb"
	MetalLBNamespace = "metallb-system"

	// The Gateway the chart's HTTPRoutes attach to (install/platform/gateway/gateway.yaml,
	// which the kind e2e applies and box embeds).
	GatewayNamespace = "s5n-gateway"
	GatewayName      = "s5n"

	// FieldManager is box's server-side-apply manager. It is what makes a
	// second bootstrap field-for-field the same write as the first.
	FieldManager = "box"

	// ReleaseOwnerLabel and ReleaseOwner are how box tells a Helm release it
	// installed from a same-named one it did not. Helm v4 stores release
	// labels beside the release record, so the marker survives upgrades and
	// is readable without parsing chart values. A release without it is the
	// operator's: box never upgrades or modifies it.
	ReleaseOwnerLabel = "app.kubernetes.io/managed-by"
	ReleaseOwner      = "box"

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
	WaitDeployment WaitKind = "deployment"
	WaitDaemonSet  WaitKind = "daemonset"
)

// Wait is one readiness check after a step. The cluster implementation bounds
// it; the decision only says what has to be true.
type Wait struct {
	Kind      WaitKind
	Namespace string
	Name      string
}

// String is how box names the object in its output.
func (w Wait) String() string {
	switch w.Kind {
	case WaitDaemonSet:
		return fmt.Sprintf("DaemonSet %s/%s", w.Namespace, w.Name)
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
	// deployed release already has. Equal to Release.Version means the step
	// is verification only, and no Helm call happens.
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
	Log     io.Writer
	// GatewayClass is the class bootstrap chose (bootstrap.Result.Gateway.
	// Class). The Gateway box applies attaches to it; the GatewayClass itself
	// is created only when this run installs its implementation.
	GatewayClass string

	// ProbeTimeout is how long the Gateway gets to report an address. Zero
	// means ProbeTimeout (60s).
	ProbeTimeout time.Duration
	// ReachTimeout is how long that address gets to answer HTTP. Zero means
	// ReachTimeout (30s).
	ReachTimeout time.Duration
	// PoolTimeout bounds how long the MetalLB webhook may refuse its pool.
	// Zero means two minutes.
	PoolTimeout time.Duration
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

func (d *Decision) logf(format string, args ...any) {
	if d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, format, args...)
}

// Apply plans, asks and applies the platform layer.
func (d *Decision) Apply(ctx context.Context, components []bootstrap.ComponentResult) (Result, error) {
	var res Result
	plan, err := BuildPlan(components, d.GatewayClass)
	if err != nil {
		return res, err
	}
	res.Plan = plan
	res.NotHandled = plan.NotHandled

	if err := d.resolve(ctx, &plan); err != nil {
		return res, err
	}
	res.Plan = plan
	d.printPlan(plan)

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
	if err := preflight(ctx, cl, plan); err != nil {
		return res, err
	}

	ok, err := d.Confirm.Ask("install the platform layer?")
	if err != nil {
		return res, err
	}
	if !ok {
		d.logf("oh heck, nvm\n")
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
					s.Release.Name, s.Release.Namespace, ReleaseOwnerLabel, ReleaseOwner)
			}
			if s.InstalledVersion == s.Release.Version {
				d.logf("= %s in %s: already at %s\n", s.Release.Name, s.Release.Namespace, s.Release.Version)
				res.Verified = append(res.Verified, s.Release.Name)
			} else {
				d.logf("installing %s\n", s.Release)
				if err := d.Releases.Install(ctx, s.Release); err != nil {
					return res, err
				}
				d.logf("installed %s in %s\n", s.Release.Name, s.Release.Namespace)
				res.Installed = append(res.Installed, s.Release.Name)
			}
			for _, w := range s.Wait {
				if err := d.await(ctx, cl, w); err != nil {
					return res, err
				}
			}
		case StepManifests:
			if err := cl.Apply(ctx, s.Documents); err != nil {
				return res, fmt.Errorf("%s: %w", s.What, err)
			}
			d.logf("applied %s\n", s.What)
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
	d.logf("platform ready\n")
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
					s.Release.Name, s.Release.Namespace, ReleaseOwnerLabel, ReleaseOwner)
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
	timeout := d.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	d.logf("waiting up to %s for Gateway %s/%s to report an address\n", timeout, GatewayNamespace, GatewayName)
	answer, err := p.Address(ctx, timeout)
	if err != nil {
		return answer, err
	}
	switch {
	case answer.Address != "" && answer.Programmed() && mode == bootstrap.ModeExternal:
		d.logf("the Gateway has an address from the external LoadBalancer: %s\n", answer.Address)
	case answer.Address != "" && answer.Programmed():
		d.logf("the Gateway already has an address: %s; MetalLB and its address pool are not installed\n", answer.Address)
	case answer.Address != "" || !deferredReason(answer.Reason):
		// An address the Gateway will not call Programmed, or no address for a
		// reason an address layer cannot answer: both are failures at the
		// deadline, and installing MetalLB over one would only hide it.
		return answer, gatewayFailure(answer, timeout)
	case mode == bootstrap.ModeExternal:
		return answer, fmt.Errorf("platform.loadbalancer.mode is external, but Gateway %s/%s got no address within %s (%s): something else has to assign one (MetalLB, k3s servicelb or a cloud provider). Install it and run `box bootstrap` again, or set platform.loadbalancer.mode=auto to let box install MetalLB",
			GatewayNamespace, GatewayName, timeout, reasonOr(answer.Reason))
	default:
		d.logf("the Gateway has no address (%s) after waiting up to %s; installing MetalLB\n", reasonOr(answer.Reason), timeout)
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
	d.logf("waiting up to %s for Gateway %s/%s to be Programmed with its address\n", timeout, GatewayNamespace, GatewayName)
	answer, err := p.Address(ctx, timeout)
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
	d.logf("checking that the Gateway answers on %s:80 from this host\n", answer.Address)
	code, err := p.Reachable(ctx, answer.Address, retry)
	if err != nil {
		return "", fmt.Errorf("the Gateway has address %s, but nothing answers on %s:80 from this host: %w", answer.Address, answer.Address, err)
	}
	d.logf("the Gateway answers on %s:80 (HTTP %d)\n", answer.Address, code)
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
	d.logf("waiting for %s\n", w)
	if err := cl.Await(ctx, w); err != nil {
		return fmt.Errorf("%s: %w", w, err)
	}
	d.logf("%s: rolled out\n", w)
	return nil
}

// applyPool writes MetalLB's pool and advertisement, retrying until the
// webhook answers. The controller's webhook is served by a pod that is Ready
// before it can accept connections (it loads its certificate first), and the
// chart's wait only proves the pod is Ready. Retrying is the same robustness
// hack/e2e-kind.sh needed; here it is bounded and box-worded.
func (d *Decision) applyPool(ctx context.Context, cl Cluster) error {
	ip, err := cl.PrimaryInternalIP(ctx)
	if err != nil {
		return fmt.Errorf("MetalLB address pool: %w", err)
	}
	docs, err := ParseManifests(poolManifest(ip))
	if err != nil {
		return fmt.Errorf("MetalLB address pool: %w", err)
	}
	wait := d.PoolTimeout
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		err = cl.Apply(ctx, docs)
		if err == nil {
			d.logf("applied IPAddressPool %s (%s/32) and L2Advertisement %s\n", MetalLBPoolName, ip, MetalLBL2Name)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("MetalLB refused its address pool for %s: %w (the chart is installed; fix the cause and run `box bootstrap` again)", wait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (d *Decision) printPlan(plan Plan) {
	if len(plan.Steps) == 0 && len(plan.NotHandled) == 0 {
		return
	}
	d.logf("platform plan:\n")
	for _, s := range plan.Steps {
		switch s.Kind {
		case StepRelease:
			if s.Foreign {
				// Only a conditional foreign release reaches the plan: the
				// unconditional one is refused in resolve.
				d.logf("  ? %s, %s (a release box did not install already exists; box will not upgrade it)\n", s.Release, s.Unless)
				continue
			}
			if s.InstalledVersion == s.Release.Version {
				d.logf("  = %s: already at %s; verifying\n", s.Release.Name, s.Release.Version)
				continue
			}
			// A conditional release is never an upgrade: resolve() drops the
			// condition as soon as the release is there, so `?` never has to
			// carry an install-or-upgrade mark.
			if s.Unless != "" {
				d.logf("  ? %s, %s\n", s.Release, s.Unless)
				continue
			}
			verb := "+"
			if s.InstalledVersion != "" {
				verb = "~"
			}
			d.logf("  %s %s\n", verb, s.Release)
		case StepGatewayAddress:
			// A question the plan cannot answer yet: box finds out after the
			// Gateway is applied, and the `?` steps below depend on the answer.
			d.logf("  ? %s\n", s.What)
		default:
			if s.Unless != "" {
				d.logf("  ? %s, %s\n", s.What, s.Unless)
				continue
			}
			d.logf("  + %s\n", s.What)
		}
	}
	for _, line := range plan.NotHandled {
		d.logf("  ! %s\n", line)
	}
}

// BuildPlan turns the decision's component results into the ordered list of
// writes. It is pure: no cluster, no clock. Only `install` produces a release;
// `use` and `verify` leave an implementation alone, and `stop` refuses the
// whole run.
//
// gatewayClass is the class bootstrap chose (bootstrap.Result.Gateway). The
// Gateway box owns is written on every run, on that class: the chart's routes
// attach to box's Gateway, so it has to exist whether box installed the
// implementation or found one. The GatewayClass is written only when box
// installs Envoy Gateway, because a class someone else's controller serves is
// the operator's and must not be rewritten.
func BuildPlan(components []bootstrap.ComponentResult, gatewayClass string) (Plan, error) {
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
				Version:   EnvoyGatewayVersion,
			},
			Wait: []Wait{{Kind: WaitDeployment, Namespace: EnvoyGatewayNamespace, Name: EnvoyGatewayRelease}},
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
		plan.Steps = append(plan.Steps, metalLBSteps("")...)
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
		plan.Steps = append(plan.Steps, metalLBSteps(UnlessGatewayAddress)...)
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

	if cm.Action == actionInstall {
		plan.NotHandled = append(plan.NotHandled,
			"cert-manager: box does not install it yet; install cert-manager yourself, or set platform.cert-manager.mode=external, and run bootstrap again")
	}
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
func metalLBSteps(unless Condition) []Step {
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
				Version:   MetalLBVersion,
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
func preflightAccess(plan Plan) []Access {
	access := []Access{
		{Verb: "create", Resource: "namespaces", What: "namespaces"},
		{Verb: "create", Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", What: "custom resource definitions"},
		{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterroles", What: "cluster roles"},
		{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings", What: "cluster role bindings"},
		{Verb: "create", Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies", What: "validating admission policies"},
		{Verb: "create", Group: "gateway.networking.k8s.io", Resource: "gatewayclasses", What: "gateway classes"},
		{Verb: "create", Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: GatewayNamespace, What: "gateways"},
		{Verb: "create", Group: "metallb.io", Resource: "ipaddresspools", Namespace: MetalLBNamespace, What: "MetalLB address pools"},
		{Verb: "create", Group: "metallb.io", Resource: "l2advertisements", Namespace: MetalLBNamespace, What: "MetalLB L2 advertisements"},
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
			access = append(access, Access{Verb: "create", Resource: r, Namespace: ns, What: r + " in " + ns})
		}
	}
	return access
}

func preflight(ctx context.Context, cl Cluster, plan Plan) error {
	var denied []string
	for _, a := range preflightAccess(plan) {
		ok, err := cl.Allowed(ctx, a)
		if err != nil {
			return fmt.Errorf("platform pre-flight: SelfSubjectAccessReview: %w", err)
		}
		if !ok {
			denied = append(denied, "create "+a.What)
		}
	}
	if len(denied) == 0 {
		return nil
	}
	return fmt.Errorf("the platform layer needs cluster admin, and this kubeconfig cannot %s: run `box bootstrap` as root, or with a cluster-admin kubeconfig",
		strings.Join(denied, ", "))
}
