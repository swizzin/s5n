package bootstrap

// The decision that writes nothing: a flow asks it before its own question, so a host
// whose answer is no is as it was. It must decide what the real run will, changing no
// cluster, k0s, record or firewall.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// previewInstaller fails when reached: a preview installs nothing.
func previewInstaller(t *testing.T) *fakeInstaller {
	t.Helper()
	trace := &[]string{}
	inst := &fakeInstaller{log: trace}
	t.Cleanup(func() {
		if len(inst.plans) != 0 {
			t.Errorf("a preview reached the k0s installer: %v", inst.plans)
		}
	})
	return inst
}

// The cluster box would create, and the platform that follows, without any of it
// happening.
func TestPreviewOnACleanHostDecidesAndInstallsNothing(t *testing.T) {
	recs := &fakeRecords{}
	inst := previewInstaller(t)
	d := newDecision(&fakeSource{}, nil, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{Preview: true, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall {
		t.Fatalf("action = %q, want install; reason: %s", res.Action, res.Reason)
	}
	if len(recs.saved) != 0 {
		t.Errorf("a preview recorded a cluster it did not install: %v", recs.saved)
	}
	// A fresh k0s's decision: everything installed, the address layer installed rather
	// than probed.
	actions := map[PlatformComponent]string{}
	for _, c := range res.Platform {
		actions[c.Component] = c.Action
	}
	for _, want := range []PlatformComponent{ComponentGatewayAPI, ComponentLoadBalancer, ComponentGateway, ComponentCertManager} {
		if actions[want] != "install" {
			t.Errorf("%s: action = %q, want install on a host with no cluster (%v)", want, actions[want], res.Platform)
		}
	}
	if res.Gateway.Class != EnvoyGatewayClass || !res.Gateway.BoxInstalls {
		t.Errorf("the preview's GatewayClass = %+v, want box's own %s, installed by box", res.Gateway, EnvoyGatewayClass)
	}
	if !strings.Contains(res.Reason, "would be installed") {
		t.Errorf("the reason does not say nothing has happened yet: %q", res.Reason)
	}
}

// A host running firewalld gets the networks from the text the install would write,
// not from a file that does not exist.
func TestPreviewOnACleanHostTrustsTheNetworksItWouldWrite(t *testing.T) {
	recs := &fakeRecords{}
	d := newDecision(&fakeSource{}, nil, &fakeHost{}, recs, previewInstaller(t))
	d.Firewall = Firewalld{Run: &fakeFirewallCmd{running: true}}

	res, err := d.Bootstrap(context.Background(), Options{Preview: true, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Firewall.Active {
		t.Fatal("the preview says firewalld is not running on a host where it is")
	}
	if len(res.Firewall.CIDRs) != 2 {
		t.Fatalf("the preview would trust %v, want the pod and service networks", res.Firewall.CIDRs)
	}
	if res.Firewall.Warning != "" {
		t.Errorf("the preview carries a warning about a firewall it can plan: %q", res.Firewall.Warning)
	}
}

// Reading the filter is the preview's whole contact with it; the plan adds the
// networks after the answer.
func TestPreviewChangesNoFirewall(t *testing.T) {
	fw := &fakeFirewallCmd{running: true}
	d := newDecision(&fakeSource{}, nil, &fakeHost{}, &fakeRecords{}, previewInstaller(t))
	d.Firewall = Firewalld{Run: fw}

	if _, err := d.Bootstrap(context.Background(), Options{Preview: true, Platform: DefaultPlatform()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range fw.calls {
		if strings.Contains(call, "--add-source") {
			t.Errorf("the preview changed the firewall: %s", call)
		}
	}
}

// A cluster box was given is probed and the decision returned without a record: one
// is what makes a host "have a box", and the answer has not been given.
func TestPreviewOnAnExplicitClusterRecordsNothing(t *testing.T) {
	const path = "/explicit.yaml"
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	prober := &fakeProber{
		version: Version{GitVersion: "v1.37.2"},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomeAbsent,
			LoadBalancer: OutcomeAbsent,
			Gateway:      OutcomeAbsent,
			CertManager:  OutcomeAbsent,
		},
	}
	recs := &fakeRecords{}
	inst := previewInstaller(t)
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{Preview: true, Kubeconfig: path, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; reason: %s", res.Action, res.Reason)
	}
	if len(recs.saved) != 0 {
		t.Errorf("a preview recorded the cluster it only looked at: %v", recs.saved)
	}
	if len(res.Platform) != len(componentSet) {
		t.Fatalf("platform = %v, want one decision per component", res.Platform)
	}
	for _, c := range res.Platform {
		if c.Action != "install" {
			t.Errorf("%s: action = %q, want install on a cluster with no platform layer", c.Component, c.Action)
		}
	}
}

// The same host, previewed and then decided for real, gives the same platform and
// class.
func TestPreviewIsTheDecisionTheRunThenMakes(t *testing.T) {
	const path = "/explicit.yaml"
	platform := PlatformProbe{
		GatewayAPI:   OutcomeAbsent,
		LoadBalancer: OutcomeAbsent,
		Gateway:      OutcomeAbsent,
		CertManager:  OutcomeAbsent,
	}
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	previewed := newDecision(src, &fakeProber{version: Version{GitVersion: "v1.37.2"}, cluster: Cluster{Nodes: 1}, platform: platform}, &fakeHost{}, &fakeRecords{}, &fakeInstaller{})
	res, err := previewed.Bootstrap(context.Background(), Options{Preview: true, Kubeconfig: path, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	installed := newDecision(src, &fakeProber{version: Version{GitVersion: "v1.37.2"}, cluster: Cluster{Nodes: 1}, platform: platform}, &fakeHost{}, &fakeRecords{}, &fakeInstaller{})
	real, err := installed.Bootstrap(context.Background(), Options{Kubeconfig: path, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(res.Platform) != len(real.Platform) {
		t.Fatalf("the preview decided %v and the run %v", res.Platform, real.Platform)
	}
	for i := range res.Platform {
		if res.Platform[i].Action != real.Platform[i].Action || res.Platform[i].Component != real.Platform[i].Component {
			t.Errorf("component %d: preview %v, run %v", i, res.Platform[i], real.Platform[i])
		}
	}
	if res.Gateway.Class != real.Gateway.Class {
		t.Errorf("the preview chose class %q and the run %q", res.Gateway.Class, real.Gateway.Class)
	}
}

// The preview does not write the config the install would.
func TestPreviewWritesNoK0sConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k0s.yaml")
	d := newDecision(&fakeSource{}, nil, &fakeHost{}, &fakeRecords{}, previewInstaller(t))
	d.K0sConfigFile = path

	if _, err := d.Bootstrap(context.Background(), Options{Preview: true, Platform: DefaultPlatform()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the preview wrote a k0s config at %s", path)
	}
}
