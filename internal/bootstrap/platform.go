package bootstrap

import (
	"fmt"
)

// PlatformMode is how a platform component is owned.
type PlatformMode string

const (
	// ModeAuto probes for a working instance; uses it if found, installs one
	// if not.
	ModeAuto PlatformMode = "auto"
	// ModeManaged is always box's own: box installs, upgrades and removes it.
	ModeManaged PlatformMode = "managed"
	// ModeExternal is the user's: box never installs or touches it, only
	// verifies it.
	ModeExternal PlatformMode = "external"
)

// PlatformComponent is one of the four pieces of the platform layer.
type PlatformComponent string

const (
	ComponentGatewayAPI   PlatformComponent = "gateway-api"
	ComponentLoadBalancer PlatformComponent = "loadbalancer"
	ComponentGateway      PlatformComponent = "gateway"
	ComponentCertManager  PlatformComponent = "cert-manager"
)

// ProbeOutcome is what a platform capability probe found.
type ProbeOutcome string

const (
	// OutcomePresent: a working instance exists.
	OutcomePresent ProbeOutcome = "present"
	// OutcomeAbsent: no working instance.
	OutcomeAbsent ProbeOutcome = "absent"
	// OutcomeConflict: something conflicting is present (box stops in every mode).
	OutcomeConflict ProbeOutcome = "conflict"
	// OutcomeNotProbed: the capability was not probed (e.g. dry run).
	OutcomeNotProbed ProbeOutcome = "not-probed"
)

// PlatformConfig holds the configured mode per platform component.
type PlatformConfig struct {
	GatewayAPI   PlatformMode
	LoadBalancer PlatformMode
	Gateway      PlatformMode
	CertManager  PlatformMode
}

// DefaultPlatform returns the default modes: everything auto.
func DefaultPlatform() PlatformConfig {
	return PlatformConfig{
		GatewayAPI:   ModeAuto,
		LoadBalancer: ModeAuto,
		Gateway:      ModeAuto,
		CertManager:  ModeAuto,
	}
}

// PlatformProbe holds the probe outcome per component.
type PlatformProbe struct {
	GatewayAPI   ProbeOutcome
	LoadBalancer ProbeOutcome
	Gateway      ProbeOutcome
	CertManager  ProbeOutcome
}

// ComponentResult is the decision for one platform component.
type ComponentResult struct {
	Component PlatformComponent
	Mode      PlatformMode
	Outcome   ProbeOutcome
	Action    string // "use", "install", "verify", "stop", "unknown"
	Reason    string
}

// componentSet is the fixed order of the four platform components.
var componentSet = []struct {
	comp PlatformComponent
	mode func(PlatformConfig) PlatformMode
	out  func(PlatformProbe) ProbeOutcome
}{
	{ComponentGatewayAPI, func(c PlatformConfig) PlatformMode { return c.GatewayAPI }, func(p PlatformProbe) ProbeOutcome { return p.GatewayAPI }},
	{ComponentLoadBalancer, func(c PlatformConfig) PlatformMode { return c.LoadBalancer }, func(p PlatformProbe) ProbeOutcome { return p.LoadBalancer }},
	{ComponentGateway, func(c PlatformConfig) PlatformMode { return c.Gateway }, func(p PlatformProbe) ProbeOutcome { return p.Gateway }},
	{ComponentCertManager, func(c PlatformConfig) PlatformMode { return c.CertManager }, func(p PlatformProbe) ProbeOutcome { return p.CertManager }},
}

// decidePlatform resolves each component's mode against its probe outcome. It
// returns the per-component results and, when any probe found something
// conflicting — which stops box in every mode — a non-empty stop reason.
func decidePlatform(cfg PlatformConfig, probe PlatformProbe) ([]ComponentResult, string) {
	out := make([]ComponentResult, 0, len(componentSet))
	for _, c := range componentSet {
		cr := ComponentResult{
			Component: c.comp,
			Mode:      c.mode(cfg),
			Outcome:   c.out(probe),
		}
		cr.Action, cr.Reason = decideComponent(cr.Mode, cr.Outcome)
		out = append(out, cr)
	}
	for _, cr := range out {
		if cr.Outcome == OutcomeConflict {
			return out, fmt.Sprintf("platform conflict: %s: %s", cr.Component, cr.Reason)
		}
	}
	return out, ""
}

// decideComponent maps a mode and a probe outcome to an action.
func decideComponent(mode PlatformMode, out ProbeOutcome) (action, reason string) {
	switch out {
	case OutcomeConflict:
		return "stop", "a conflicting implementation is present; box will not fight it"
	case OutcomeNotProbed:
		return "unknown", "capability not probed"
	case OutcomePresent:
		switch mode {
		case ModeManaged:
			return "install", "managed: box installs its own even though one is present"
		case ModeExternal:
			return "verify", "external: verified, never installed or touched"
		default:
			return "use", "auto: a working instance was found"
		}
	case OutcomeAbsent:
		switch mode {
		case ModeExternal:
			return "stop", "external mode but no working instance was found"
		default:
			return "install", "no working instance; box installs one"
		}
	default:
		return "unknown", "unexpected probe outcome"
	}
}

// platformFromConfig renders a PlatformConfig into results with nothing probed:
// what a recorded cluster shows on a re-run, when capability probes are skipped.
func platformFromConfig(cfg PlatformConfig) []ComponentResult {
	return platformFromOutcomes(cfg, PlatformProbe{})
}

func platformFromOutcomes(cfg PlatformConfig, probe PlatformProbe) []ComponentResult {
	out := make([]ComponentResult, 0, len(componentSet))
	for _, c := range componentSet {
		cr := ComponentResult{
			Component: c.comp,
			Mode:      c.mode(cfg),
			Outcome:   c.out(probe),
		}
		cr.Action, cr.Reason = decideComponent(cr.Mode, cr.Outcome)
		out = append(out, cr)
	}
	return out
}

// storageClassRequired reports whether a cluster with this many nodes must use
// a storage class instead of hostPath (hostPath is single-node only).
func storageClassRequired(nodes int) bool { return nodes > 1 }
