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

	// The Gateway the chart's HTTPRoutes attach to (hack/envoy-gateway.yaml,
	// which the kind e2e applies and box embeds).
	GatewayNamespace = "s5n-gateway"
	GatewayName      = "s5n"

	// FieldManager is box's server-side-apply manager. It is what makes a
	// second bootstrap field-for-field the same write as the first.
	FieldManager = "box"

	// MetalLBPoolName and MetalLBL2Name are box's pool and advertisement.
	MetalLBPoolName = "s5n-pool"
	MetalLBL2Name   = "s5n-l2"
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
)

// WaitKind is what readiness means for one object.
type WaitKind string

const (
	WaitDeployment WaitKind = "deployment"
	WaitDaemonSet  WaitKind = "daemonset"
	WaitGateway    WaitKind = "gateway"
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
	case WaitGateway:
		return fmt.Sprintf("Gateway %s/%s", w.Namespace, w.Name)
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
	Manifests []byte
	Wait      []Wait
	// InstalledVersion is filled in by resolution: the chart version a
	// deployed release already has. Equal to Release.Version means the step
	// is verification only, and no Helm call happens.
	InstalledVersion string
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
	// Version returns the chart version of a deployed release, and false when
	// the release is not installed.
	Version(ctx context.Context, name, namespace string) (string, bool, error)
	// Install installs or upgrades the release and waits for its workloads.
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
	Confirm  Confirmer
	Log      io.Writer

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
	plan, err := BuildPlan(components)
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

	for _, s := range plan.Steps {
		switch s.Kind {
		case StepRelease:
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
			docs, err := ParseManifests(s.Manifests)
			if err != nil {
				return res, fmt.Errorf("%s: %w", s.What, err)
			}
			if err := cl.Apply(ctx, docs); err != nil {
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
		}
	}
	d.logf("platform ready\n")
	return res, nil
}

// resolve reads which releases are already deployed, so the printed plan says
// install or verify instead of guessing. A release at another version is an
// upgrade: Helm's own action, one write.
func (d *Decision) resolve(ctx context.Context, plan *Plan) error {
	for i := range plan.Steps {
		s := &plan.Steps[i]
		if s.Kind != StepRelease {
			continue
		}
		v, ok, err := d.Releases.Version(ctx, s.Release.Name, s.Release.Namespace)
		if err != nil {
			return err
		}
		if ok {
			s.InstalledVersion = v
		}
	}
	return nil
}

func (d *Decision) await(ctx context.Context, cl Cluster, w Wait) error {
	d.logf("waiting for %s\n", w)
	if err := cl.Await(ctx, w); err != nil {
		return fmt.Errorf("%s: %w", w, err)
	}
	switch w.Kind {
	case WaitGateway:
		d.logf("%s: Programmed\n", w)
	default:
		d.logf("%s: rolled out\n", w)
	}
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
			if s.InstalledVersion == s.Release.Version {
				d.logf("  = %s: already at %s; verifying\n", s.Release.Name, s.Release.Version)
				continue
			}
			verb := "+"
			if s.InstalledVersion != "" {
				verb = "~"
			}
			d.logf("  %s %s\n", verb, s.Release)
		default:
			d.logf("  + %s\n", s.What)
		}
	}
	for _, line := range plan.NotHandled {
		d.logf("  ! %s\n", line)
	}
}

// BuildPlan turns the decision's component results into the ordered list of
// writes. It is pure: no cluster, no clock. Only `install` produces a step;
// `use` and `verify` are left alone, and `stop` refuses the whole run.
func BuildPlan(components []bootstrap.ComponentResult) (Plan, error) {
	var plan Plan
	byComponent := make(map[bootstrap.PlatformComponent]bootstrap.ComponentResult, len(components))
	for _, c := range components {
		if c.Action == actionStop {
			return Plan{}, fmt.Errorf("platform conflict: %s: %s", c.Component, c.Reason)
		}
		byComponent[c.Component] = c
	}

	gwAPI := byComponent[bootstrap.ComponentGatewayAPI]
	gw := byComponent[bootstrap.ComponentGateway]
	lb := byComponent[bootstrap.ComponentLoadBalancer]
	cm := byComponent[bootstrap.ComponentCertManager]

	// Envoy Gateway is both the Gateway API CRDs and the controller, so an
	// install of either capability is one Helm release.
	if gwAPI.Action == actionInstall || gw.Action == actionInstall {
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

	// MetalLB before the Gateway: the Gateway's Service only gets an address
	// once a LoadBalancer implementation answers, and Envoy Gateway reports
	// Programmed only when its data plane has one.
	if installsLoadBalancer(lb) {
		plan.Steps = append(plan.Steps,
			Step{
				Component: bootstrap.ComponentLoadBalancer,
				Kind:      StepRelease,
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
			Step{
				Component: bootstrap.ComponentLoadBalancer,
				Kind:      StepPool,
				What:      fmt.Sprintf("IPAddressPool %s (the node's InternalIP as a /32) and L2Advertisement %s", MetalLBPoolName, MetalLBL2Name),
			},
		)
	}

	if gw.Action == actionInstall {
		plan.Steps = append(plan.Steps, Step{
			Component: bootstrap.ComponentGateway,
			Kind:      StepManifests,
			What:      fmt.Sprintf("GatewayClass eg and Gateway %s/%s", GatewayNamespace, GatewayName),
			Manifests: gatewayManifest,
			Wait:      []Wait{{Kind: WaitGateway, Namespace: GatewayNamespace, Name: GatewayName}},
		})
	}

	// The admission policy is the platform layer's tenancy contract: root
	// installs it once, `box adduser` binds it per namespace. Applying it on
	// every run is safe (server-side apply, same bytes, no change) and it is
	// what repairs a platform whose policy was removed.
	plan.Steps = append(plan.Steps, Step{
		Component: bootstrap.ComponentGatewayAPI,
		Kind:      StepManifests,
		What:      "ValidatingAdmissionPolicies in s5n-system (the grant, enforced by the cluster)",
		Manifests: admissionPolicy,
	})

	if cm.Action == actionInstall {
		plan.NotHandled = append(plan.NotHandled,
			"cert-manager: box does not install it yet; install cert-manager yourself, or set platform.cert-manager.mode=external, and run bootstrap again")
	}
	return plan, nil
}

// installsLoadBalancer is the one place a step can appear without the decision
// saying "install". bootstrap's platform probe is read-only, so the
// LoadBalancer capability is reported not-probed rather than absent (deciding
// it for real means creating a throwaway Service, which is a write before the
// confirmation). In auto and managed mode the decision treats an absent
// LoadBalancer as box's to install, so bootstrap does the same here: the
// Gateway's Service has no address until something answers, and MetalLB is the
// platform's LoadBalancer. external mode is never written: box does not own
// someone else's address layer, and a missing one there is the decision's
// "unknown", not an install.
func installsLoadBalancer(c bootstrap.ComponentResult) bool {
	if c.Action == actionInstall {
		return true
	}
	return c.Component == bootstrap.ComponentLoadBalancer &&
		c.Outcome == bootstrap.OutcomeNotProbed &&
		c.Mode != bootstrap.ModeExternal
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
