package bootstrap

import (
	"fmt"
	"sort"
	"strings"
)

// EnvoyGatewayClass is the GatewayClass box creates when it installs Envoy Gateway:
// the only class box ever creates.
const EnvoyGatewayClass = "eg"

// EnvoyGatewayController is the controllerName of the only Gateway API
// implementation box supports: logins are checked at the edge through Envoy's
// SecurityPolicy (docs/box.md, "Logins"), with no per-app fallback, so another
// controller's class cannot serve this box.
const EnvoyGatewayController = "gateway.envoyproxy.io/gatewayclass-controller"

// traefikGatewayController is k3s's bundled Traefik: the refusal that finds it names
// k3s's --disable=traefik instead.
const traefikGatewayController = "traefik.io/gateway-controller"

// GatewayClass is one GatewayClass the probe found, reduced to what the decision
// needs: an operator's or box's statement that a controller will serve Gateways.
type GatewayClass struct {
	// Name is the class's own name: what the Gateway says.
	Name string
	// ControllerName is spec.controllerName, which decides whether box may use the
	// class at all (see EnvoyGatewayController).
	ControllerName string
	// Accepted is the Accepted condition being True.
	Accepted bool
	// AcceptedReason is the Accepted condition's message or reason when the class is
	// not Accepted: it is never used, and saying why is the point of reading it.
	AcceptedReason string
	// ManagedByBox is the app.kubernetes.io/managed-by=box label: box created the
	// class. Reported with the class; the decision does not branch on it.
	ManagedByBox bool
	// Features is status.supportedFeatures; nothing decides from it, since box
	// supports only Envoy Gateway, whose class publishes none.
	Features []string
}

// GatewayChoice is the class box's Gateway attaches to and how box arrived at it:
// it travels with the decision so the plan need not re-read the cluster.
type GatewayChoice struct {
	// Class is the GatewayClass name the Gateway will reference.
	Class string
	// ControllerName is that class's controller, for box's output.
	ControllerName string
	// BoxInstalls says this run installs Envoy Gateway and creates the class.
	BoxInstalls bool
}

// ClassChooser asks which of several Accepted Envoy classes to use: cmd/box
// implements it over setup.Prompter.Select, so it is init's own dialog, while the
// decision stays here with every other bootstrap refusal.
type ClassChooser interface {
	ChooseGatewayClass(classes []GatewayClass) (string, error)
}

// gatewayChoice resolves the GatewayClass for this run. gw is the Gateway component's
// result, where "box installs its own" and "box uses one there" are decided; dist is
// for the one refusal k3s answers for itself (see k3sTraefikHint).
//
// The order is docs/box.md's: only a class accepted by Envoy's controller counts; a
// chosen class wins (and must be Accepted); else exactly one Accepted Envoy class is
// used; several are the operator's to choose; none means box installs Envoy Gateway.
// Classes that exist but are not Accepted stop box, rather than installing Envoy
// under a controller still starting, and a foreign class beside an Accepted Envoy one
// is left alone.
func (d *Decision) gatewayChoice(opts Options, probe PlatformProbe, gw ComponentResult, dist Distribution) (GatewayChoice, string) {
	accepted := acceptedClasses(probe.Classes)
	envoy := envoyClasses(probe.Classes)

	if gw.Mode == ModeManaged {
		// managed owns the implementation and the class: box installs Envoy Gateway
		// and writes `eg`. Other classes are no conflict; the operator asked for
		// box's own.
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	if len(probe.Classes) > 0 && len(accepted) == 0 {
		if !hasEnvoyClass(probe.Classes) {
			// Nothing here is box's implementation, so waiting would not help: it
			// must be removed.
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
		// A stop or unknown action is decided elsewhere; there is no class to name.
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

// foreignGatewayClassRefusal is the stop for a cluster whose Accepted classes belong
// to implementations box does not support: each class and controller named, the rule,
// and the way out -- --disable=traefik on k3s.
func foreignGatewayClassRefusal(accepted []GatewayClass, dist Distribution) string {
	subject := fmt.Sprintf("GatewayClass %s is Accepted", classDescription(accepted[0]))
	if len(accepted) > 1 {
		subject = fmt.Sprintf("GatewayClasses %s are Accepted", classDescriptions(accepted))
	}
	return fmt.Sprintf("%s, but box supports only Envoy Gateway (controller %s) and puts no routes on another implementation. %s",
		subject, EnvoyGatewayController, foreignRemedy(dist, accepted))
}

// foreignRemedy is the way out. On k3s with bundled Traefik the k3s flag is the whole
// answer. Otherwise there are two: remove the other implementation and box installs
// Envoy Gateway itself, or install Envoy Gateway beside it and box uses its class.
func foreignRemedy(dist Distribution, classes []GatewayClass) string {
	if hint := k3sTraefikHint(dist, classes); hint != "" {
		return strings.TrimSpace(hint)
	}
	return "Remove that implementation and run box bootstrap again, and box installs Envoy Gateway itself; or install Envoy Gateway beside it, and box uses its class."
}

// k3sTraefikHint names --disable=traefik when the refusal is about the Traefik class
// k3s ships: the usual reason a fresh k3s node has an Accepted class box cannot use,
// and no box-side setting removes it.
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

// hasEnvoyClass reports whether any class, accepted or not, is Envoy Gateway's: with
// none, no class would serve box even once accepted.
func hasEnvoyClass(classes []GatewayClass) bool {
	for _, c := range classes {
		if isEnvoyClass(c) {
			return true
		}
	}
	return false
}

// envoyClasses is the Accepted classes of Envoy Gateway's controller, by name.
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

// classDescription names one class with its controller: `"nginx" (controller ...)`.
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

// describeClasses names each class with its controller and, if not Accepted, the
// reason.
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
