package platform

// The plan a flow shows before it applies it: the same BuildPlan with the firewall
// line, without reading the cluster.

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
)

// A host whose firewall trusts the cluster's networks gets that line first, as Apply's
// plan does, with every component the decision said to install.
func TestPreviewIsBuildPlanWithTheFirewallLine(t *testing.T) {
	plan, err := Preview(pins, allInstall, "eg", "", bootstrap.FirewallState{
		Active: true,
		CIDRs:  []string{"10.244.0.0/16", "10.96.0.0/12"},
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("the preview is empty")
	}
	if plan.Steps[0].Kind != StepFirewall {
		t.Fatalf("the preview's first step is %q, want the firewall", plan.Steps[0].Kind)
	}
	if plan.Steps[0].Plan != "firewalld's trusted zone" {
		t.Errorf("the firewall line is %q, want the zone alone: the networks are what the step does", plan.Steps[0].Plan)
	}
	if !strings.Contains(plan.Steps[0].What, "10.244.0.0/16 and 10.96.0.0/12") {
		t.Errorf("the firewall step is %q, want the two networks", plan.Steps[0].What)
	}
	items := plan.Items()
	if len(items) == 0 {
		t.Fatal("the previewed plan renders nothing")
	}
	rendered := strings.Join(texts(items), "\n")
	for _, want := range []string{"Envoy Gateway", "cert-manager", "MetalLB", "firewalld's trusted zone"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the previewed plan does not name %q:\n%s", want, rendered)
		}
	}
}

// No firewalld, no line: the plan is BuildPlan's own.
func TestPreviewWithoutAFirewallSaysNothingAboutIt(t *testing.T) {
	plan, err := Preview(pins, allInstall, "eg", "", bootstrap.FirewallState{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	for _, s := range plan.Steps {
		if s.Kind == StepFirewall {
			t.Errorf("a host with no firewalld gets a firewall step: %+v", s)
		}
	}
}

// The preview is BuildPlan, so a decision bootstrap would stop on is a refusal, not a
// plan with the step quietly missing.
func TestPreviewRefusesAComponentItCannotInstall(t *testing.T) {
	comps := components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGatewayAPI:   "install",
		bootstrap.ComponentGateway:      "install",
		bootstrap.ComponentCertManager:  "install",
		bootstrap.ComponentLoadBalancer: "stop",
	})
	if _, err := Preview(pins, comps, "eg", "", bootstrap.FirewallState{}); err == nil {
		t.Fatal("a component bootstrap stopped on was previewed as a plan")
	}
}

// texts is each item's line, children included.
func texts(items []say.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Text)
		for _, child := range it.Children {
			out = append(out, child.Text)
		}
	}
	return out
}
