package bootstrap

import (
	"context"
	"fmt"
)

// finishRecorded is the decision for a cluster a record names. The record is a cached
// decision, not evidence: it says which cluster, who owns its lifecycle, the platform
// modes and the class, while the cluster is probed again so a re-run plans against
// what is there now. That is what lets a second bootstrap repair a first run whose
// platform apply failed: the missing pieces are installs from the probe's outcomes.
//
// Nothing here writes the record: its decision has not changed, only its cached view,
// and rewriting that would hide a drift the next run still checks.
func (d *Decision) finishRecorded(ctx context.Context, obs clusterProbes, rec *Record, how string) Result {
	plat, err := obs.probe.Platform(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: platform probe: %v", how, rec.Kubeconfig, describeRemote(err)))
	}
	components, conflict := decidePlatform(rec.Platform, plat)
	action := ActionUseCluster
	if rec.Source == SourceK0sManaged {
		action = ActionUseManaged
	}
	res := Result{
		Action:       action,
		Source:       rec.Source,
		Kubeconfig:   rec.Kubeconfig,
		Distribution: obs.dist,
		Nodes:        obs.cluster.Nodes,
		Platform:     components,
		Warnings:     nodeIPWarnings(obs.cluster, d.net()),
	}
	if conflict != "" {
		res.Action = ActionStop
		res.Reason = conflict
		return res
	}
	choice, problem := d.recordedClass(rec, plat, gatewayComponent(components))
	if problem != "" {
		res.Action = ActionStop
		res.Reason = problem
		return res
	}
	res.Gateway = choice
	res = d.withFirewall(ctx, res)
	if rec.Source == SourceK0sManaged {
		res.Reason = fmt.Sprintf("recorded k0s-managed cluster at %s (%s); k0s is started and upgraded by box update (a later phase), platform re-probed", rec.Kubeconfig, obs.dist)
	} else {
		res.Reason = fmt.Sprintf("using recorded external cluster at %s (%s); lifecycle never touched, platform re-probed", rec.Kubeconfig, obs.dist)
	}
	return res
}

// recordedClass keeps the class the record chose while the cluster serves it: a class
// is a decision, and asking again could silently move a box's routes. The cluster is
// the evidence, so the class must be Accepted and Envoy Gateway's; one that is not, or
// is gone while another implementation serves, stops rather than moving routes.
//
// The one vanished class box repairs is its own `eg`, and only when no class is
// accepted at all: what box installed is missing, and the install writes it back.
// Managed mode owns its implementation and always writes `eg`.
func (d *Decision) recordedClass(rec *Record, probe PlatformProbe, gw ComponentResult) (GatewayChoice, string) {
	if rec.Platform.Gateway == ModeManaged {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	class := rec.GatewayClass
	if class == "" {
		// Older records came from a box that only ever wrote its own class.
		class = EnvoyGatewayClass
	}
	for _, c := range probe.Classes {
		if c.Name != class {
			continue
		}
		if !c.Accepted {
			return GatewayChoice{}, fmt.Sprintf("the recorded GatewayClass %q is not Accepted (%s): box will not attach its Gateway to a class no controller serves", c.Name, acceptedReason(c))
		}
		if !isEnvoyClass(c) {
			return GatewayChoice{}, fmt.Sprintf("the recorded GatewayClass %q (controller %s) is not Envoy Gateway, the only implementation box supports (controller %s): install Envoy Gateway in place of that implementation, then remove %s deliberately so box chooses a class again.%s",
				c.Name, controllerOr(c), EnvoyGatewayController, d.Records.RecordPath(), k3sTraefikHint(rec.Distribution, []GatewayClass{c}))
		}
		return choiceOf(c), ""
	}
	if class == EnvoyGatewayClass && gw.Action == "install" {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	return GatewayChoice{}, fmt.Sprintf("the recorded GatewayClass %q is gone from the cluster (accepted: %s): box will not move this seedbox's routes to another implementation.%s", class, names(acceptedClasses(probe.Classes)), k3sTraefikHint(rec.Distribution, probe.Classes))
}
