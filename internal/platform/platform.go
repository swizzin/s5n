// Package platform applies the platform layer box owns: the pieces k0s does not
// ship (PLAN.md, "platform layer"). internal/bootstrap decides which of the four
// capabilities is there and which box must install; this is the half that writes.
//
// It applies only what the decision says "install": a component it uses is left alone
// -- box never touches someone else's Gateway or LoadBalancer -- and a conflict stops
// the run before the first write. The flow is a decision over small interfaces
// (Releases, Cluster, Confirmer) with the Helm and client-go halves beside it, so the
// plan, the order, the refusals and "a refusal writes nothing" are tested from stubs.
// Versions come from the chart release's platform.yaml; no chart tarball is embedded.
package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/wait"
)

// The platform releases: where each chart comes from and what box calls the result.
// The versions are the chart release's platform.yaml, so a bump moves one chart line.
const (
	// EnvoyGatewayChart ships the Gateway API CRDs at the channel the controller is built
	// against; a separately pinned bundle could drift from that set.
	EnvoyGatewayChart     = "gateway-helm"
	EnvoyGatewayRef       = "oci://docker.io/envoyproxy/gateway-helm"
	EnvoyGatewayRelease   = "envoy-gateway"
	EnvoyGatewayNamespace = "envoy-gateway-system"

	// MetalLBChart is the MetalLB chart in L2 mode: `type: LoadBalancer` on bare metal.
	MetalLBChart     = "metallb"
	MetalLBRef       = "https://metallb.github.io/metallb"
	MetalLBRelease   = "metallb"
	MetalLBNamespace = "metallb-system"

	// CertManagerChart ships its CRDs, and config.gatewayAPI.enabled is what lets it
	// solve HTTP-01 through the Gateway's HTTPRoute.
	CertManagerChart     = "cert-manager"
	CertManagerRef       = "oci://quay.io/jetstack/charts/cert-manager"
	CertManagerRelease   = "cert-manager"
	CertManagerNamespace = "cert-manager"

	// The Gateway the chart's HTTPRoutes attach to (install/platform/gateway/gateway.yaml).
	GatewayNamespace = "s5n-gateway"
	GatewayName      = "s5n"

	// MetalLBPoolName and MetalLBL2Name are box's pool and advertisement.
	MetalLBPoolName = "s5n-pool"
	MetalLBL2Name   = "s5n-l2"
)

// Condition names what has to be false for a step to run: a question only a write can
// settle is printed as `?` rather than guessed at.
type Condition string

const (
	// UnlessGatewayAddress: the step runs only if the Gateway box applied gets no address
	// of its own, which its status answers once it exists.
	UnlessGatewayAddress Condition = "unless the Gateway gets an address without it"
)

// Step kinds, in the order box applies them.
type StepKind string

const (
	// StepRelease is a Helm release: install-or-upgrade, with Helm's wait.
	StepRelease StepKind = "release"
	// StepManifests is an embedded manifest applied with server-side apply.
	StepManifests StepKind = "manifests"
	// StepPool is MetalLB's IPAddressPool/L2Advertisement, from the node's InternalIP.
	StepPool StepKind = "metallb-pool"
	// StepIssuers is cert-manager's two ClusterIssuers, retried like the pool: the
	// webhook's endpoint can be Ready while its server still refuses.
	StepIssuers StepKind = "cert-manager-issuers"
	// StepGatewayAddress asks whether the Gateway gets an address without MetalLB; the
	// answer decides the conditional steps.
	StepGatewayAddress StepKind = "gateway-address"
	// StepReachable is the Gateway's address answering HTTP from this host.
	StepReachable StepKind = "gateway-reachable"
	// StepFirewall trusts the cluster's networks where box installed k0s and firewalld
	// runs, before anything is installed behind it.
	StepFirewall StepKind = "host-firewall"
)

// WaitKind is what readiness means for one object.
type WaitKind string

const (
	WaitDeployment  WaitKind = "deployment"
	WaitDaemonSet   WaitKind = "daemonset"
	WaitCertificate WaitKind = "certificate"
)

// Wait is one readiness check after a step; the cluster implementation bounds it.
type Wait struct {
	Kind      WaitKind
	Namespace string
	Name      string
	// Timeout bounds this wait when non-zero, else the client's own bound; a caller with
	// a shorter budget uses it.
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

// String is how the plan names a release.
func (r Release) String() string {
	return fmt.Sprintf("%s %s (Helm, %s)", r.Name, r.Version, r.Namespace)
}

// Step is one item of the plan, in application order.
type Step struct {
	Component bootstrap.PlatformComponent
	Kind      StepKind
	// What names the step for progress and done lines, with exact object names.
	What string
	// Plan is what the printed plan calls the step when What names objects. Empty
	// means What.
	Plan    string
	Release Release
	// Documents are the step's objects, parsed and pointed at the chosen class at plan
	// time, so the apply path is free of YAML.
	Documents []map[string]any
	Wait      []Wait
	// Unless makes the step conditional; MetalLB and its pool are skipped when the
	// Gateway has an address without them.
	Unless Condition
	// Mode is the ownership a questioning step acts under: external's address comes from
	// elsewhere, auto's absence is box's to fix.
	Mode bootstrap.PlatformMode
	// InstalledVersion is what a deployed release has; at its pin, no Helm call.
	InstalledVersion string
	// Foreign marks a same-named release box did not install: never upgraded.
	Foreign bool
	// CIDRs are the networks a StepFirewall trusts, from bootstrap's k0s config.
	CIDRs []string
}

// Plan is what bootstrap will apply, in order.
type Plan struct {
	Steps []Step
	// Warnings are what box saw and will not change, printed with the plan (a firewalld
	// it does not own, with the commands that would open it).
	Warnings []string
	// NotHandled names decided installs box does not do: printed, never dropped.
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
	// Await blocks until the object is ready or the bound expires.
	Await(ctx context.Context, w Wait) error
	// CertificateReady answers the Ready condition with cert-manager's reason; bootstrap
	// polls it for the root, the per-host step reads it after a wait.
	CertificateReady(ctx context.Context, namespace, name string) (bool, string, error)
}

// Releases is the Helm side.
type Releases interface {
	// Version is a deployed release's chart version and whether box owns it.
	Version(ctx context.Context, name, namespace string) (version string, owned, exists bool, err error)
	// Install installs or upgrades the release, waits, and stamps box's label.
	Install(ctx context.Context, spec Release) error
}

// Firewall is the one step that changes the host's filter, permanently and now
// (bootstrap.Firewalld); reading it is bootstrap's.
type Firewall interface {
	Trust(ctx context.Context, cidrs []string) error
}

// Confirmer asks the one question before the first write; the platform layer is asked
// with no, since it changes a running host.
type Confirmer interface {
	Ask(question string, def bool) (bool, error)
}

// Decision is one `box bootstrap`'s platform half.
type Decision struct {
	// Open builds the cluster client, contacting nothing, only when something is read or
	// written.
	Open     func() (Cluster, error)
	Releases Releases
	// Probe builds the Gateway probe only from a step that needs it, contacting nothing.
	Probe   func() (GatewayProbe, error)
	Confirm Confirmer
	// Say is every line; policy stays here, appearance is the reporter's.
	Say say.Reporter
	// GatewayClass is the class bootstrap chose; the class is created only when this run
	// installs the implementation.
	GatewayClass string
	// Pins is the release's platform pins: the versions this plan installs, a platform
	// patch being a chart release.
	Pins release.Versions
	// Firewall is what bootstrap found: firewalld running, the networks to trust, or the
	// warning for a host box does not change.
	Firewall bootstrap.FirewallState
	// HostFirewall carries out a StepFirewall, only where box installed k0s.
	HostFirewall Firewall
	// ACMEServer points s5n-letsencrypt elsewhere; empty in every real run, it exists for
	// the e2e's Pebble.
	ACMEServer string
	// CallerConfirms says the flow shows the plan and asks itself, as a migration's
	// single confirm does.
	CallerConfirms bool

	// AddressTimeout is how long the Gateway has to say it has no address; zero means
	// the constant.
	AddressTimeout time.Duration
	// ProbeTimeout is the whole wait for the Gateway to be Programmed; zero means
	// ProbeTimeout.
	ProbeTimeout time.Duration
	// ReachTimeout is how long that address gets to answer HTTP; zero means
	// ReachTimeout.
	ReachTimeout time.Duration
	// PoolTimeout bounds the MetalLB webhook refusing its pool; zero means two minutes.
	PoolTimeout time.Duration
	// WebhookTimeout bounds cert-manager's webhook refusing the first issuer apply; zero
	// means two minutes.
	WebhookTimeout time.Duration
	// RootTimeout bounds the root Certificate's wait before the CA issuer is applied.
	RootTimeout time.Duration
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
	plan = withFirewall(plan, d.Firewall)
	res.NotHandled = plan.NotHandled

	if err := d.resolve(ctx, &plan); err != nil {
		return res, err
	}
	res.Plan = plan
	r := d.report()
	if !d.CallerConfirms {
		printPlan(r, plan)
	}

	if len(plan.Steps) == 0 {
		return res, nil
	}

	cl, err := d.Open()
	if err != nil {
		return res, err
	}

	// The checks below are reads, the last thing before the confirm: a kubeconfig that
	// cannot create a CRD is refused before the operator is asked.
	if err := preflight(ctx, cl, preflightAccess(plan)); err != nil {
		return res, err
	}

	if !d.CallerConfirms {
		ok, err := d.Confirm.Ask("install the platform layer?", false)
		if err != nil {
			return res, err
		}
		if !ok {
			r.Line("oh heck, nvm")
			return res, ErrCancelled
		}
	}

	// answered is what the plan's question found; every step with Unless runs or not
	// depending on it.
	answered := ""
	runs := newComponentRuns(r, plan)
	for i, s := range plan.Steps {
		if s.Unless != "" && answered != "" {
			continue
		}
		run := runs.begin(i, s)
		switch s.Kind {
		case StepRelease:
			if s.Foreign {
				return res, fmt.Errorf("a Helm release named %s already exists in %s, but box did not install it (no %s=%s label): box will not upgrade another operator's release. Remove it, or use the implementation it provides",
					s.Release.Name, s.Release.Namespace, kube.ManagedByLabel, kube.ManagedBy)
			}
			if AtPin(s.InstalledVersion, s.Release.Version) {
				r.Detail(fmt.Sprintf("%s in %s: already at %s", s.Release.Name, s.Release.Namespace, s.Release.Version))
				res.Verified = append(res.Verified, s.Release.Name)
			} else {
				r.Detail("installing " + s.Release.String())
				if err := d.Releases.Install(ctx, s.Release); err != nil {
					run.fail(err)
					return res, err
				}
				r.Detail(fmt.Sprintf("installed %s in %s", s.Release.Name, s.Release.Namespace))
				res.Installed = append(res.Installed, s.Release.Name)
			}
			for _, w := range s.Wait {
				if err := d.await(ctx, cl, w); err != nil {
					run.fail(err)
					return res, err
				}
			}
		case StepManifests:
			r.Detail("applying " + s.What)
			if err := cl.Apply(ctx, s.Documents); err != nil {
				run.fail(err)
				return res, fmt.Errorf("%s: %w", s.What, err)
			}
			res.Installed = append(res.Installed, s.What)
			for _, w := range s.Wait {
				if err := d.await(ctx, cl, w); err != nil {
					run.fail(err)
					return res, err
				}
			}
		case StepPool:
			if err := d.applyPool(ctx, cl); err != nil {
				run.fail(err)
				return res, err
			}
			res.Installed = append(res.Installed, "MetalLB address pool")
		case StepIssuers:
			if err := d.applyIssuers(ctx, cl, s.Documents); err != nil {
				run.fail(err)
				return res, err
			}
			res.Installed = append(res.Installed, s.What)
		case StepFirewall:
			if d.HostFirewall == nil {
				return res, errors.New("the plan trusts the cluster's pod and service networks with firewalld, and this run has no way to change the host's firewall")
			}
			if err := d.HostFirewall.Trust(ctx, s.CIDRs); err != nil {
				run.fail(err)
				return res, err
			}
			r.Detail("firewalld trusts " + strings.Join(s.CIDRs, " and "))
			res.Installed = append(res.Installed, "firewalld's trusted zone")
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
			// The address is the one fact worth keeping: what the person types.
			runs.fact(address)
		}
		runs.end(i)
	}
	runs.finish()
	return res, nil
}

// componentRun is one component's row: started at its first step and finished at its
// last, so cert-manager's install, waits and issuers read as one line (rule 4); the
// steps are the log's (rule 5).
type componentRun struct {
	handle say.Handle
	// last is the index of the component's last step.
	last int
	// facts are what the row says besides the duration.
	facts []string
}

// componentRuns keeps one row per component for the length of one apply.
type componentRuns struct {
	r     say.Reporter
	last  map[string]int
	runs  map[string]*componentRun
	order []string
	// current is the component the step being applied belongs to, if any.
	current *componentRun
	// currentKey is that component's key, "" for a step of its own.
	currentKey string
}

// newComponentRuns reads the plan once to find each component's last step.
func newComponentRuns(r say.Reporter, plan Plan) *componentRuns {
	c := &componentRuns{
		r:    r,
		last: make(map[string]int, len(planComponents)),
		runs: make(map[string]*componentRun, len(planComponents)),
	}
	for i, s := range plan.Steps {
		if key := planGroupOf(s); key != "" {
			c.last[key] = i
		}
	}
	return c
}

// begin starts this step's component row, or returns the one running; a step in no
// component is its own row.
func (c *componentRuns) begin(i int, s Step) *componentRun {
	key := planGroupOf(s)
	c.currentKey = key
	if key == "" {
		c.current = &componentRun{last: i, handle: c.r.Start(say.Op{Name: planText(s)})}
		return c.current
	}
	if run, ok := c.runs[key]; ok {
		c.current = run
		return run
	}
	run := &componentRun{last: c.last[key], handle: c.r.Start(say.Op{Name: componentName(key)})}
	c.runs[key] = run
	c.order = append(c.order, key)
	c.current = run
	return run
}

// end finishes the row of the step that just ran, when it was the component's
// last step.
func (c *componentRuns) end(i int) {
	if c.current == nil || i != c.current.last {
		return
	}
	c.current.done()
}

// fact adds one fact to the running component's row, or the Gateway's when it has
// one: the address is what the Gateway answers on.
func (c *componentRuns) fact(text string) {
	if text == "" {
		return
	}
	target := c.current
	if run, ok := c.runs[groupGateway]; ok {
		target = run
	}
	if target == nil {
		return
	}
	target.facts = append(target.facts, text)
}

// finish closes a row whose last step never ran, so a stopped run leaves no
// half-drawn component.
func (c *componentRuns) finish() {
	for _, key := range c.order {
		c.runs[key].done()
	}
}

func (c *componentRun) done() {
	if c.handle == nil {
		return
	}
	c.handle.Done(strings.Join(c.facts, " · "))
	c.handle = nil
}

func (c *componentRun) fail(err error) {
	if c.handle == nil {
		return
	}
	c.handle.Failed(err)
	c.handle = nil
}

// componentName is the name a component's row carries: the one the plan used.
func componentName(key string) string {
	for _, c := range planComponents {
		if c.key == key {
			return c.name
		}
	}
	return key
}

// resolve reads which releases are deployed, so the plan says install or verify. A
// release at another version is an upgrade: Helm's action, one write.
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
			// A same-named release box did not install is the operator's. A conditional
			// step may never run, so it is only marked; an unconditional one is refused
			// before the confirm.
			s.Foreign = true
			if s.Unless == "" {
				return fmt.Errorf("a Helm release named %s already exists in %s, but box did not install it (no %s=%s label): box will not upgrade another operator's release. Remove it, or use the implementation it provides",
					s.Release.Name, s.Release.Namespace, kube.ManagedByLabel, kube.ManagedBy)
			}
		}
	}
	if boxMetalLB {
		// Box's own MetalLB is already there: verify or upgrade it, and drop the
		// Gateway question, since box's MetalLB is exactly what gives the Gateway an
		// address and keeping the question would skip its upgrade.
		plan.settle()
	}
	return nil
}

// settle drops the Gateway question and makes every conditional step unconditional.
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

// gatewayAddress asks whether the Gateway got an address without box's MetalLB: in
// auto mode no address installs MetalLB, in external mode it is a refusal.
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
		// External mode said something else hands out addresses, and a slow
		// LoadBalancer is still a working one: it gets the whole wait.
		quick = timeout
	}
	r.Detail(fmt.Sprintf("waiting up to %s for Gateway %s/%s to report an address", quick, GatewayNamespace, GatewayName))
	answer, err := p.Address(ctx, quick)
	if err != nil {
		return answer, err
	}
	if settling(answer) && timeout > quick {
		// Only "no address, AddressNotAssigned" is an answer at the short deadline;
		// anything else gets the rest of the whole wait.
		r.Detail(fmt.Sprintf("the Gateway is still settling (%s); waiting up to another %s", reasonOr(answer.Reason), timeout-quick))
		answer, err = p.Address(ctx, timeout-quick)
		if err != nil {
			return answer, err
		}
	} else {
		timeout = quick
	}
	switch {
	case answer.Address != "" && answer.Programmed() && mode == bootstrap.ModeExternal:
		r.Detail("the Gateway has an address from the external LoadBalancer: " + answer.Address)
	case answer.Address != "" && answer.Programmed():
		r.Detail("the Gateway already has an address: " + answer.Address + "; MetalLB and its address pool are not installed")
	case answer.Address != "" || !deferredReason(answer.Reason):
		// An address the Gateway will not call Programmed, or no address for a reason
		// an address layer cannot answer: installing MetalLB over either hides it.
		return answer, gatewayFailure(answer, timeout)
	case mode == bootstrap.ModeExternal:
		return answer, fmt.Errorf("platform.loadbalancer.mode is external, but Gateway %s/%s got no address within %s (%s): something else has to assign one (MetalLB, k3s servicelb or a cloud provider). Install it and run `box bootstrap` again, or set platform.loadbalancer.mode=auto to let box install MetalLB",
			GatewayNamespace, GatewayName, timeout, reasonOr(answer.Reason))
	default:
		r.Detail(fmt.Sprintf("the Gateway has no address (%s) after waiting up to %s; installing MetalLB", reasonOr(answer.Reason), timeout))
	}
	return answer, nil
}

// reachable is the last check on every path: the Gateway's address must answer HTTP
// from this host, not only in the API server's opinion.
func (d *Decision) reachable(ctx context.Context) (string, error) {
	p, err := d.probe()
	if err != nil {
		return "", err
	}
	timeout := d.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	// MetalLB and its pool are in place by now, so a second AddressNotAssigned is the
	// end of what box can do.
	r := d.report()
	r.Detail(fmt.Sprintf("waiting up to %s for Gateway %s/%s to be Programmed with its address", timeout, GatewayNamespace, GatewayName))
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
	r.Detail(fmt.Sprintf("checking that the Gateway answers on %s:80 from this host", answer.Address))
	code, err := p.Reachable(ctx, answer.Address, retry)
	if err != nil {
		return "", fmt.Errorf("the Gateway has address %s, but nothing answers on %s:80 from this host: %w", answer.Address, answer.Address, err)
	}
	r.Detail(fmt.Sprintf("the Gateway answers on %s:80 (HTTP %d)", answer.Address, code))
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

// gatewayFailure is the refusal when an address layer will not fix the Gateway: it has
// an address but is not Programmed, or none and a reason MetalLB cannot answer.
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

// settling says a short-deadline answer is not one to act on: the Gateway exists,
// is neither Programmed nor plainly unassigned. Envoy reports NoResources with its
// address assigned while the proxy rolls out, the common case where a LoadBalancer
// already exists.
func settling(a GatewayAnswer) bool {
	if a.missing {
		return false
	}
	if a.Address != "" && a.Programmed() {
		return false
	}
	return a.Address != "" || a.Reason != gatewayAddressNotAssigned
}

// deferredReason is a reason about the address layer still arriving, not the data
// plane: anything else (NoResources, AddressNotUsable) makes box stop rather than
// installing MetalLB over it.
func deferredReason(reason string) bool {
	return reason == "" || reason == gatewayAddressNotAssigned
}

// reasonOr is the Gateway's own word, or "not reconciled yet" when it has none.
func reasonOr(reason string) string {
	if reason == "" {
		return "not reconciled yet"
	}
	return reason
}

func (d *Decision) await(ctx context.Context, cl Cluster, w Wait) error {
	r := d.report()
	r.Detail("waiting for " + w.String())
	if err := cl.Await(ctx, w); err != nil {
		return fmt.Errorf("%s: %w", w, err)
	}
	r.Detail(w.String() + ": rolled out")
	return nil
}

// retryApply applies docs until the API server accepts them or the bound expires.
// Both webhooks -- MetalLB's pool and cert-manager's issuers -- are served by pods
// Ready before they can accept a connection, and the chart's wait only proves
// Readiness. The last refusal is returned: it is the operator's to fix.
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

// applyPool writes MetalLB's pool and advertisement, retried until the webhook answers.
func (d *Decision) applyPool(ctx context.Context, cl Cluster) error {
	r := d.report()
	r.Detail("applying the MetalLB address pool")
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
	if err := d.retryApply(ctx, cl, docs, wait); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("MetalLB refused its address pool for %s: %w (the chart is installed; fix the cause and run `box bootstrap` again)", wait, err)
	}
	r.Detail(fmt.Sprintf("applied IPAddressPool %s (%s/32) and L2Advertisement %s", MetalLBPoolName, ip, MetalLBL2Name))
	return nil
}

func (d *Decision) applyIssuers(ctx context.Context, cl Cluster, docs []map[string]any) error {
	r := d.report()
	step := r.Start(say.Op{Label: "applying the cert-manager ClusterIssuers and box's root"})
	wait := d.WebhookTimeout
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	waves, err := splitIssuers(docs)
	if err != nil {
		step.Failed(err)
		return err
	}
	if err := d.retryApply(ctx, cl, waves.beforeRoot, wait); err != nil {
		step.Failed(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cert-manager's webhook refused its ClusterIssuers for %s: %w (cert-manager is installed; fix the cause and run `box bootstrap` again)", wait, err)
	}
	// The root's Certificate is issued by the bootstrap issuer applied above, and the
	// CA issuer below signs with its Secret: applied first, it would point at a Secret
	// that does not exist, and every host's Certificate would wait on the wrong
	// failure. So: root Ready, then the issuer that uses it.
	if err := d.awaitRoot(ctx, cl); err != nil {
		return err
	}
	if err := d.retryApply(ctx, cl, waves.ca, wait); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cert-manager's webhook refused its ClusterIssuers for %s: %w (cert-manager is installed; fix the cause and run `box bootstrap` again)", wait, err)
	}
	r.Detail(fmt.Sprintf("applied ClusterIssuers %s and %s, and the root they sign with", LetsEncryptIssuer, SelfSignedIssuer))
	return nil
}

// RootTimeout is how long box's root Certificate gets to be Ready: a local signing
// operation, so the bound is a webhook and a controller catching up, not DNS.
const RootTimeout = 2 * time.Minute

// awaitRoot waits for box's root Certificate, polling to the deadline and reporting
// cert-manager's own reason: the root is the one thing a self-signed box can never
// re-derive, so a guess wastes a run that could have said what is wrong.
func (d *Decision) awaitRoot(ctx context.Context, cl Cluster) error {
	timeout := d.RootTimeout
	if timeout <= 0 {
		timeout = RootTimeout
	}
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	r := d.report()
	r.Detail(fmt.Sprintf("waiting up to %s for Certificate %s/%s to be Ready", timeout, CertManagerNamespace, RootCertificateName))
	reason := ""
	waited, err := wait.Until(ctx, timeout, poll, func(ctx context.Context) (bool, error) {
		ready, why, err := cl.CertificateReady(ctx, CertManagerNamespace, RootCertificateName)
		if err != nil {
			// A failed read is not a slow Certificate: stop rather than spending the
			// budget on a broken client.
			return false, err
		}
		if !ready {
			reason = why
		}
		return ready, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("box's root Certificate %s/%s: %w", CertManagerNamespace, RootCertificateName, err)
	}
	if !waited.Done {
		if reason == "" {
			reason = certNotReadyReason
		}
		return fmt.Errorf("box's root Certificate %s/%s is not Ready after %s (%s): every self-signed host's certificate is issued by it, so box stopped before the CA issuer; run `box bootstrap` again once cert-manager has issued it",
			CertManagerNamespace, RootCertificateName, timeout, reason)
	}
	r.Detail(fmt.Sprintf("Certificate %s/%s: Ready", CertManagerNamespace, RootCertificateName))
	return nil
}

// Preview is the plan for a cluster whose layer is not applied yet, shown before a
// flow's one question: what Apply builds, plus the firewall line. It does not resolve
// against the cluster, since every step is an install on nothing.
func Preview(pins release.Versions, components []bootstrap.ComponentResult, class, acmeServer string, firewall bootstrap.FirewallState) (Plan, error) {
	plan, err := BuildPlan(pins, components, class, acmeServer)
	if err != nil {
		return plan, err
	}
	return withFirewall(plan, firewall), nil
}

// BuildPlan turns the component results into the ordered list of writes. Pure: no
// cluster, no clock. Only `install` produces a release; `use` and `verify` leave an
// implementation alone and `stop` refuses the run. Every version is the release's.
//
// The Gateway box owns is written on every run on the chosen class, because the
// chart's routes attach to it. The GatewayClass itself is written only when box
// installs Envoy Gateway: a class someone else's controller serves is not box's.
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

	// Envoy Gateway is both the CRDs and the controller: an install of either is one
	// release, and the case where box also creates the GatewayClass.
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

	// cert-manager is the TLS half, installed only when the decision says so (an
	// existing usable install is verified). Its CRDs ship with the chart, and
	// config.gatewayAPI.enabled is what makes the HTTP-01 solver work.
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

	// MetalLB is the address layer, which only the Gateway needs: the torrent port is a
	// NodePort, so a LoadBalancer Service exists only because the Gateway implementation
	// creates one -- which makes the Gateway's own status the probe.
	//
	// managed installs it first and asks nothing; auto applies the Gateway and installs
	// MetalLB only if it got no address; external owns nothing and only verifies.
	managedLB := lb.Action == actionInstall
	if managedLB {
		plan.Steps = append(plan.Steps, metalLBSteps(pins, "")...)
	}

	// Box's Gateway on every path: on `use` it is still box's object, since the chart's
	// HTTPRoutes name it.
	objects, err := gatewayObjects(gatewayClass, installEnvoy)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentGateway,
		Kind:      StepManifests,
		What:      gatewayStepName(gatewayClass, installEnvoy),
		Plan:      "the s5n Gateway",
		Documents: objects,
	})

	// The issuers are root's and every seedbox's Certificate names one: applied after
	// the Gateway they solve through, on every run, which is what repairs a cluster
	// whose issuers were removed.
	issuers, err := issuerObjects(acmeServer)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentCertManager,
		Kind:      StepIssuers,
		What:      fmt.Sprintf("ClusterIssuers %s and %s", LetsEncryptIssuer, SelfSignedIssuer),
		Plan:      "ClusterIssuers: Let's Encrypt, plus self-signed",
		Documents: issuers,
	})

	switch lb.Action {
	case actionProbe:
		// auto: the Gateway's status decides. Asked every run -- a re-run asks the
		// Gateway already there, and a cluster with none answers at once.
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepGatewayAddress,
			Mode:      lb.Mode,
			What:      fmt.Sprintf("Gateway %s/%s getting an address", GatewayNamespace, GatewayName),
		})
		plan.Steps = append(plan.Steps, metalLBSteps(pins, UnlessGatewayAddress)...)
	case actionVerify:
		// external: no MetalLB ever; the question verifies the operator's layer.
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentLoadBalancer,
			Kind:      StepGatewayAddress,
			Mode:      lb.Mode,
			What:      fmt.Sprintf("Gateway %s/%s getting an address", GatewayNamespace, GatewayName),
		})
	}

	// Every path ends the same way: the Gateway's address must answer HTTP from this
	// host, whoever assigned it.
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentLoadBalancer,
		Kind:      StepReachable,
		What:      "the Gateway answering HTTP on 80",
		Plan:      "HTTP on :80",
	})

	// The admission policy is the tenancy contract: root installs it once and
	// `box adduser` binds it per namespace. Re-applying the same bytes is safe, and is
	// what repairs a policy that was removed.
	policy, err := ParseManifests(admissionPolicy)
	if err != nil {
		return Plan{}, fmt.Errorf("the admission policy manifest: %w", err)
	}
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentGatewayAPI,
		Kind:      StepManifests,
		What:      "ValidatingAdmissionPolicies in s5n-system (the grant, enforced by the cluster)",
		Plan:      "admission policies",
		Documents: policy,
	})

	return plan, nil
}

// withFirewall is the plan's line about the packet filter, its first step because
// everything box installs is reached through the host: kube-router routes pod-to-pod
// traffic through it, so a firewall that does not know the cluster's networks refuses
// Envoy's dial and every request behind the login fails.
//
// Only a host where box installed k0s has networks box may trust; elsewhere the plan
// carries the warning with the two commands.
func withFirewall(plan Plan, fw bootstrap.FirewallState) Plan {
	if !fw.Active {
		return plan
	}
	if len(fw.CIDRs) > 0 {
		// The plan names the zone and its networks; the screen row is the zone alone.
		line := "firewalld's trusted zone: " + strings.Join(fw.CIDRs, " and ")
		plan.Steps = append([]Step{{
			Kind:  StepFirewall,
			What:  line,
			Plan:  "firewalld's trusted zone",
			CIDRs: append([]string(nil), fw.CIDRs...),
		}}, plan.Steps...)
	}
	if fw.Warning != "" {
		plan.Warnings = append(plan.Warnings, fw.Warning)
	}
	return plan
}

// gatewayStepName names the Gateway step by the class box creates or found.
func gatewayStepName(class string, withClass bool) string {
	if withClass {
		return fmt.Sprintf("GatewayClass %s and Gateway %s/%s", class, GatewayNamespace, GatewayName)
	}
	return fmt.Sprintf("Gateway %s/%s on GatewayClass %s", GatewayNamespace, GatewayName, class)
}

// metalLBSteps are the address layer's two writes; the condition is empty when box
// owns the layer and set when the Gateway may get an address without it.
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
				// box announces over L2 and never peers over BGP. frr-k8s, on by
				// default since 0.15, is BGP's: a DaemonSet, a webhook and a status
				// cleaner the install would wait on for nothing.
				Values: map[string]any{
					"frrk8s": map[string]any{"enabled": false},
				},
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
			Plan:      "IPAddressPool: this node's IP as a /32, over L2",
		},
	}
}

// preflightAccess is the RBAC the plan needs, a fixed list rather than every chart
// object: any identity with these is cluster-admin enough for the platform.
//
// Every entry is an apply, not a create: the API server authorizes an apply as create
// for a new object and patch for an existing one, and asking only for create passed an
// identity that could not patch and failed after the confirm.
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

// preflight is the gate before the confirm and the first write; the question, the
// verbs and the refusal live in internal/kube.
func preflight(ctx context.Context, cl Cluster, access []Access) error {
	return kube.Preflight(ctx, cl, access)
}
