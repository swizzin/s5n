package platform

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
)

type fakeReleases struct {
	versions  map[string]string
	installed []Release
	err       error
}

func (f *fakeReleases) key(name, namespace string) string { return namespace + "/" + name }

func (f *fakeReleases) Version(_ context.Context, name, namespace string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.versions[f.key(name, namespace)]
	return v, ok, nil
}

func (f *fakeReleases) Install(_ context.Context, spec Release) error {
	if f.err != nil {
		return f.err
	}
	f.installed = append(f.installed, spec)
	return nil
}

type fakeCluster struct {
	deny      string
	ip        string
	writes    [][]map[string]any
	awaits    []Wait
	allowed   []Access
	applyErrs []error
	openable  bool
}

func (f *fakeCluster) Allowed(_ context.Context, a Access) (bool, error) {
	f.allowed = append(f.allowed, a)
	if a.Resource == f.deny {
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
	return nil
}

func (f *fakeCluster) Await(_ context.Context, w Wait) error {
	f.awaits = append(f.awaits, w)
	return nil
}

type fakeConfirm struct {
	answer bool
	asked  int
}

func (f *fakeConfirm) Ask(string) (bool, error) {
	f.asked++
	return f.answer, nil
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

func newDecision(cl *fakeCluster, rel *fakeReleases, confirm *fakeConfirm) (*Decision, *bytes.Buffer, *int) {
	log := &bytes.Buffer{}
	opened := 0
	return &Decision{
		Open: func() (Cluster, error) {
			opened++
			return cl, nil
		},
		Releases: rel,
		Confirm:  confirm,
		Log:      log,
	}, log, &opened
}

var allInstall = []bootstrap.ComponentResult{
	{Component: bootstrap.ComponentGatewayAPI, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentLoadBalancer, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentGateway, Action: "install", Mode: bootstrap.ModeAuto},
	{Component: bootstrap.ComponentCertManager, Action: "install", Mode: bootstrap.ModeAuto},
}

// Only an install action becomes a step. A component box uses or verifies is
// never written, and the order is the PLAN.md order: CRDs, LoadBalancer,
// Gateway, then the tenancy policy.
func TestBuildPlanInstallsOnlyInstallActions(t *testing.T) {
	plan, err := BuildPlan(components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGatewayAPI:   "use",
		bootstrap.ComponentLoadBalancer: "use",
		bootstrap.ComponentGateway:      "use",
		bootstrap.ComponentCertManager:  "verify",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != StepManifests {
		t.Fatalf("using every capability must leave only the policy, got %+v", plan.Steps)
	}
	if len(plan.NotHandled) != 0 {
		t.Fatalf("nothing installs, so nothing is unhandled: %v", plan.NotHandled)
	}

	plan, err = BuildPlan(allInstall)
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
		}
	}
	want := []string{"release/envoy-gateway", "release/metallb", "pool", "gateway", "policy"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("plan order = %v, want %v", got, want)
	}
	if len(plan.NotHandled) != 1 || !strings.Contains(plan.NotHandled[0], "cert-manager") {
		t.Fatalf("cert-manager install must be reported, got %v", plan.NotHandled)
	}
}

// bootstrap's probe cannot decide the LoadBalancer capability without writing
// a throwaway Service, so it reports not-probed; auto mode must still install
// MetalLB, because the Gateway's Service has no address without one.
func TestBuildPlanInstallsAnUnprobedLoadBalancer(t *testing.T) {
	comps := []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentLoadBalancer, Action: "unknown", Outcome: bootstrap.OutcomeNotProbed, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentGateway, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentCertManager, Action: "use", Outcome: bootstrap.OutcomePresent, Mode: bootstrap.ModeAuto},
	}
	plan, err := BuildPlan(comps)
	if err != nil {
		t.Fatal(err)
	}
	var got []StepKind
	for _, s := range plan.Steps {
		got = append(got, s.Kind)
	}
	want := []StepKind{StepRelease, StepPool, StepManifests}
	if len(got) != len(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan = %v, want %v", got, want)
		}
	}
	if plan.Steps[0].Release.Name != MetalLBRelease {
		t.Fatalf("the only release is %s, want MetalLB", plan.Steps[0].Release.Name)
	}

	// external mode is the user's address layer: never installs one.
	comps[1].Mode = bootstrap.ModeExternal
	plan, err = BuildPlan(comps)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Steps {
		if s.Kind == StepRelease {
			t.Fatalf("external mode planned a release: %+v", s.Release)
		}
	}
}

// A conflict stops box in every mode, before any client exists.
func TestBuildPlanRefusesConflict(t *testing.T) {
	comps := components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGateway: "stop",
	})
	plan, err := BuildPlan(comps)
	if err == nil || !strings.Contains(err.Error(), "platform conflict") {
		t.Fatalf("want a conflict refusal, got %v (%+v)", err, plan)
	}
	cl := &fakeCluster{}
	d, _, opened := newDecision(cl, &fakeReleases{}, &fakeConfirm{answer: true})
	if _, err := d.Apply(context.Background(), comps); err == nil {
		t.Fatal("Apply accepted a conflict")
	}
	if *opened != 0 {
		t.Fatal("Apply opened a cluster client for a conflict")
	}
}

// A no writes nothing at all: not a Helm release, not a manifest, not the pool.
func TestApplyRefusalWritesNothing(t *testing.T) {
	cl := &fakeCluster{}
	rel := &fakeReleases{}
	confirm := &fakeConfirm{answer: false}
	d, log, _ := newDecision(cl, rel, confirm)

	_, err := d.Apply(context.Background(), allInstall)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if !strings.Contains(log.String(), "oh heck, nvm") {
		t.Fatalf("the refusal has no voice: %q", log.String())
	}
	if len(rel.installed) != 0 || len(cl.writes) != 0 || len(cl.awaits) != 0 {
		t.Fatalf("a refusal wrote: %d releases, %d applies, %d waits", len(rel.installed), len(cl.writes), len(cl.awaits))
	}
	if confirm.asked != 1 {
		t.Fatalf("asked %d times, want once", confirm.asked)
	}
	// The plan was still printed before the question: that is the review.
	if !strings.Contains(log.String(), "envoy-gateway 1.9.3") {
		t.Fatalf("the plan did not name the pinned Envoy Gateway: %q", log.String())
	}
}

// Missing RBAC is refused before the confirm and before any write.
func TestApplyRefusesWithoutAccess(t *testing.T) {
	cl := &fakeCluster{deny: "customresourcedefinitions"}
	rel := &fakeReleases{}
	confirm := &fakeConfirm{answer: true}
	d, _, _ := newDecision(cl, rel, confirm)

	_, err := d.Apply(context.Background(), allInstall)
	if err == nil || !strings.Contains(err.Error(), "custom resource definitions") {
		t.Fatalf("want an RBAC refusal naming the CRDs, got %v", err)
	}
	if confirm.asked != 0 {
		t.Fatal("box asked before it knew it could do the work")
	}
	if len(rel.installed) != 0 || len(cl.writes) != 0 {
		t.Fatalf("an RBAC refusal wrote: %d releases, %d applies", len(rel.installed), len(cl.writes))
	}
}

// The whole install, in order, with readiness after each component.
func TestApplyInstallsInOrder(t *testing.T) {
	cl := &fakeCluster{}
	rel := &fakeReleases{}
	confirm := &fakeConfirm{answer: true}
	d, log, _ := newDecision(cl, rel, confirm)

	res, err := d.Apply(context.Background(), allInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.installed) != 2 ||
		rel.installed[0].Name != EnvoyGatewayRelease || rel.installed[0].Version != EnvoyGatewayVersion ||
		rel.installed[1].Name != MetalLBRelease || rel.installed[1].Version != MetalLBVersion {
		t.Fatalf("releases = %+v", rel.installed)
	}
	if len(cl.writes) != 3 {
		t.Fatalf("applies = %d, want pool, gateway, policy", len(cl.writes))
	}
	if kind, ns, name := objectMeta(cl.writes[0][0]); kind != "IPAddressPool" || ns != MetalLBNamespace || name != MetalLBPoolName {
		t.Fatalf("first apply is %s %s/%s, want the MetalLB pool", kind, ns, name)
	}
	if pool := cl.writes[0][0]; !strings.Contains(pool["spec"].(map[string]any)["addresses"].([]any)[0].(string), "192.0.2.10/32") {
		t.Fatalf("pool addresses = %v", pool["spec"])
	}
	if kind, ns, name := objectMeta(cl.writes[1][0]); kind != "Namespace" || name != GatewayNamespace {
		t.Fatalf("second apply is %s %s/%s, want the Gateway's namespace", kind, ns, name)
	}
	if kind, _, name := objectMeta(cl.writes[1][1]); kind != "GatewayClass" || name != "eg" {
		t.Fatalf("second apply's second doc is %s %s, want the GatewayClass", kind, name)
	}
	if kind, ns, name := objectMeta(cl.writes[1][2]); kind != "Gateway" || ns != GatewayNamespace || name != GatewayName {
		t.Fatalf("second apply's third doc is %s %s/%s", kind, ns, name)
	}
	if kind, _, _ := objectMeta(cl.writes[2][0]); kind != "Namespace" {
		t.Fatalf("third apply starts with %s, want the policy's namespace", kind)
	}

	var waits []string
	for _, w := range cl.awaits {
		waits = append(waits, w.String())
	}
	want := []string{
		"Deployment envoy-gateway-system/envoy-gateway",
		"Deployment metallb-system/metallb-controller",
		"DaemonSet metallb-system/metallb-speaker",
		"Gateway s5n-gateway/s5n",
	}
	if strings.Join(waits, ",") != strings.Join(want, ",") {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	if len(res.Installed) == 0 || !strings.Contains(log.String(), "platform ready") {
		t.Fatalf("result/log do not report the install: %+v %q", res, log.String())
	}
}

// A release already at the pinned version is verified, not sent to Helm: a
// second bootstrap must do no writes, and piling up Helm revisions is a write.
func TestApplyVerifiesPinnedReleases(t *testing.T) {
	cl := &fakeCluster{}
	rel := &fakeReleases{versions: map[string]string{
		"envoy-gateway-system/envoy-gateway": EnvoyGatewayVersion,
		"metallb-system/metallb":             MetalLBVersion,
	}}
	d, log, _ := newDecision(cl, rel, &fakeConfirm{answer: true})

	if _, err := d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(rel.installed) != 0 {
		t.Fatalf("Helm was called for releases already at their pinned version: %+v", rel.installed)
	}
	if !strings.Contains(log.String(), "already at "+EnvoyGatewayVersion) {
		t.Fatalf("the plan did not say the release is already there: %q", log.String())
	}
	// Readiness is still checked: a no-op run is verification, not silence.
	if len(cl.awaits) != 4 {
		t.Fatalf("waits = %v, want all four readiness checks", cl.awaits)
	}
}

// A release at another version is an upgrade, and the plan says so with ~.
func TestApplyUpgradesAnotherVersion(t *testing.T) {
	cl := &fakeCluster{}
	rel := &fakeReleases{versions: map[string]string{
		"envoy-gateway-system/envoy-gateway": "1.9.2",
	}}
	d, log, _ := newDecision(cl, rel, &fakeConfirm{answer: true})

	if _, err := d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(rel.installed) != 2 || rel.installed[0].Name != EnvoyGatewayRelease {
		t.Fatalf("releases = %+v", rel.installed)
	}
	if !strings.Contains(log.String(), "~ envoy-gateway 1.9.3") {
		t.Fatalf("the plan did not mark the upgrade: %q", log.String())
	}
}

// MetalLB's webhook answers a moment after its pods are Ready. The pool apply
// is retried until it accepts, and the error is box's own.
func TestApplyRetriesThePool(t *testing.T) {
	cl := &fakeCluster{
		applyErrs: []error{errors.New("no endpoints available for service \"metallb-webhook-service\""), nil},
	}
	rel := &fakeReleases{versions: map[string]string{"metallb-system/metallb": MetalLBVersion}}
	d, log, _ := newDecision(cl, rel, &fakeConfirm{answer: true})
	d.PoolTimeout = 1000000000 // one second
	d.Poll = 1000000           // one millisecond

	if _, err := d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(cl.writes) != 3 {
		t.Fatalf("applies = %d, want the pool retried then the gateway and policy", len(cl.writes))
	}
	if !strings.Contains(log.String(), "applied IPAddressPool s5n-pool (192.0.2.10/32)") {
		t.Fatalf("the pool was not reported: %q", log.String())
	}
}

func TestApplyReportsCertManager(t *testing.T) {
	cl := &fakeCluster{}
	rel := &fakeReleases{}
	d, log, _ := newDecision(cl, rel, &fakeConfirm{answer: true})

	res, err := d.Apply(context.Background(), allInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotHandled) != 1 || !strings.Contains(res.NotHandled[0], "cert-manager") {
		t.Fatalf("cert-manager was silently skipped: %+v", res.NotHandled)
	}
	if !strings.Contains(log.String(), "! cert-manager: box does not install it yet") {
		t.Fatalf("cert-manager was not printed: %q", log.String())
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
	if policies != 2 {
		t.Fatalf("policy manifest has %d policies, want 2", policies)
	}
}
