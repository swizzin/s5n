package bootstrap

import (
	"context"
	"strings"
	"testing"
)

// classProbe is one cluster's GatewayClasses, with the component outcomes a
// read-only probe derives from them.
func classProbe(classes ...GatewayClass) PlatformProbe {
	probe := PlatformProbe{
		GatewayAPI:   OutcomePresent,
		LoadBalancer: OutcomeNotProbed,
		CertManager:  OutcomePresent,
		Classes:      classes,
	}
	for _, c := range classes {
		if c.Accepted {
			probe.Gateway = OutcomePresent
			break
		}
	}
	if probe.Gateway != OutcomePresent {
		probe.Gateway = OutcomeAbsent
	}
	return probe
}

// classFixture runs Bootstrap against one explicit cluster whose probe returns
// platform. It records what the decision wrote, so a refusal can be checked to
// have written nothing. newClassFixture is a k3s cluster, which is where the
// Traefik refusal has its own fix; newClassFixtureOn is any distribution.
type classFixture struct {
	recs  *fakeRecords
	probe *fakeProber
	d     *Decision
}

func newClassFixture(platform PlatformProbe) *classFixture {
	return newClassFixtureOn(platform, "v1.37.2+k3s1")
}

func newClassFixtureOn(platform PlatformProbe, version string) *classFixture {
	const path = "/explicit.yaml"
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	prober := &fakeProber{version: Version{GitVersion: version}, cluster: Cluster{Nodes: 1}, platform: platform}
	recs := &fakeRecords{}
	d := newDecision(src, prober, &fakeHost{}, recs, &fakeInstaller{})
	return &classFixture{recs: recs, probe: prober, d: d}
}

func (f *classFixture) run(t *testing.T, opts Options) Result {
	t.Helper()
	opts.Kubeconfig = "/explicit.yaml"
	res, err := f.d.Bootstrap(context.Background(), opts)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return res
}

func envoyClass(name string) GatewayClass {
	return GatewayClass{Name: name, ControllerName: EnvoyGatewayController, Accepted: true}
}

func foreignClass(name, controller string) GatewayClass {
	return GatewayClass{Name: name, ControllerName: controller, Accepted: true}
}

const (
	nginxController   = "gateway.nginx.org/nginx-gateway-controller"
	traefikController = "traefik.io/gateway-controller"
)

// One Accepted Envoy class is used, and the Gateway is planned on it: no
// question is asked and no implementation is installed.
func TestGatewayClassEnvoySingleAcceptedIsUsed(t *testing.T) {
	f := newClassFixture(classProbe(envoyClass("eg")))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != "eg" || res.Gateway.ControllerName != EnvoyGatewayController || res.Gateway.BoxInstalls {
		t.Fatalf("gateway = %+v, want the accepted Envoy class and no install", res.Gateway)
	}
	if len(f.recs.saved) != 1 || f.recs.saved[0].GatewayClass != "eg" {
		t.Fatalf("the record does not carry the class: %+v", f.recs.saved)
	}
	if gate := componentAction(res.Platform, ComponentGateway); gate != "use" {
		t.Fatalf("Gateway action = %q, want use", gate)
	}
}

// A class box created is accepted on the same rule as any other class: its
// controller is Envoy Gateway's. The managed-by label no longer skips a check.
func TestGatewayClassBoxOwnedEnvoyClassIsUsed(t *testing.T) {
	c := envoyClass("eg")
	c.ManagedByBox = true
	f := newClassFixture(classProbe(c))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster || res.Gateway.Class != "eg" {
		t.Fatalf("action=%q gateway=%+v, want box's own class used", res.Action, res.Gateway)
	}
}

// A foreign implementation's Accepted class stops bootstrap: box names the
// class and its controller, says Envoy Gateway is the only implementation it
// supports, and writes nothing.
func TestGatewayClassForeignAcceptedStops(t *testing.T) {
	f := newClassFixture(classProbe(foreignClass("nginx", nginxController)))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	for _, want := range []string{
		`GatewayClass "nginx" (controller gateway.nginx.org/nginx-gateway-controller) is Accepted`,
		"controller " + nginxController,
		"box supports only Envoy Gateway",
		"controller " + EnvoyGatewayController,
		"box supports only Envoy Gateway (controller gateway.envoyproxy.io/gatewayclass-controller) and puts no routes on another implementation",
	} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class choice wrote the record")
	}
	if res.Gateway.Class != "" {
		t.Fatalf("a stopped decision carries a class: %+v", res.Gateway)
	}
}

// Several foreign classes are named one by one, not truncated to the first.
func TestGatewayClassSeveralForeignAcceptedStops(t *testing.T) {
	f := newClassFixture(classProbe(
		foreignClass("nginx", nginxController),
		foreignClass("traefik", traefikController),
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	for _, want := range []string{`"nginx" (controller ` + nginxController + `)`, `"traefik" (controller ` + traefikController + `)`, "are Accepted", "box supports only Envoy Gateway"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class choice wrote the record")
	}
}

// An Accepted Envoy class beside another implementation is box's class: the
// other implementation existing is not box's business, and no question is
// asked about it.
func TestGatewayClassForeignBesideEnvoyUsesEnvoy(t *testing.T) {
	f := newClassFixture(classProbe(
		foreignClass("nginx", nginxController),
		envoyClass("eg"),
	))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != "eg" || res.Gateway.BoxInstalls {
		t.Fatalf("gateway = %+v, want the Accepted Envoy class used", res.Gateway)
	}
}

// Two Accepted Envoy classes with nobody to ask is the unattended refusal: it
// names the setting and the flag, and writes nothing. The foreign class in the
// list is not offered and not named as a choice.
func TestGatewayClassSeveralEnvoyUnattendedStops(t *testing.T) {
	f := newClassFixture(classProbe(
		foreignClass("nginx", nginxController),
		envoyClass("eg"),
		envoyClass("envoy"),
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"platform.gateway.className", "--gateway-class", "eg", "envoy"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not name %q: %s", want, res.Reason)
		}
	}
	if strings.Contains(res.Reason, "nginx") {
		t.Fatalf("the refusal offered a foreign implementation: %s", res.Reason)
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class choice wrote the record")
	}
}

// recordingChooser is the guided answer: it sees the list box would render and
// returns one of its names.
type recordingChooser struct {
	got    []GatewayClass
	answer string
	err    error
}

func (c *recordingChooser) ChooseGatewayClass(classes []GatewayClass) (string, error) {
	c.got = append([]GatewayClass(nil), classes...)
	return c.answer, c.err
}

func TestGatewayClassSeveralEnvoyGuidedPicks(t *testing.T) {
	chooser := &recordingChooser{answer: "envoy"}
	f := newClassFixture(classProbe(
		foreignClass("nginx", nginxController),
		envoyClass("eg"),
		envoyClass("envoy"),
	))
	f.d.Prompt = chooser
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != "envoy" {
		t.Fatalf("class = %q, want the operator's pick", res.Gateway.Class)
	}
	if len(chooser.got) != 2 || chooser.got[0].Name != "eg" || chooser.got[1].Name != "envoy" {
		t.Fatalf("the prompt did not see exactly the accepted Envoy classes: %+v", chooser.got)
	}
	if chooser.got[0].ControllerName != EnvoyGatewayController {
		t.Fatal("the prompt did not see the controller names")
	}
}

// A configured class wins over the list, and a class that exists but is not
// Accepted stops with the controller's own reason: box will not attach routes
// to a class nobody serves.
func TestGatewayClassConfiguredNotAcceptedStops(t *testing.T) {
	notAccepted := envoyClass("envoy")
	notAccepted.Accepted = false
	notAccepted.AcceptedReason = "Waiting for controller"
	f := newClassFixture(classProbe(envoyClass("eg"), notAccepted))
	res := f.run(t, Options{GatewayClass: "envoy"})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"envoy", "not Accepted", "Waiting for controller"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

func TestGatewayClassConfiguredUnknownStops(t *testing.T) {
	f := newClassFixture(classProbe(envoyClass("eg")))
	res := f.run(t, Options{GatewayClass: "envoy"})
	if res.Action != ActionStop || !strings.Contains(res.Reason, "no GatewayClass by that name") {
		t.Fatalf("action=%q reason=%q, want a refusal naming the missing class", res.Action, res.Reason)
	}
}

// Naming another implementation's class explicitly gets the rule, not a
// not-found: the class is there, and box does not use it.
func TestGatewayClassConfiguredForeignStops(t *testing.T) {
	f := newClassFixture(classProbe(
		envoyClass("eg"),
		foreignClass("nginx", nginxController),
	))
	res := f.run(t, Options{GatewayClass: "nginx"})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"platform.gateway.className", "nginx", nginxController, "box supports only Envoy Gateway"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

// An Envoy Gateway class exists but is not Accepted: box does not install its
// own implementation beside one that is merely not ready yet.
func TestGatewayClassEnvoyNoneAcceptedStopsInsteadOfInstallingEnvoy(t *testing.T) {
	c := envoyClass("eg")
	c.Accepted = false
	c.AcceptedReason = "Waiting for controller"
	f := newClassFixture(classProbe(c))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"none is Accepted", "will not install Envoy Gateway beside", "Waiting for controller", "platform.gateway.className"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
}

// A foreign class that is not Accepted is refused for what it is, not told to
// wait for an acceptance that would not make box use it.
func TestGatewayClassForeignNoneAcceptedStops(t *testing.T) {
	c := foreignClass("nginx", nginxController)
	c.Accepted = false
	c.AcceptedReason = "Waiting for controller"
	f := newClassFixture(classProbe(c))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"none is Accepted", "Waiting for controller", "box supports only Envoy Gateway", nginxController} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if strings.Contains(res.Reason, "Fix the implementation so its class is Accepted") {
		t.Fatalf("a foreign class was told to wait for an acceptance box would still refuse: %s", res.Reason)
	}
}

// With no GatewayClass at all box installs Envoy Gateway and creates its class:
// the one case where box owns the implementation.
func TestGatewayClassNoneInstallsEnvoy(t *testing.T) {
	f := newClassFixture(PlatformProbe{
		GatewayAPI:   OutcomeAbsent,
		LoadBalancer: OutcomeNotProbed,
		Gateway:      OutcomeAbsent,
		CertManager:  OutcomePresent,
	})
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != EnvoyGatewayClass || !res.Gateway.BoxInstalls {
		t.Fatalf("gateway = %+v, want class %s installed by box", res.Gateway, EnvoyGatewayClass)
	}
	if gate := componentAction(res.Platform, ComponentGateway); gate != "install" {
		t.Fatalf("Gateway action = %q, want install", gate)
	}
}

// k3s ships Traefik as its own Gateway implementation, and no box setting can
// remove it: the refusal names the k3s server flag that can.
func TestGatewayClassK3sTraefikStopsNamingDisableFlag(t *testing.T) {
	f := newClassFixture(classProbe(foreignClass("traefik", traefikController)))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	for _, want := range []string{`GatewayClass "traefik" (controller traefik.io/gateway-controller) is Accepted`, traefikController, "box supports only Envoy Gateway", "k3s", "`--disable=traefik`"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

// The flag is k3s's, not a general Traefik fix: the same class on another
// distribution is refused without it.
func TestGatewayClassTraefikOnAnotherDistributionDoesNotNameK3s(t *testing.T) {
	f := newClassFixtureOn(classProbe(foreignClass("traefik", traefikController)), "v1.31.3")
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	if strings.Contains(res.Reason, "k3s") || strings.Contains(res.Reason, "--disable=traefik") {
		t.Fatalf("a non-k3s refusal named k3s's flag: %s", res.Reason)
	}
}

// A cluster with no Accepted class whose only class is Traefik's is still k3s's
// problem, so that refusal carries the same fix.
func TestGatewayClassNoneAcceptedOnK3sNamesDisableFlag(t *testing.T) {
	c := foreignClass("traefik", traefikController)
	c.Accepted = false
	c.AcceptedReason = "Waiting for controller"
	f := newClassFixture(classProbe(c))
	res := f.run(t, Options{})
	if res.Action != ActionStop || !strings.Contains(res.Reason, "`--disable=traefik`") {
		t.Fatalf("action=%q reason=%q, want the k3s fix named", res.Action, res.Reason)
	}
}

func componentAction(components []ComponentResult, component PlatformComponent) string {
	for _, c := range components {
		if c.Component == component {
			return c.Action
		}
	}
	return ""
}
