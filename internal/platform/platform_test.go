package platform

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	platformassets "github.com/swizzin/s5n/install/platform"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
)

// trace records the writes in the order they happened. Every assertion about
// ordering (the Gateway before the question, the question before MetalLB, a
// skipped install, the reachability check last) reads it, so a passing test is
// about side effects rather than the text box printed.
type trace struct{ steps []string }

func (t *trace) add(step string) {
	if t != nil {
		t.steps = append(t.steps, step)
	}
}

func (t *trace) String() string { return strings.Join(t.steps, ",") }

type fakeReleases struct {
	versions map[string]string
	// unowned names releases that exist but carry no box ownership label.
	unowned   map[string]bool
	installed []Release
	err       error
	tr        *trace
}

func (f *fakeReleases) key(name, namespace string) string { return namespace + "/" + name }

func (f *fakeReleases) Version(_ context.Context, name, namespace string) (string, bool, bool, error) {
	if f.err != nil {
		return "", false, false, f.err
	}
	key := f.key(name, namespace)
	v, ok := f.versions[key]
	return v, ok && !f.unowned[key], ok, nil
}

func (f *fakeReleases) Install(_ context.Context, spec Release) error {
	if f.err != nil {
		return f.err
	}
	f.installed = append(f.installed, spec)
	f.tr.add("install:" + spec.Name)
	return nil
}

type fakeCluster struct {
	deny          string
	denyNamespace string
	ip            string
	writes        [][]map[string]any
	awaits        []Wait
	allowed       []Access
	applyErrs     []error
	tr            *trace
}

func (f *fakeCluster) Allowed(_ context.Context, a Access) (bool, error) {
	f.allowed = append(f.allowed, a)
	if a.Resource == f.deny && (f.denyNamespace == "" || a.Namespace == f.denyNamespace) {
		return false, nil
	}
	return true, nil
}

func (f *fakeCluster) PrimaryInternalIP(context.Context) (string, error) {
	if f.ip == "" {
		return "192.0.2.10", nil
	}
	return f.ip, nil
}

func (f *fakeCluster) Apply(_ context.Context, docs []map[string]any) error {
	if len(f.applyErrs) > 0 {
		err := f.applyErrs[0]
		f.applyErrs = f.applyErrs[1:]
		if err != nil {
			return err
		}
	}
	f.writes = append(f.writes, docs)
	kind, _, _ := objectMeta(docs[0])
	f.tr.add("apply:" + kind)
	return nil
}

func (f *fakeCluster) Await(_ context.Context, w Wait) error {
	f.awaits = append(f.awaits, w)
	f.tr.add("await:" + w.String())
	return nil
}

type fakeConfirm struct {
	answer bool
	asked  int
	tr     *trace
}

func (f *fakeConfirm) Ask(string) (bool, error) {
	f.asked++
	f.tr.add("confirm")
	return f.answer, nil
}

// fakeProbe is the Gateway's answer: successive Address calls return
// successive answers (the last repeats), so "no address, then an address after
// MetalLB" is one sequence.
type fakeProbe struct {
	answers      []GatewayAnswer
	addressErr   error
	reachCode    int
	reachErr     error
	addressCalls int
	reachCalls   int
	reached      string
	reachRetry   time.Duration
	addressWaits []time.Duration
	tr           *trace
}

func (f *fakeProbe) Address(_ context.Context, timeout time.Duration) (GatewayAnswer, error) {
	f.addressCalls++
	f.addressWaits = append(f.addressWaits, timeout)
	f.tr.add("address")
	if f.addressErr != nil {
		return GatewayAnswer{}, f.addressErr
	}
	if len(f.answers) == 0 {
		return GatewayAnswer{}, nil
	}
	answer := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	// The probe only stops early on an address the Gateway also calls
	// Programmed, so an answer with an address means both unless a test says
	// otherwise.
	if answer.Address != "" && answer.Status == "" {
		answer.Status = "True"
	}
	return answer, nil
}

func (f *fakeProbe) Reachable(_ context.Context, address string, retry time.Duration) (int, error) {
	f.reachCalls++
	f.reached = address
	f.reachRetry = retry
	f.tr.add("reach")
	if f.reachErr != nil {
		return 0, f.reachErr
	}
	return f.reachCode, nil
}

// fixture is one Decision wired to recording fakes.
type fixture struct {
	d       *Decision
	cl      *fakeCluster
	rel     *fakeReleases
	confirm *fakeConfirm
	probe   *fakeProbe
	log     *bytes.Buffer
	tr      *trace
	// opened is how many times the cluster client was built, probes how many
	// times the Gateway probe was built.
	opened int
	probes int
}

func newFixture() *fixture {
	f := &fixture{
		tr:      &trace{},
		log:     &bytes.Buffer{},
		cl:      &fakeCluster{},
		rel:     &fakeReleases{},
		confirm: &fakeConfirm{answer: true},
		probe:   &fakeProbe{reachCode: 404},
	}
	f.cl.tr = f.tr
	f.rel.tr = f.tr
	f.confirm.tr = f.tr
	f.probe.tr = f.tr
	f.d = &Decision{
		GatewayClass: "eg",
		Open: func() (Cluster, error) {
			f.opened++
			return f.cl, nil
		},
		Releases: f.rel,
		Probe: func() (GatewayProbe, error) {
			f.probes++
			return f.probe, nil
		},
		Confirm: f.confirm,
		Say:     &say.Plain{W: f.log},
	}
	return f
}

func components(actions map[bootstrap.PlatformComponent]string) []bootstrap.ComponentResult {
	order := []bootstrap.PlatformComponent{
		bootstrap.ComponentGatewayAPI,
		bootstrap.ComponentLoadBalancer,
		bootstrap.ComponentGateway,
		bootstrap.ComponentCertManager,
	}
	out := make([]bootstrap.ComponentResult, 0, len(order))
	for _, c := range order {
		out = append(out, bootstrap.ComponentResult{Component: c, Action: actions[c], Mode: bootstrap.ModeAuto})
	}
	return out
}

var allInstall = []bootstrap.ComponentResult{
	{Component: bootstrap.ComponentGatewayAPI, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentLoadBalancer, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentGateway, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentCertManager, Action: "install", Mode: bootstrap.ModeAuto},
}

// notProbed is what bootstrap's read-only probes produce on a cluster with
// nothing on it: Envoy Gateway and its Gateway are planned installs, and the
// LoadBalancer is the question the plan defers to the Gateway's own status.
func notProbed(mode bootstrap.PlatformMode) []bootstrap.ComponentResult {
	action := "probe"
	if mode == bootstrap.ModeExternal {
		action = "verify"
	}
	return []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "install", Outcome: bootstrap.OutcomeAbsent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentLoadBalancer, Action: action, Outcome: bootstrap.OutcomeNotProbed, Mode: mode},
		{Component: bootstrap.ComponentGateway, Action: "install", Outcome: bootstrap.OutcomeAbsent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentCertManager, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
	}
}

// Only an install action becomes a release. A component box uses is never
// installed or rewritten -- but box's own Gateway is on every path, because
// the chart's routes name it -- and the tenancy policy always closes the plan.
func TestBuildPlanInstallsOnlyInstallActions(t *testing.T) {
	plan, err := BuildPlan(components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGatewayAPI:   "use",
		bootstrap.ComponentLoadBalancer: "use",
		bootstrap.ComponentGateway:      "use",
		bootstrap.ComponentCertManager:  "verify",
	}), "nginx", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 4 || plan.Steps[0].Kind != StepManifests || plan.Steps[3].Kind != StepManifests {
		t.Fatalf("using every capability must leave the Gateway, the issuers, its HTTP check, and the policy, got %+v", plan.Steps)
	}
	if plan.Steps[1].Kind != StepIssuers || plan.Steps[2].Kind != StepReachable {
		t.Fatalf("the issuers and the Gateway's HTTP check are not among the steps: %+v", plan.Steps)
	}
	if !strings.Contains(plan.Steps[0].What, "GatewayClass nginx") {
		t.Fatalf("the Gateway was not planned on the chosen class: %q", plan.Steps[0].What)
	}
	// A class someone else's controller serves is never rewritten: no
	// GatewayClass document when box does not install the implementation.
	for _, doc := range plan.Steps[0].Documents {
		if kind, _, _ := objectMeta(doc); kind == "GatewayClass" {
			t.Fatal("box planned to write a GatewayClass it does not own")
		}
	}
	if len(plan.NotHandled) != 0 {
		t.Fatalf("nothing installs, so nothing is unhandled: %v", plan.NotHandled)
	}

	plan, err = BuildPlan(allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range plan.Steps {
		switch s.Kind {
		case StepRelease:
			got = append(got, "release/"+s.Release.Name)
		case StepPool:
			got = append(got, "pool")
		case StepManifests:
			if strings.Contains(s.What, "GatewayClass") {
				got = append(got, "gateway")
			} else {
				got = append(got, "policy")
			}
		case StepReachable:
			got = append(got, "reachable")
		}
	}
	want := []string{"release/envoy-gateway", "release/cert-manager", "release/metallb", "pool", "gateway", "reachable", "policy"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("plan order = %v, want %v", got, want)
	}
	if len(plan.NotHandled) != 0 {
		t.Fatalf("every install is handled, so nothing is unhandled: %v", plan.NotHandled)
	}
	var certManager *Release
	for _, s := range plan.Steps {
		if s.Kind == StepRelease && s.Release.Name == CertManagerRelease {
			r := s.Release
			certManager = &r
		}
	}
	if certManager == nil || certManager.Version != platformassets.Pins.CertManager {
		t.Fatalf("cert-manager was not planned at its pin: %+v", certManager)
	}
	crds, _ := certManager.Values["crds"].(map[string]any)
	config, _ := certManager.Values["config"].(map[string]any)
	gwapi, _ := config["gatewayAPI"].(map[string]any)
	if crds["enabled"] != true || gwapi["enabled"] != true {
		t.Fatalf("cert-manager values = %v, want its CRDs and Gateway API support on", certManager.Values)
	}
}

// The Gateway is applied first in auto mode, then the question, then (only if
// the answer is "no address") MetalLB and its pool, then the reachability
// check. That order is the whole design: the Gateway's own status decides.
func TestBuildPlanMakesMetalLBConditionalWhenUnprobed(t *testing.T) {
	plan, err := BuildPlan(notProbed(bootstrap.ModeAuto), "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []StepKind
	var conditional []string
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind)
		if s.Unless != "" {
			if s.Unless != UnlessGatewayAddress {
				t.Fatalf("%s step carries an unknown condition %q", s.Kind, s.Unless)
			}
			conditional = append(conditional, string(s.Kind)+"/"+s.Release.Name)
		}
	}
	want := []StepKind{StepRelease, StepManifests, StepIssuers, StepGatewayAddress, StepRelease, StepPool, StepReachable, StepManifests}
	if len(kinds) != len(want) {
		t.Fatalf("plan = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("plan = %v, want %v", kinds, want)
		}
	}
	if plan.Steps[0].Release.Name != EnvoyGatewayRelease || plan.Steps[4].Release.Name != MetalLBRelease {
		t.Fatalf("plan releases = %s, %s", plan.Steps[0].Release.Name, plan.Steps[4].Release.Name)
	}
	if got := strings.Join(conditional, ","); got != "release/metallb,metallb-pool/" {
		t.Fatalf("conditional steps = %q, want MetalLB and its pool only", got)
	}
	if plan.Steps[3].Mode != bootstrap.ModeAuto {
		t.Fatalf("the address question does not run in auto mode: %+v", plan.Steps[3])
	}
	// The policy is never conditional: tenancy is not a LoadBalancer question.
	if plan.Steps[len(plan.Steps)-1].Unless != "" {
		t.Fatal("the admission policy was made conditional")
	}
}

// A re-run writes the Gateway again (it is box's object on every path) but it
// still asks the Gateway that is already there: that is what keeps a second
// bootstrap on a cluster whose MetalLB someone else installed from installing
// one of box's own.
func TestBuildPlanAsksAboutTheGatewayItDidNotWrite(t *testing.T) {
	comps := []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentLoadBalancer, Action: "probe", Outcome: bootstrap.OutcomeNotProbed, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentGateway, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentCertManager, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
	}
	plan, err := BuildPlan(comps, "nginx", "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []StepKind
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind)
	}
	want := []StepKind{StepManifests, StepIssuers, StepGatewayAddress, StepRelease, StepPool, StepReachable, StepManifests}
	if len(kinds) != len(want) {
		t.Fatalf("plan = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("plan = %v, want %v", kinds, want)
		}
	}
	if !strings.Contains(plan.Steps[0].What, "GatewayClass nginx") {
		t.Fatalf("the Gateway was not written on the used class: %q", plan.Steps[0].What)
	}
	if plan.Steps[3].Unless != UnlessGatewayAddress || plan.Steps[4].Unless != UnlessGatewayAddress {
		t.Fatal("MetalLB is not conditional on a Gateway box did not write")
	}
}

// managed is box's own address layer: installed before the Gateway (whose
// Service is what needs it) without asking anything.
func TestBuildPlanManagedInstallsMetalLBWithoutAQuestion(t *testing.T) {
	comps := notProbed(bootstrap.ModeManaged)
	comps[1].Action = "install"
	plan, err := BuildPlan(comps, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []StepKind
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind)
		if s.Unless != "" {
			t.Fatalf("managed mode planned a conditional step: %+v", s)
		}
	}
	want := []StepKind{StepRelease, StepRelease, StepPool, StepManifests, StepIssuers, StepReachable, StepManifests}
	if len(kinds) != len(want) {
		t.Fatalf("managed plan = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("managed plan = %v, want %v", kinds, want)
		}
	}
	if plan.Steps[1].Release.Name != MetalLBRelease {
		t.Fatalf("managed mode installs %s before the Gateway, want MetalLB", plan.Steps[1].Release.Name)
	}
}

// external mode writes nothing for the address layer: the Gateway's address
// verifies the operator's own, and the plan says so with a question.
func TestBuildPlanExternalVerifiesWithTheGatewayAddress(t *testing.T) {
	plan, err := BuildPlan(notProbed(bootstrap.ModeExternal), "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Steps {
		if s.Component == bootstrap.ComponentLoadBalancer && s.Kind != StepGatewayAddress && s.Kind != StepReachable {
			t.Fatalf("external mode planned a write for the address layer: %+v", s)
		}
	}
	question := false
	for _, s := range plan.Steps {
		if s.Kind == StepGatewayAddress {
			question = true
			if s.Mode != bootstrap.ModeExternal {
				t.Fatalf("the question runs in %s mode, want external", s.Mode)
			}
		}
	}
	if !question {
		t.Fatal("external mode planned no verification of the Gateway's address")
	}
}

// A conflict stops box in every mode, before any client exists.
func TestBuildPlanRefusesConflict(t *testing.T) {
	comps := components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGateway: "stop",
	})
	plan, err := BuildPlan(comps, "eg", "")
	if err == nil || !strings.Contains(err.Error(), "platform conflict") {
		t.Fatalf("want a conflict refusal, got %v (%+v)", err, plan)
	}
	f := newFixture()
	if _, err := f.d.Apply(context.Background(), comps); err == nil {
		t.Fatal("Apply accepted a conflict")
	}
	if f.opened != 0 {
		t.Fatal("Apply opened a cluster client for a conflict")
	}
}

// A no writes nothing at all: not a Helm release, not a manifest, not the pool
// -- and the Gateway is never asked about its address.
func TestApplyRefusalWritesNothing(t *testing.T) {
	f := newFixture()
	f.confirm.answer = false

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if !strings.Contains(f.log.String(), "oh heck, nvm") {
		t.Fatalf("the refusal has no voice: %q", f.log.String())
	}
	if len(f.rel.installed) != 0 || len(f.cl.writes) != 0 || len(f.cl.awaits) != 0 {
		t.Fatalf("a refusal wrote: %d releases, %d applies, %d waits", len(f.rel.installed), len(f.cl.writes), len(f.cl.awaits))
	}
	if f.probe.addressCalls != 0 || f.probes != 0 {
		t.Fatalf("a refusal asked about the Gateway: %d answers, %d probes built", f.probe.addressCalls, f.probes)
	}
	if got := f.tr.String(); got != "confirm" {
		t.Fatalf("a refusal did more than ask: %s", got)
	}
	if f.confirm.asked != 1 {
		t.Fatalf("asked %d times, want once", f.confirm.asked)
	}
	// The plan was still printed before the question: that is the review.
	if !strings.Contains(f.log.String(), "envoy-gateway 1.9.3") {
		t.Fatalf("the plan did not name the pinned Envoy Gateway: %q", f.log.String())
	}
	if !strings.Contains(f.log.String(), "? metallb 0.16.1 (Helm, metallb-system), unless the Gateway gets an address without it") {
		t.Fatalf("the plan did not say MetalLB is conditional: %q", f.log.String())
	}
	if !strings.Contains(f.log.String(), "? Gateway s5n-gateway/s5n getting an address") {
		t.Fatalf("the plan did not say it waits for the Gateway's address: %q", f.log.String())
	}
}

// Missing RBAC is refused before the confirm and before any write.
func TestApplyRefusesWithoutAccess(t *testing.T) {
	f := newFixture()
	f.cl.deny = "customresourcedefinitions"

	_, err := f.d.Apply(context.Background(), allInstall)
	if err == nil || !strings.Contains(err.Error(), "custom resource definitions") {
		t.Fatalf("want an RBAC refusal naming the CRDs, got %v", err)
	}
	if f.confirm.asked != 0 {
		t.Fatal("box asked before it knew it could do the work")
	}
	if len(f.rel.installed) != 0 || len(f.cl.writes) != 0 {
		t.Fatalf("an RBAC refusal wrote: %d releases, %d applies", len(f.rel.installed), len(f.cl.writes))
	}
	if f.probe.addressCalls != 0 {
		t.Fatal("an RBAC refusal asked about the Gateway")
	}
}

// The whole install, in order, with readiness after each component. This is
// the managed shape: MetalLB before the Gateway.
func TestApplyInstallsInOrder(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	res, err := f.d.Apply(context.Background(), allInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.rel.installed) != 3 ||
		f.rel.installed[0].Name != EnvoyGatewayRelease || f.rel.installed[0].Version != platformassets.Pins.EnvoyGateway ||
		f.rel.installed[1].Name != CertManagerRelease || f.rel.installed[1].Version != platformassets.Pins.CertManager ||
		f.rel.installed[2].Name != MetalLBRelease || f.rel.installed[2].Version != platformassets.Pins.MetalLB {
		t.Fatalf("releases = %+v", f.rel.installed)
	}
	if len(f.cl.writes) != 4 {
		t.Fatalf("applies = %d, want pool, gateway, issuers, policy", len(f.cl.writes))
	}
	if kind, ns, name := objectMeta(f.cl.writes[0][0]); kind != "IPAddressPool" || ns != MetalLBNamespace || name != MetalLBPoolName {
		t.Fatalf("first apply is %s %s/%s, want the MetalLB pool", kind, ns, name)
	}
	if pool := f.cl.writes[0][0]; !strings.Contains(pool["spec"].(map[string]any)["addresses"].([]any)[0].(string), "192.0.2.10/32") {
		t.Fatalf("pool addresses = %v", pool["spec"])
	}
	if kind, ns, name := objectMeta(f.cl.writes[1][0]); kind != "Namespace" || name != GatewayNamespace {
		t.Fatalf("second apply is %s %s/%s, want the Gateway's namespace", kind, ns, name)
	}
	if kind, _, name := objectMeta(f.cl.writes[1][1]); kind != "GatewayClass" || name != "eg" {
		t.Fatalf("second apply's second doc is %s %s, want the GatewayClass", kind, name)
	}
	if kind, ns, name := objectMeta(f.cl.writes[1][2]); kind != "Gateway" || ns != GatewayNamespace || name != GatewayName {
		t.Fatalf("second apply's third doc is %s %s/%s", kind, ns, name)
	}
	if kind, _, name := objectMeta(f.cl.writes[2][0]); kind != "ClusterIssuer" || name != LetsEncryptIssuer {
		t.Fatalf("third apply starts with %s %s, want the cert-manager issuers", kind, name)
	}
	if kind, _, _ := objectMeta(f.cl.writes[3][0]); kind != "Namespace" {
		t.Fatalf("fourth apply starts with %s, want the policy's namespace", kind)
	}

	var waits []string
	for _, w := range f.cl.awaits {
		waits = append(waits, w.String())
	}
	want := []string{
		"Deployment envoy-gateway-system/envoy-gateway",
		"Deployment cert-manager/cert-manager",
		"Deployment cert-manager/cert-manager-webhook",
		"Deployment cert-manager/cert-manager-cainjector",
		"Deployment metallb-system/metallb-controller",
		"DaemonSet metallb-system/metallb-speaker",
	}
	if strings.Join(waits, ",") != strings.Join(want, ",") {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	// No question: managed mode owns the address layer. The reachability check
	// still runs, because an address nobody can reach is not an install.
	if f.probe.addressCalls != 1 || f.probe.reachCalls != 1 {
		t.Fatalf("address calls = %d, reach calls = %d, want the reachability check only", f.probe.addressCalls, f.probe.reachCalls)
	}
	if len(res.Installed) == 0 || !strings.Contains(f.log.String(), "platform ready") {
		t.Fatalf("result/log do not report the install: %+v %q", res, f.log.String())
	}
}

// The point of the design: when the Gateway gets an address on its own, MetalLB
// and its pool are neither installed nor applied.
func TestApplySkipsMetalLBWhenTheGatewayGetsAnAddress(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "10.0.0.50"}}

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatal("MetalLB was installed although the Gateway got an address")
		}
	}
	for _, docs := range f.cl.writes {
		if kind, _, _ := objectMeta(docs[0]); kind == "IPAddressPool" {
			t.Fatal("the MetalLB pool was applied although the Gateway got an address")
		}
	}
	if f.probe.addressCalls != 2 {
		t.Fatalf("address calls = %d, want the question and the reachability check", f.probe.addressCalls)
	}
	if f.probe.reached != "10.0.0.50" {
		t.Fatalf("the reachability check used %q, want the Gateway's address", f.probe.reached)
	}
	if f.probe.addressWaits[0] != AddressTimeout {
		t.Fatalf("the question waited %s, want %s", f.probe.addressWaits[0], AddressTimeout)
	}
	if AddressTimeout != 15*time.Second {
		t.Fatalf("the address question is %s, want the 15s docs/box.md specifies", AddressTimeout)
	}
	if ProbeTimeout != 60*time.Second {
		t.Fatalf("the Programmed wait is %s, want the 60s docs/box.md specifies", ProbeTimeout)
	}
	if ReachTimeout != 30*time.Second {
		t.Fatalf("the reachability retry is %s, want 30s", ReachTimeout)
	}
	// Ordering: the Gateway is applied, then asked, then the check.
	got := f.tr.String()
	want := []string{
		"confirm",
		"install:" + EnvoyGatewayRelease,
		"await:Deployment envoy-gateway-system/envoy-gateway",
		"apply:Namespace",
		"apply:ClusterIssuer",
		"address",
		"address",
		"reach",
		"apply:Namespace",
	}
	if strings.Join(want, ",") != got {
		t.Fatalf("trace = %s, want %s", got, strings.Join(want, ","))
	}
	if !strings.Contains(f.log.String(), "the Gateway already has an address: 10.0.0.50") {
		t.Fatalf("box did not say what answered: %q", f.log.String())
	}
	if !strings.Contains(f.log.String(), "the Gateway answers on 10.0.0.50:80 (HTTP 404)") {
		t.Fatalf("box did not print the reachability result: %q", f.log.String())
	}
}

// AddressNotAssigned: MetalLB is installed and the pool applied, then the
// Gateway is waited on again -- and only then is the address checked.
func TestApplyInstallsMetalLBOnAddressNotAssigned(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{
		{Reason: gatewayAddressNotAssigned},
		{Address: "192.0.2.10"},
	}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto)); err != nil {
		t.Fatal(err)
	}
	if len(f.rel.installed) != 2 || f.rel.installed[1].Name != MetalLBRelease {
		t.Fatalf("releases = %+v, want Envoy Gateway then MetalLB", f.rel.installed)
	}
	if len(f.cl.writes) != 4 {
		t.Fatalf("applies = %d, want pool, gateway, issuers, policy", len(f.cl.writes))
	}
	// The Gateway and its issuers are applied before the question, so the
	// pool is the third write, not the first.
	if kind, _, name := objectMeta(f.cl.writes[2][0]); kind != "IPAddressPool" || name != MetalLBPoolName {
		t.Fatalf("the pool was not applied after the Gateway: %s %s", kind, name)
	}
	got := f.tr.String()
	want := []string{
		"confirm",
		"install:" + EnvoyGatewayRelease,
		"await:Deployment envoy-gateway-system/envoy-gateway",
		"apply:Namespace",
		"apply:ClusterIssuer",
		"address",
		"install:" + MetalLBRelease,
		"await:Deployment metallb-system/metallb-controller",
		"await:DaemonSet metallb-system/metallb-speaker",
		"apply:IPAddressPool",
		"address",
		"reach",
		"apply:Namespace",
	}
	if strings.Join(want, ",") != got {
		t.Fatalf("trace = %s, want %s", got, strings.Join(want, ","))
	}
	if !strings.Contains(f.log.String(), "the Gateway has no address ("+gatewayAddressNotAssigned+") after waiting up to 15s; installing MetalLB") {
		t.Fatalf("box did not say why it installed MetalLB: %q", f.log.String())
	}
	if f.probe.reached != "192.0.2.10" {
		t.Fatalf("the reachability check used %q", f.probe.reached)
	}
}

// An address and a Programmed reason that is not AddressNotAssigned is a
// failure to stop on: Envoy Gateway reports NoResources while its data plane
// is still coming up, and box must not install MetalLB over a Gateway that has
// an address it cannot call ready by the deadline.
func TestApplyStopsOnAnAddressThatIsNotProgrammed(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{
		Address: "172.18.255.239",
		Status:  "False",
		Reason:  "NoResources",
		Message: "Envoy replicas unavailable",
	}}

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err == nil {
		t.Fatal("box accepted a Gateway with an address it will not call Programmed")
	}
	for _, want := range []string{"has address 172.18.255.239 but is not Programmed", "NoResources", "Envoy replicas unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatal("MetalLB was installed over an address that already existed")
		}
	}
	if f.probe.reachCalls != 0 {
		t.Fatal("the reachability check ran on a Gateway that had already failed")
	}
}

// The short deadline only decides "no address": a Gateway that already has an
// address from an existing LoadBalancer, with Envoy's proxy still rolling out
// (NoResources), gets the rest of the whole wait and is then used as it is.
func TestApplyWaitsOutASettlingGatewayPastTheShortDeadline(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{
		{Address: "192.0.2.10", Status: "False", Reason: "NoResources"},
		{Address: "192.0.2.10"},
	}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto)); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatal("MetalLB was installed over a Gateway that was still settling")
		}
	}
	if len(f.probe.addressWaits) < 2 || f.probe.addressWaits[0] != AddressTimeout || f.probe.addressWaits[1] != ProbeTimeout-AddressTimeout {
		t.Fatalf("address waits = %v, want %s then the rest of %s", f.probe.addressWaits, AddressTimeout, ProbeTimeout)
	}
	if !strings.Contains(f.log.String(), "the Gateway is still settling (NoResources); waiting up to another 45s") {
		t.Fatalf("box did not say why it kept waiting: %q", f.log.String())
	}
}

// In external mode the operator's LoadBalancer gets the whole wait at once:
// there is no MetalLB to fall back to, so a short deadline would only refuse a
// slow address layer.
func TestApplyExternalGetsTheWholeWait(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "203.0.113.9"}}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeExternal)); err != nil {
		t.Fatal(err)
	}
	if f.probe.addressWaits[0] != ProbeTimeout {
		t.Fatalf("external mode waited %s, want %s", f.probe.addressWaits[0], ProbeTimeout)
	}
}

// A Gateway with no address that is failing for a reason MetalLB cannot answer
// stops too: installing an address layer would hide it.
func TestApplyStopsOnAnotherReasonWithNoAddress(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Status: "False", Reason: "AddressNotUsable", Message: "One or more Gateway addresses cannot be used"}}

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err == nil || !strings.Contains(err.Error(), "AddressNotUsable") {
		t.Fatalf("want the Gateway's own reason, got %v", err)
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatal("MetalLB was installed over a Gateway failing for another reason")
		}
	}
}

// The last check is the one that matters: an address in the API server that
// nothing answers on is not a working platform.
func TestApplyFailsWhenNothingAnswersOnTheAddress(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "10.0.0.50"}}
	f.probe.reachErr = errors.New("dial tcp 10.0.0.50:80: connect: connection refused")

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err == nil {
		t.Fatal("box accepted an address nothing answers on")
	}
	for _, want := range []string{
		"the Gateway has address 10.0.0.50, but nothing answers on 10.0.0.50:80 from this host",
		"connection refused",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(f.log.String(), "answers on 10.0.0.50:80 (HTTP") {
		t.Fatalf("box reported a pass it did not get: %q", f.log.String())
	}
}

// external mode: no MetalLB ever. A Gateway with no address stops bootstrap
// with a reason of box's own.
func TestApplyExternalWithoutAnAddressStops(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Reason: gatewayAddressNotAssigned}}

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeExternal))
	if err == nil {
		t.Fatal("external mode installed a platform with no address behind it")
	}
	for _, want := range []string{"external", "got no address within 1m0s", "platform.loadbalancer.mode=auto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	// The Gateway itself has to be applied before anything can be asked about
	// it; nothing the address layer would write is.
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatal("external mode installed MetalLB")
		}
	}
	for _, docs := range f.cl.writes {
		if kind, _, _ := objectMeta(docs[0]); kind == "IPAddressPool" || kind == "L2Advertisement" {
			t.Fatal("external mode applied MetalLB's pool")
		}
	}
	if f.probe.reachCalls != 0 {
		t.Fatal("the reachability check ran with no address")
	}
}

// external mode with an address: the rest proceeds, and box says the address
// layer is the operator's.
func TestApplyExternalWithAnAddressInstallsTheRest(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "203.0.113.9"}}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeExternal)); err != nil {
		t.Fatal(err)
	}
	if len(f.rel.installed) != 1 || f.rel.installed[0].Name != EnvoyGatewayRelease {
		t.Fatalf("releases = %+v, want only Envoy Gateway", f.rel.installed)
	}
	if !strings.Contains(f.log.String(), "the Gateway has an address from the external LoadBalancer: 203.0.113.9") {
		t.Fatalf("box did not report the external address: %q", f.log.String())
	}
	if f.probe.reached != "203.0.113.9" {
		t.Fatalf("the reachability check used %q", f.probe.reached)
	}
}

// box's own MetalLB is not a question: a second bootstrap verifies it as
// before and never asks whether the Gateway could have done without it.
func TestApplyVerifiesBoxMetalLBWithoutAsking(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{"metallb-system/metallb": platformassets.Pins.MetalLB}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto)); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatalf("Helm was called for MetalLB already at its pinned version: %+v", f.rel.installed)
		}
	}
	if !strings.Contains(f.log.String(), "= metallb: already at "+platformassets.Pins.MetalLB) {
		t.Fatalf("the plan did not verify MetalLB: %q", f.log.String())
	}
	if strings.Contains(f.log.String(), "? metallb") {
		t.Fatalf("a release already deployed was printed as a question: %q", f.log.String())
	}
	if strings.Contains(f.log.String(), "getting an address") {
		t.Fatalf("box asked whether the Gateway needed MetalLB it already owns: %q", f.log.String())
	}
	// One read, from the reachability check; no question.
	if f.probe.addressCalls != 1 || f.probe.reachCalls != 1 {
		t.Fatalf("address calls = %d, reach calls = %d", f.probe.addressCalls, f.probe.reachCalls)
	}
	if !strings.Contains(f.log.String(), "applied IPAddressPool "+MetalLBPoolName) {
		t.Fatalf("the pool was not applied: %q", f.log.String())
	}
}

// A release already at the pinned version is verified, not sent to Helm: a
// second bootstrap must do no writes, and piling up Helm revisions is a write.
func TestApplyVerifiesPinnedReleases(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{
		"envoy-gateway-system/envoy-gateway": platformassets.Pins.EnvoyGateway,
		"cert-manager/cert-manager":          platformassets.Pins.CertManager,
		"metallb-system/metallb":             platformassets.Pins.MetalLB,
	}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	if _, err := f.d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(f.rel.installed) != 0 {
		t.Fatalf("Helm was called for releases already at their pinned version: %+v", f.rel.installed)
	}
	if !strings.Contains(f.log.String(), "already at "+platformassets.Pins.EnvoyGateway) {
		t.Fatalf("the plan did not say the release is already there: %q", f.log.String())
	}
	// Readiness is still checked: a no-op run is verification, not silence.
	if len(f.cl.awaits) != 6 {
		t.Fatalf("waits = %v, want every readiness check", f.cl.awaits)
	}
}

// A release at another version is an upgrade — including MetalLB's, which is
// never skipped in favour of an address box's own older copy provides.
func TestApplyUpgradesAnotherVersion(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{
		"envoy-gateway-system/envoy-gateway": "1.9.2",
		"metallb-system/metallb":             "0.15.0",
	}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto)); err != nil {
		t.Fatal(err)
	}
	var upgraded []string
	for _, r := range f.rel.installed {
		upgraded = append(upgraded, r.Name)
	}
	if strings.Join(upgraded, ",") != EnvoyGatewayRelease+","+MetalLBRelease {
		t.Fatalf("upgrades = %v, want Envoy Gateway and MetalLB", upgraded)
	}
	if !strings.Contains(f.log.String(), "~ metallb 0.16.1") {
		t.Fatalf("the plan did not mark the MetalLB upgrade: %q", f.log.String())
	}
}

// MetalLB's webhook answers a moment after its pods are Ready. The pool apply
// is retried until it accepts, and the error is box's own.
func TestApplyRetriesThePool(t *testing.T) {
	f := newFixture()
	f.cl.applyErrs = []error{errors.New("no endpoints available for service \"metallb-webhook-service\""), nil}
	f.rel.versions = map[string]string{"metallb-system/metallb": platformassets.Pins.MetalLB}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}
	f.d.PoolTimeout = time.Second
	f.d.Poll = time.Millisecond

	if _, err := f.d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(f.cl.writes) != 4 {
		t.Fatalf("applies = %d, want the pool retried, then the gateway, issuers and policy", len(f.cl.writes))
	}
	if !strings.Contains(f.log.String(), "applied IPAddressPool s5n-pool (192.0.2.10/32)") {
		t.Fatalf("the pool was not reported: %q", f.log.String())
	}
}

func TestApplyInstallsCertManager(t *testing.T) {
	f := newFixture()
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	res, err := f.d.Apply(context.Background(), allInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotHandled) != 0 {
		t.Fatalf("cert-manager was left unhandled: %+v", res.NotHandled)
	}
	if !strings.Contains(f.log.String(), "installing cert-manager v1.21.3 (Helm, cert-manager)") {
		t.Fatalf("cert-manager was not printed: %q", f.log.String())
	}
}

// The embedded manifests are what the e2e applies with kubectl.
func TestEmbeddedManifests(t *testing.T) {
	docs, err := ParseManifests(gatewayManifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("gateway manifest has %d documents, want namespace + GatewayClass + Gateway", len(docs))
	}
	if kind, _, name := objectMeta(docs[0]); kind != "Namespace" || name != GatewayNamespace {
		t.Fatalf("first document = %s %s", kind, name)
	}
	if kind, _, name := objectMeta(docs[1]); kind != "GatewayClass" || name != "eg" {
		t.Fatalf("second document = %s %s", kind, name)
	}
	if kind, ns, name := objectMeta(docs[2]); kind != "Gateway" || ns != GatewayNamespace || name != GatewayName {
		t.Fatalf("third document = %s %s/%s", kind, ns, name)
	}

	docs, err = ParseManifests(admissionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 3 {
		t.Fatalf("policy manifest has %d documents", len(docs))
	}
	if kind, _, name := objectMeta(docs[0]); kind != "Namespace" || name != "s5n-system" {
		t.Fatalf("policy manifest starts with %s %s", kind, name)
	}
	policies := 0
	for _, d := range docs {
		if kind, _, _ := objectMeta(d); kind == "ValidatingAdmissionPolicy" {
			policies++
		}
	}
	// Three: the image rule, the granted Service, and the login -- the last
	// one is what keeps a namespace from removing or overrode the
	// SecurityPolicy root put on the Gateway.
	if policies != 3 {
		t.Fatalf("policy manifest has %d policies, want 3", policies)
	}
}

// A same-named release box did not install is the operator's. When the
// Gateway's own address means MetalLB is not needed, box leaves it alone and
// does not even call Helm about it.
func TestApplyLeavesAForeignMetalLBAlone(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{"metallb-system/metallb": "0.15.0"}
	f.rel.unowned = map[string]bool{"metallb-system/metallb": true}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	res, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range res.Verified {
		if v == MetalLBRelease {
			t.Fatalf("box verified a release it does not own: %v", res.Verified)
		}
	}
	if strings.Contains(f.log.String(), "already at 0.15.0") {
		t.Fatalf("box treated an operator's release as its own: %q", f.log.String())
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatalf("box called Helm for a release it does not own: %+v", f.rel.installed)
		}
	}
}

// If MetalLB is actually needed and the name is taken by a release box did not
// install, box stops rather than upgrading it into box's version.
func TestApplyRefusesToUpgradeAForeignMetalLB(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{"metallb-system/metallb": "0.15.0"}
	f.rel.unowned = map[string]bool{"metallb-system/metallb": true}
	f.probe.answers = []GatewayAnswer{
		{Reason: gatewayAddressNotAssigned},
		{Reason: gatewayAddressNotAssigned},
	}

	_, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto))
	if err == nil {
		t.Fatal("box accepted a platform that needs MetalLB but cannot install it")
	}
	for _, want := range []string{"metallb", "did not install it", "will not upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease {
			t.Fatalf("box upgraded an operator's release: %+v", f.rel.installed)
		}
	}
}

// An unconditional release step -- Envoy Gateway when box has to install the
// implementation -- is refused before the confirm and before any write.
func TestApplyRefusesAForeignEnvoyBeforeTheConfirm(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{"envoy-gateway-system/envoy-gateway": "1.8.0"}
	f.rel.unowned = map[string]bool{"envoy-gateway-system/envoy-gateway": true}

	_, err := f.d.Apply(context.Background(), allInstall)
	if err == nil || !strings.Contains(err.Error(), "did not install it") {
		t.Fatalf("want a refusal naming the operator's release, got %v", err)
	}
	if f.confirm.asked != 0 {
		t.Fatal("box asked to install before refusing another operator's release")
	}
	if len(f.rel.installed) != 0 || len(f.cl.writes) != 0 {
		t.Fatalf("a refusal wrote: %d releases, %d applies", len(f.rel.installed), len(f.cl.writes))
	}
}

// The embedded ClusterIssuers are root's contract: a production ACME issuer
// that solves HTTP-01 through the s5n Gateway's port-80 listener, and a
// self-signed issuer for everything that cannot be validated. There is no
// ACME email: Let's Encrypt stopped sending expiry mail.
func TestEmbeddedIssuers(t *testing.T) {
	docs, err := issuerObjects("")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("issuer manifest has %d documents, want two ClusterIssuers", len(docs))
	}
	byName := map[string]map[string]any{}
	for _, doc := range docs {
		kind, _, name := objectMeta(doc)
		if kind != "ClusterIssuer" {
			t.Fatalf("%s %s is not a ClusterIssuer", kind, name)
		}
		byName[name] = doc
	}
	le := byName[LetsEncryptIssuer]
	if le == nil {
		t.Fatalf("no %s", LetsEncryptIssuer)
	}
	acme, _ := le["spec"].(map[string]any)["acme"].(map[string]any)
	if acme["server"] != "https://acme-v02.api.letsencrypt.org/directory" {
		t.Fatalf("ACME server = %v, want Let's Encrypt production", acme["server"])
	}
	if _, ok := acme["email"]; ok {
		t.Fatal("the issuer sets an email address")
	}
	solvers, _ := acme["solvers"].([]any)
	if len(solvers) != 1 {
		t.Fatalf("solvers = %v", acme["solvers"])
	}
	solver, _ := solvers[0].(map[string]any)["http01"].(map[string]any)
	gw, _ := solver["gatewayHTTPRoute"].(map[string]any)
	parentRefs, _ := gw["parentRefs"].([]any)
	if len(parentRefs) != 1 {
		t.Fatalf("parentRefs = %v", gw["parentRefs"])
	}
	ref, _ := parentRefs[0].(map[string]any)
	// No sectionName: the solver route must attach to the generic http listener
	// and to every per-host http-<host> listener that admits its namespace. A
	// pinned sectionName would stop matching once a host has its own listener.
	if ref["name"] != GatewayName || ref["namespace"] != GatewayNamespace {
		t.Fatalf("the solver does not attach to the s5n Gateway: %v", ref)
	}
	if _, ok := ref["sectionName"]; ok {
		t.Fatalf("the solver pins a listener section: %v", ref)
	}
	if byName[SelfSignedIssuer] == nil {
		t.Fatalf("no %s", SelfSignedIssuer)
	}
	if _, ok := byName[SelfSignedIssuer]["spec"].(map[string]any)["selfSigned"]; !ok {
		t.Fatalf("%s is not self-signed: %v", SelfSignedIssuer, byName[SelfSignedIssuer]["spec"])
	}
}

// The only ACME server override in the tree: a development run points the
// production issuer at its test server, and that server's own certificate is
// self-signed, so the test issuer skips verifying it. A real run keeps the
// manifest byte for byte.
func TestIssuerOverrideIsForTestServersOnly(t *testing.T) {
	const testServer = "https://pebble.pebble.svc:14000/dir"
	docs, err := issuerObjects(testServer)
	if err != nil {
		t.Fatal(err)
	}
	acme, _ := docs[0]["spec"].(map[string]any)["acme"].(map[string]any)
	if acme["server"] != testServer || acme["skipTLSVerify"] != true {
		t.Fatalf("overridden acme = %v", acme)
	}
	plain, err := issuerObjects("")
	if err != nil {
		t.Fatal(err)
	}
	prod, _ := plain[0]["spec"].(map[string]any)["acme"].(map[string]any)
	if _, ok := prod["skipTLSVerify"]; ok {
		t.Fatal("a production issuer skips ACME endpoint verification")
	}
}

// cert-manager's webhook can refuse the first apply while its certificate is
// still being injected, exactly like MetalLB's; the issuer apply is retried,
// bounded, and the error is box's own.
func TestApplyRetriesTheIssuers(t *testing.T) {
	f := newFixture()
	f.cl.applyErrs = []error{nil, nil, errors.New("failed calling webhook: no endpoints available"), nil}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}
	f.d.WebhookTimeout = time.Second
	f.d.Poll = time.Millisecond

	res, err := f.d.Apply(context.Background(), allInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.cl.writes) != 4 {
		t.Fatalf("applies = %d, want the issuers retried, then the policy", len(f.cl.writes))
	}
	if !strings.Contains(f.log.String(), "applied ClusterIssuers "+LetsEncryptIssuer) {
		t.Fatalf("the issuers were not reported: %q", f.log.String())
	}
	found := false
	for _, name := range res.Installed {
		if strings.Contains(name, LetsEncryptIssuer) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the issuers are not in the result: %+v", res.Installed)
	}
}
