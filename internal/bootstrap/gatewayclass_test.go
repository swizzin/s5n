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
// have written nothing.
type classFixture struct {
	recs  *fakeRecords
	probe *fakeProber
	d     *Decision
}

func newClassFixture(platform PlatformProbe) *classFixture {
	const path = "/explicit.yaml"
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	prober := &fakeProber{version: Version{GitVersion: "v1.37.2+k3s1"}, cluster: Cluster{Nodes: 1}, platform: platform}
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

// One Accepted class is used, and the Gateway is planned on it: no question is
// asked and no implementation is installed.
func TestGatewayClassSingleAcceptedIsUsed(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
	))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != "nginx" || res.Gateway.BoxInstalls {
		t.Fatalf("gateway = %+v, want the accepted class and no install", res.Gateway)
	}
	if len(f.recs.saved) != 1 || f.recs.saved[0].GatewayClass != "nginx" {
		t.Fatalf("the record does not carry the class: %+v", f.recs.saved)
	}
	if gate := componentAction(res.Platform, ComponentGateway); gate != "use" {
		t.Fatalf("Gateway action = %q, want use", gate)
	}
}

// Several Accepted classes with nobody to ask is the unattended refusal: it
// names the setting and the flag, and writes nothing.
func TestGatewayClassSeveralUnattendedStops(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
		GatewayClass{Name: "traefik", ControllerName: "traefik.io/gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"platform.gateway.className", "--gateway-class", "nginx", "traefik"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not name %q: %s", want, res.Reason)
		}
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

func TestGatewayClassSeveralGuidedPicks(t *testing.T) {
	chooser := &recordingChooser{answer: "traefik"}
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
		GatewayClass{Name: "traefik", ControllerName: "traefik.io/gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
	))
	f.d.Prompt = chooser
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if res.Gateway.Class != "traefik" {
		t.Fatalf("class = %q, want the operator's pick", res.Gateway.Class)
	}
	if len(chooser.got) != 2 || chooser.got[0].Name != "nginx" || chooser.got[1].Name != "traefik" {
		t.Fatalf("the prompt did not see every accepted class: %+v", chooser.got)
	}
	if chooser.got[0].ControllerName == "" {
		t.Fatal("the prompt did not see the controller names")
	}
}

// A configured class wins over the list, and a class that exists but is not
// Accepted stops with the controller's own reason: box will not attach routes
// to a class nobody serves.
func TestGatewayClassConfiguredNotAcceptedStops(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
		GatewayClass{Name: "traefik", ControllerName: "traefik.io/gateway-controller", Accepted: false, AcceptedReason: "Waiting for controller"},
	))
	res := f.run(t, Options{GatewayClass: "traefik"})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"traefik", "not Accepted", "Waiting for controller"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

func TestGatewayClassConfiguredUnknownStops(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
	))
	res := f.run(t, Options{GatewayClass: "traefik"})
	if res.Action != ActionStop || !strings.Contains(res.Reason, "no GatewayClass by that name") {
		t.Fatalf("action=%q reason=%q, want a refusal naming the missing class", res.Action, res.Reason)
	}
}

// Classes exist but none is Accepted: box does not install Envoy Gateway
// beside an implementation that is merely not ready yet.
func TestGatewayClassNoneAcceptedStopsInsteadOfInstallingEnvoy(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: false, AcceptedReason: "Waiting for controller"},
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"none is Accepted", "will not install Envoy Gateway beside", "Waiting for controller"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
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

func componentAction(components []ComponentResult, component PlatformComponent) string {
	for _, c := range components {
		if c.Component == component {
			return c.Action
		}
	}
	return ""
}

// A class that publishes a feature list without HTTPRoutePathRewrite cannot
// serve the chart's URLRewrite; box stops, and never installs Envoy beside it.
func TestGatewayClassMissingPathRewriteStops(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true,
			Features: []string{"Gateway", "HTTPRoute", "HTTPRouteHostRewrite"}},
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"nginx", FeaturePathRewrite, "will not install Envoy Gateway beside", "ReplacePrefixMatch"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

// A class that publishes no feature list is only usable with the operator's
// explicit override, which the refusal names by setting and by flag.
func TestGatewayClassNoFeatureListStopsNamingTheOverride(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true},
	))
	res := f.run(t, Options{})
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"nginx", "publishes no supportedFeatures", "platform.gateway.assumeFeatures", "--gateway-assume-features"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("the refusal does not say %q: %s", want, res.Reason)
		}
	}
	if len(f.recs.saved) != 0 {
		t.Fatal("a refused class wrote the record")
	}
}

// The override continues, says once that box could not check, and records the
// choice so a re-run does not have to be told again.
func TestGatewayClassNoFeatureListOverrideContinues(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true},
	))
	res := f.run(t, Options{AssumeGatewayFeatures: true})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if !res.Gateway.Assumed || res.Gateway.FeaturesChecked {
		t.Fatalf("gateway = %+v, want the unchecked override", res.Gateway)
	}
	if len(f.recs.saved) != 1 || !f.recs.saved[0].AssumeGatewayFeatures {
		t.Fatalf("the override was not recorded: %+v", f.recs.saved)
	}
	if !strings.Contains(FormatResult(res), "platform.gateway.assumeFeatures is set") {
		t.Fatalf("box did not say it could not check: %s", FormatResult(res))
	}
}

// A published list that contains the feature is used, and box says so.
func TestGatewayClassFeatureChecked(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "nginx", ControllerName: "gateway.nginx.org/nginx-gateway-controller", Accepted: true, Features: []string{FeaturePathRewrite}},
	))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster (%s)", res.Action, res.Reason)
	}
	if !res.Gateway.FeaturesChecked {
		t.Fatalf("gateway = %+v, want the feature checked", res.Gateway)
	}
	if !strings.Contains(FormatResult(res), FeaturePathRewrite+" is supported") {
		t.Fatalf("box did not report the feature: %s", FormatResult(res))
	}
}

// A class box created beside its own Envoy Gateway needs no published feature
// list: box chose that implementation. This is the re-run of a box install.
func TestGatewayClassBoxOwnedSkipsFeatureCheck(t *testing.T) {
	f := newClassFixture(classProbe(
		GatewayClass{Name: "eg", ControllerName: "gateway.envoyproxy.io/gatewayclass-controller", Accepted: true, ManagedByBox: true},
	))
	res := f.run(t, Options{})
	if res.Action != ActionUseCluster || res.Gateway.Class != "eg" {
		t.Fatalf("action=%q gateway=%+v, want box's own class used", res.Action, res.Gateway)
	}
}
