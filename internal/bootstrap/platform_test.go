package bootstrap

import (
	"strings"
	"testing"
)

func TestDecideComponent(t *testing.T) {
	cases := []struct {
		component  PlatformComponent
		mode       PlatformMode
		out        ProbeOutcome
		wantAction string
	}{
		// auto: use when present, install when absent
		{ComponentGateway, ModeAuto, OutcomePresent, "use"},
		{ComponentGateway, ModeAuto, OutcomeAbsent, "install"},
		// managed: always install ours, present or absent
		{ComponentGateway, ModeManaged, OutcomePresent, "install"},
		{ComponentGateway, ModeManaged, OutcomeAbsent, "install"},
		// external: verify when present, stop when absent
		{ComponentGateway, ModeExternal, OutcomePresent, "verify"},
		{ComponentGateway, ModeExternal, OutcomeAbsent, "stop"},
		// conflict stops in every mode
		{ComponentGateway, ModeAuto, OutcomeConflict, "stop"},
		{ComponentGateway, ModeManaged, OutcomeConflict, "stop"},
		{ComponentGateway, ModeExternal, OutcomeConflict, "stop"},
		// not probed is unknown for every capability whose probe is read-only
		{ComponentGateway, ModeAuto, OutcomeNotProbed, "unknown"},
		{ComponentCertManager, ModeExternal, OutcomeNotProbed, "unknown"},
		// the LoadBalancer's probe is a write, so not probed means after the
		// confirm: a conditional install in auto, an install in managed, a
		// verification in external.
		{ComponentLoadBalancer, ModeAuto, OutcomeNotProbed, "probe"},
		{ComponentLoadBalancer, ModeManaged, OutcomeNotProbed, "install"},
		{ComponentLoadBalancer, ModeExternal, OutcomeNotProbed, "verify"},
	}
	for _, tc := range cases {
		got, _ := decideComponent(tc.component, tc.mode, tc.out)
		if got != tc.wantAction {
			t.Errorf("decideComponent(%s, %s, %s) = %q, want %q", tc.component, tc.mode, tc.out, got, tc.wantAction)
		}
	}
}

// The decision block bootstrap prints before the platform plan must not
// contradict the plan: a not-probed LoadBalancer is not "unknown", it is the
// question the plan prints as "unless the Gateway gets an address without it".
func TestDecidePlatformLoadBalancerSaysWhenItIsDecided(t *testing.T) {
	components, conflict := decidePlatform(DefaultPlatform(), PlatformProbe{
		GatewayAPI:   OutcomePresent,
		LoadBalancer: OutcomeNotProbed,
		Gateway:      OutcomePresent,
		CertManager:  OutcomePresent,
	})
	if conflict != "" {
		t.Fatalf("unexpected conflict: %s", conflict)
	}
	for _, c := range components {
		if c.Component != ComponentLoadBalancer {
			continue
		}
		if c.Action != "probe" {
			t.Fatalf("auto LoadBalancer action = %q, want probe", c.Action)
		}
		if !strings.Contains(c.Reason, "unless the Gateway gets an address without it") {
			t.Fatalf("the decision does not say what the plan decides: %q", c.Reason)
		}
		if strings.Contains(c.Reason, "not probed") {
			t.Fatalf("the decision still reads as undecided: %q", c.Reason)
		}
		return
	}
	t.Fatal("no LoadBalancer result")
}

func TestDecidePlatformConflictStopsInEveryMode(t *testing.T) {
	for _, mode := range []PlatformMode{ModeAuto, ModeManaged, ModeExternal} {
		cfg := PlatformConfig{
			GatewayAPI:   mode,
			LoadBalancer: mode,
			Gateway:      mode,
			CertManager:  mode,
		}
		probe := PlatformProbe{Gateway: OutcomeConflict}
		components, conflict := decidePlatform(cfg, probe)
		if conflict == "" {
			t.Errorf("mode %s: a conflicting Gateway must stop box, got no conflict", mode)
		}
		if !strings.Contains(conflict, "conflict") {
			t.Errorf("mode %s: conflict reason = %q", mode, conflict)
		}
		if len(components) != 4 {
			t.Errorf("mode %s: expected 4 component results, got %d", mode, len(components))
		}
		// the conflicting component's own action is "stop" in every mode
		for _, c := range components {
			if c.Component == ComponentGateway && c.Action != "stop" {
				t.Errorf("mode %s: Gateway action = %q, want stop", mode, c.Action)
			}
		}
	}
}

func TestDecidePlatformAutoUsesPresentInstallsAbsent(t *testing.T) {
	cfg := DefaultPlatform()
	probe := PlatformProbe{
		GatewayAPI:   OutcomePresent,
		LoadBalancer: OutcomeAbsent,
		Gateway:      OutcomePresent,
		CertManager:  OutcomeAbsent,
	}
	components, conflict := decidePlatform(cfg, probe)
	if conflict != "" {
		t.Fatalf("unexpected conflict: %s", conflict)
	}
	actions := map[PlatformComponent]string{}
	for _, c := range components {
		actions[c.Component] = c.Action
	}
	if actions[ComponentGatewayAPI] != "use" || actions[ComponentGateway] != "use" {
		t.Errorf("present components should be used: %v", actions)
	}
	if actions[ComponentLoadBalancer] != "install" || actions[ComponentCertManager] != "install" {
		t.Errorf("absent components should be installed: %v", actions)
	}
}

func TestDecidePlatformExternalAbsentStops(t *testing.T) {
	cfg := PlatformConfig{
		GatewayAPI:   ModeExternal,
		LoadBalancer: ModeExternal,
		Gateway:      ModeExternal,
		CertManager:  ModeExternal,
	}
	components, conflict := decidePlatform(cfg, PlatformProbe{
		GatewayAPI:   OutcomePresent,
		LoadBalancer: OutcomePresent,
		Gateway:      OutcomePresent,
		CertManager:  OutcomeAbsent,
	})
	if conflict != "" {
		t.Fatalf("external-absent is a per-component stop, not a platform conflict: %s", conflict)
	}
	for _, c := range components {
		if c.Component == ComponentCertManager && c.Action != "stop" {
			t.Errorf("external cert-manager absent must stop, got %q", c.Action)
		}
		if c.Component != ComponentCertManager && c.Action != "verify" {
			t.Errorf("external present component should verify, got %q for %s", c.Action, c.Component)
		}
	}
}
