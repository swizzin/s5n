package bootstrap

import (
	"context"
	"fmt"
)

// finishRecorded is the decision for a cluster a record names. The record is a
// cached decision, not evidence: it says which cluster box chose, who owns its
// lifecycle, the platform modes and the GatewayClass, but the cluster itself is
// probed again so a re-run plans against what is there now. That is what makes
// a second bootstrap repair a first run whose platform apply failed: the
// missing Envoy Gateway, Gateway API CRDs, MetalLB and admission policy are
// installs from the probe's outcomes, not the record's old configuration.
//
// Nothing here writes the record. The decision a record holds has not changed;
// only its cached view of the cluster has, and rewriting it would hide a drift
// the next run still has to check.
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
	if rec.Source == SourceK0sManaged {
		res.Reason = fmt.Sprintf("recorded k0s-managed cluster at %s (%s); k0s is started and upgraded by box update (a later phase), platform re-probed", rec.Kubeconfig, obs.dist)
	} else {
		res.Reason = fmt.Sprintf("using recorded external cluster at %s (%s); lifecycle never touched, platform re-probed", rec.Kubeconfig, obs.dist)
	}
	return res
}

// recordedClass keeps the class the record chose while the cluster still
// serves it: the class is a decision, and asking again could silently move a
// box's routes, so the record wins over a second look. The cluster is still
// the evidence, though, so the class must be Accepted and must publish the
// feature the chart's routes need (under the assumeFeatures choice the record
// carries). A recorded class that is no longer Accepted stops with the same
// refusal a chosen class gets; so does one that is gone while another
// implementation is serving the cluster, because box will not move a box's
// routes to a different implementation on its own.
//
// The one vanished class box repairs is its own `eg`, and only when the
// platform decision found no accepted class at all: then the implementation
// box installed is what is missing, and the install below writes the class back
// with it. Managed mode owns its implementation and always writes `eg`.
func (d *Decision) recordedClass(rec *Record, probe PlatformProbe, gw ComponentResult) (GatewayChoice, string) {
	if rec.Platform.Gateway == ModeManaged {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	class := rec.GatewayClass
	if class == "" {
		// Records written before box recorded the class came from a box that
		// only ever wrote its own.
		class = EnvoyGatewayClass
	}
	for _, c := range probe.Classes {
		if c.Name != class {
			continue
		}
		if !c.Accepted {
			return GatewayChoice{}, fmt.Sprintf("the recorded GatewayClass %q is not Accepted (%s): box will not attach its Gateway to a class no controller serves", c.Name, acceptedReason(c))
		}
		return d.withFeatures(Options{AssumeGatewayFeatures: rec.AssumeGatewayFeatures}, c)
	}
	if class == EnvoyGatewayClass && gw.Action == "install" {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	return GatewayChoice{}, fmt.Sprintf("the recorded GatewayClass %q is gone from the cluster (accepted: %s): box will not move this box's routes to another implementation", class, names(acceptedClasses(probe.Classes)))
}
