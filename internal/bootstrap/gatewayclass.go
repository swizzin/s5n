package bootstrap

import (
	"fmt"
	"sort"
	"strings"
)

// EnvoyGatewayClass is the GatewayClass box creates when box installs Envoy
// Gateway. It is the only class box ever creates.
const EnvoyGatewayClass = "eg"

// EnvoyGatewayController is the controllerName of the only Gateway API
// implementation box supports. Logins are checked at the edge by one provider
// through Envoy's SecurityPolicy (docs/box.md, "Logins"), and box builds no
// per-app fallback, so a class owned by any other controller cannot serve this
// box.
const EnvoyGatewayController = "gateway.envoyproxy.io/gatewayclass-controller"

// traefikGatewayController is k3s's bundled Traefik. It is not named as an
// implementation box might use: the refusal that finds it names k3s's
// --disable=traefik instead.
const traefikGatewayController = "traefik.io/gateway-controller"

// GatewayClass is one GatewayClass the probe found, reduced to what the class
// decision needs. The class is the operator's (or box's) statement that a
// controller is willing to serve Gateways; box never guesses one.
type GatewayClass struct {
	// Name is the GatewayClass's own name: what the Gateway says.
	Name string
	// ControllerName is spec.controllerName, which decides whether box may
	// use this class at all (see EnvoyGatewayController).
	ControllerName string
	// Accepted is the GatewayClass's Accepted condition being True.
	Accepted bool
	// AcceptedReason is the Accepted condition's message (or reason when it
	// carries no message) when the class is not Accepted. A class that is not
	// Accepted is never used, and saying why is the whole point of reading it.
	AcceptedReason string
	// ManagedByBox is the app.kubernetes.io/managed-by=box label on the
	// class: box created it beside the implementation box installed. It is
	// read by the probe and reported with the class; the decision no longer
	// branches on it.
	ManagedByBox bool
	// Features is status.supportedFeatures as the class published it. The
	// probe still records it; nothing decides from it: box supports only
	// Envoy Gateway, whose class publishes no list.
	Features []string
}

// GatewayChoice is the GatewayClass box will attach its Gateway to, and how box
// arrived at it. It travels with the decision so the plan does not have to
// re-read the cluster to know what class to write.
type GatewayChoice struct {
	// Class is the GatewayClass name the Gateway will reference.
	Class string
	// ControllerName is that class's controller, for box's output.
	ControllerName string
	// BoxInstalls says this run installs Envoy Gateway, and therefore creates
	// the GatewayClass.
	BoxInstalls bool
}

// ClassChooser asks which of several Accepted Envoy Gateway classes box should
// use. cmd/box implements it over setup.Prompter.Select, so the question is the
// same dialog `box init` asks with; the decision itself stays here, where every
// other bootstrap refusal lives.
type ClassChooser interface {
	ChooseGatewayClass(classes []GatewayClass) (string, error)
}

// gatewayChoice resolves the GatewayClass for this run. gw is the Gateway
// component's result, because "box installs its own" and "box uses one that is
// there" are decided there, not here. dist is the detected distribution, for
// the one refusal k3s can answer for itself (see k3sTraefikHint).
//
// The order is docs/box.md's decided paragraph: only a class accepted by
// Envoy's controller counts (a class for another implementation stops box); a
// chosen class wins (and must be Accepted); otherwise exactly one Accepted
// Envoy class is used; several Envoy classes are the operator's to choose;
// none means box installs Envoy Gateway. Classes that exist but are not
// Accepted stop box rather than letting it install Envoy underneath a
// controller that is merely still starting. A class for another implementation
// beside an Accepted Envoy class is not box's business: the Envoy class is used
// and the other implementation is left alone.
func (d *Decision) gatewayChoice(opts Options, probe PlatformProbe, gw ComponentResult, dist Distribution) (GatewayChoice, string) {
	accepted := acceptedClasses(probe.Classes)
	envoy := envoyClasses(probe.Classes)

	if gw.Mode == ModeManaged {
		// managed owns the implementation, so it owns the class too: box
		// installs Envoy Gateway and writes `eg`. Other classes are not a
		// conflict here; the operator asked for box's own.
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	if len(probe.Classes) > 0 && len(accepted) == 0 {
		if !hasEnvoyClass(probe.Classes) {
			// Nothing here is box's implementation, so waiting for an
			// acceptance would not help: the fix is to remove it.
			return GatewayChoice{}, fmt.Sprintf(
				"%d GatewayClass(es) exist but none is Accepted (%s), and box supports only Envoy Gateway (controller %s), so none of them would be usable once Accepted. %s",
				len(probe.Classes), describeClasses(probe.Classes), EnvoyGatewayController, foreignRemedy(dist, probe.Classes))
		}
		return GatewayChoice{}, fmt.Sprintf(
			"%d GatewayClass(es) exist but none is Accepted (%s): box will not install Envoy Gateway beside another implementation. Fix the implementation so its class is Accepted, or set platform.gateway.className (--gateway-class) to the class to use.%s",
			len(probe.Classes), describeClasses(probe.Classes), k3sTraefikHint(dist, probe.Classes))
	}
	if gw.Action == "install" {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	if gw.Action != "use" && gw.Action != "verify" {
		// A stop or an unknown action is refused (or decided) elsewhere; there
		// is no class to name.
		return GatewayChoice{}, ""
	}

	if opts.GatewayClass != "" {
		for _, c := range probe.Classes {
			if c.Name != opts.GatewayClass {
				continue
			}
			if !c.Accepted {
				return GatewayChoice{}, fmt.Sprintf("platform.gateway.className is %q, but that GatewayClass is not Accepted (%s): box will not attach its Gateway to a class no controller serves",
					c.Name, acceptedReason(c))
			}
			if !isEnvoyClass(c) {
				return GatewayChoice{}, fmt.Sprintf("platform.gateway.className is %q (controller %s), but box supports only Envoy Gateway (controller %s) and puts no routes on another implementation. Remove that setting and let box use an Envoy Gateway class.%s",
					c.Name, controllerOr(c), EnvoyGatewayController, k3sTraefikHint(dist, []GatewayClass{c}))
			}
			return choiceOf(c), ""
		}
		return GatewayChoice{}, fmt.Sprintf("platform.gateway.className is %q, but this cluster has no GatewayClass by that name (accepted: %s)",
			opts.GatewayClass, names(accepted))
	}

	if len(envoy) == 0 {
		return GatewayChoice{}, foreignGatewayClassRefusal(accepted, dist)
	}
	if len(envoy) == 1 {
		return choiceOf(envoy[0]), ""
	}

	if d.Prompt == nil {
		return GatewayChoice{}, fmt.Sprintf("several Envoy Gateway GatewayClasses are Accepted (%s): choose one with platform.gateway.className (--gateway-class) and run `box bootstrap` again",
			describeClasses(envoy))
	}
	chosen, err := d.Prompt.ChooseGatewayClass(envoy)
	if err != nil {
		return GatewayChoice{}, fmt.Sprintf("choosing a GatewayClass: %v", err)
	}
	for _, c := range envoy {
		if c.Name == chosen {
			return choiceOf(c), ""
		}
	}
	return GatewayChoice{}, fmt.Sprintf("the chosen GatewayClass %q is not one of the Accepted Envoy Gateway classes (%s)", chosen, names(envoy))
}

func choiceOf(c GatewayClass) GatewayChoice {
	return GatewayChoice{Class: c.Name, ControllerName: c.ControllerName}
}

// foreignGatewayClassRefusal is the stop for a cluster whose Accepted classes
// belong to implementations box does not support. It names each class and its
// controller, states the rule, and gives the way out; on k3s it also names
// --disable=traefik, which is the fix for the implementation k3s bundles.
func foreignGatewayClassRefusal(accepted []GatewayClass, dist Distribution) string {
	subject := fmt.Sprintf("GatewayClass %s is Accepted", classDescription(accepted[0]))
	if len(accepted) > 1 {
		subject = fmt.Sprintf("GatewayClasses %s are Accepted", classDescriptions(accepted))
	}
	return fmt.Sprintf("%s, but box supports only Envoy Gateway (controller %s) and puts no routes on another implementation. %s",
		subject, EnvoyGatewayController, foreignRemedy(dist, accepted))
}

// foreignRemedy is the way out of a cluster whose classes all belong to
// another implementation. On k3s with its bundled Traefik the cause is k3s's
// own default, so the k3s flag is the whole answer. Otherwise there are two
// ways out, and the sentence says what box does after each: with the other
// implementation gone box installs Envoy Gateway itself (and creates class
// "eg"); with Envoy Gateway installed beside it, box uses Envoy's class and
// leaves the other implementation alone.
func foreignRemedy(dist Distribution, classes []GatewayClass) string {
	if hint := k3sTraefikHint(dist, classes); hint != "" {
		return strings.TrimSpace(hint)
	}
	return "Remove that implementation and run box bootstrap again, and box installs Envoy Gateway itself; or install Envoy Gateway beside it, and box uses its class."
}

// k3sTraefikHint names k3s's --disable=traefik when the refusal is about the
// Traefik GatewayClass k3s ships. k3s's bundled Traefik is the usual reason a
// fresh k3s node has an Accepted class box cannot use, and no box-side setting
// can remove it.
func k3sTraefikHint(dist Distribution, classes []GatewayClass) string {
	if dist != DistK3s {
		return ""
	}
	for _, c := range classes {
		if c.ControllerName == traefikGatewayController {
			return " k3s bundles this Traefik: start k3s with `--disable=traefik` to remove it, then run box bootstrap again."
		}
	}
	return ""
}

func isEnvoyClass(c GatewayClass) bool {
	return c.ControllerName == EnvoyGatewayController
}

// hasEnvoyClass reports whether any class, accepted or not, belongs to Envoy
// Gateway's controller. A cluster with none of them has no class box could use
// even after the controller accepted it.
func hasEnvoyClass(classes []GatewayClass) bool {
	for _, c := range classes {
		if isEnvoyClass(c) {
			return true
		}
	}
	return false
}

// envoyClasses is the Accepted classes owned by Envoy Gateway's controller, by
// name. Every other class is not a class box considers, whatever it publishes.
func envoyClasses(classes []GatewayClass) []GatewayClass {
	out := make([]GatewayClass, 0, len(classes))
	for _, c := range classes {
		if c.Accepted && isEnvoyClass(c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func controllerOr(c GatewayClass) string {
	if c.ControllerName == "" {
		return "unknown"
	}
	return c.ControllerName
}

func acceptedClasses(classes []GatewayClass) []GatewayClass {
	out := make([]GatewayClass, 0, len(classes))
	for _, c := range classes {
		if c.Accepted {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func names(classes []GatewayClass) string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

// classDescription names one class with its controller, quoted so a refusal
// reads as prose: `"nginx" (controller gateway.nginx.org/... )`.
func classDescription(c GatewayClass) string {
	return fmt.Sprintf("%q (controller %s)", c.Name, controllerOr(c))
}

func classDescriptions(classes []GatewayClass) string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, classDescription(c))
	}
	return strings.Join(out, ", ")
}

// describeClasses names each class with its controller and, when it is not
// Accepted, the reason the controller gave.
func describeClasses(classes []GatewayClass) string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		line := c.Name
		if c.ControllerName != "" {
			line += " (" + c.ControllerName + ")"
		}
		if !c.Accepted {
			line += ": not Accepted: " + acceptedReason(c)
		}
		out = append(out, line)
	}
	return strings.Join(out, "; ")
}

func acceptedReason(c GatewayClass) string {
	if c.AcceptedReason == "" {
		return "no reason given"
	}
	return c.AcceptedReason
}
