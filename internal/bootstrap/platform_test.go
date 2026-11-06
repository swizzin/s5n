package bootstrap

import (
	"strings"
	"testing"
)

func TestDecideComponent(t *testing.T) {
	cases := []struct {
		mode       PlatformMode
		out        ProbeOutcome
		wantAction string
	}{
		// auto: use when present, install when absent
		{ModeAuto, OutcomePresent, "use"},
		{ModeAuto, OutcomeAbsent, "install"},
		// managed: always install ours, present or absent
		{ModeManaged, OutcomePresent, "install"},
		{ModeManaged, OutcomeAbsent, "install"},
		// external: verify when present, stop when absent
		{ModeExternal, OutcomePresent, "verify"},
		{ModeExternal, OutcomeAbsent, "stop"},
		// conflict stops in every mode
		{ModeAuto, OutcomeConflict, "stop"},
		{ModeManaged, OutcomeConflict, "stop"},
		{ModeExternal, OutcomeConflict, "stop"},
		// not probed is unknown in every mode
		{ModeAuto, OutcomeNotProbed, "unknown"},
		{ModeExternal, OutcomeNotProbed, "unknown"},
	}
	for _, tc := range cases {
		got, _ := decideComponent(tc.mode, tc.out)
		if got != tc.wantAction {
			t.Errorf("decideComponent(%s, %s) = %q, want %q", tc.mode, tc.out, got, tc.wantAction)
		}
	}
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
