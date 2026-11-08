package bootstrap

import (
	"fmt"
	"sort"
	"strings"
)

// EnvoyGatewayClass is the GatewayClass box creates when box installs Envoy
// Gateway. It is the only class box ever creates: an existing implementation
// keeps its own class, and box's Gateway attaches to that one instead.
const EnvoyGatewayClass = "eg"

// GatewayClass is one GatewayClass the probe found, reduced to what the class
// decision needs. The class is the operator's (or box's) statement that a
// controller is willing to serve Gateways; box never guesses one.
type GatewayClass struct {
	// Name is the GatewayClass's own name: what the Gateway says.
	Name string
	// ControllerName is the controller that owns it, shown when box asks the
	// operator to choose between several.
	ControllerName string
	// Accepted is the GatewayClass's Accepted condition being True.
	Accepted bool
	// AcceptedReason is the Accepted condition's message (or reason when it
	// carries no message) when the class is not Accepted. A class that is not
	// Accepted is never used, and saying why is the whole point of reading it.
	AcceptedReason string
	// ManagedByBox is the app.kubernetes.io/managed-by=box label on the
	// class: box created it beside the implementation box installed, so box
	// does not have to ask a controller whether it supports what the chart
	// emits (see FeaturePathRewrite).
	ManagedByBox bool
	// Features is status.supportedFeatures as the class published it. Empty
	// means the class published no list at all, which box treats as "cannot
	// check" rather than "supports everything".
	Features []string
}

// HasFeature reports whether the class published name.
func (c GatewayClass) HasFeature(name string) bool {
	for _, f := range c.Features {
		if f == name {
			return true
		}
	}
	return false
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
	// the GatewayClass: box's own implementation does not need to be asked
	// about features.
	BoxInstalls bool
}

// ClassChooser asks which of several Accepted GatewayClasses box should use.
// cmd/box implements it over setup.Prompter.Select, so the question is the same
// dialog `box init` asks with; the decision itself stays here, where every
// other bootstrap refusal lives.
type ClassChooser interface {
	ChooseGatewayClass(classes []GatewayClass) (string, error)
}

// gatewayChoice resolves the GatewayClass for this run. gw is the Gateway
// component's result, because "box installs its own" and "box uses one that is
// there" are decided there, not here.
//
// The order is docs/box.md's decided paragraph: a chosen class wins (and must
// be Accepted); otherwise exactly one Accepted class is used; several are the
// operator's to choose; none means box installs Envoy Gateway. Classes that
// exist but are not Accepted stop box rather than letting it install Envoy
// underneath a controller that is merely still starting.
func (d *Decision) gatewayChoice(opts Options, probe PlatformProbe, gw ComponentResult) (GatewayChoice, string) {
	accepted := acceptedClasses(probe.Classes)

	if gw.Mode == ModeManaged {
		// managed owns the implementation, so it owns the class too: box
		// installs Envoy Gateway and writes `eg`. Other classes are not a
		// conflict here; the operator asked for box's own.
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: true}, ""
	}
	if len(probe.Classes) > 0 && len(accepted) == 0 {
		return GatewayChoice{}, fmt.Sprintf(
			"%d GatewayClass(es) exist but none is Accepted (%s): box will not install Envoy Gateway beside another implementation. Fix the implementation so its class is Accepted, or set platform.gateway.className (--gateway-class) to the class to use",
			len(probe.Classes), describeClasses(probe.Classes))
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
			return choiceOf(c), ""
		}
		return GatewayChoice{}, fmt.Sprintf("platform.gateway.className is %q, but this cluster has no GatewayClass by that name (accepted: %s)",
			opts.GatewayClass, names(accepted))
	}

	switch len(accepted) {
	case 0:
		// Unreachable while the Gateway component says "use"/"verify", but a
		// refusal is better than an empty class name.
		return GatewayChoice{}, "no GatewayClass is Accepted, so there is no class for box's Gateway"
	case 1:
		return choiceOf(accepted[0]), ""
	}

	if d.Prompt == nil {
		return GatewayChoice{}, fmt.Sprintf("several GatewayClasses are Accepted (%s): choose one with platform.gateway.className (--gateway-class) and run `box bootstrap` again",
			describeClasses(accepted))
	}
	chosen, err := d.Prompt.ChooseGatewayClass(accepted)
	if err != nil {
		return GatewayChoice{}, fmt.Sprintf("choosing a GatewayClass: %v", err)
	}
	for _, c := range accepted {
		if c.Name == chosen {
			return choiceOf(c), ""
		}
	}
	return GatewayChoice{}, fmt.Sprintf("the chosen GatewayClass %q is not one of the Accepted classes (%s)", chosen, names(accepted))
}

func choiceOf(c GatewayClass) GatewayChoice {
	return GatewayChoice{Class: c.Name, ControllerName: c.ControllerName}
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
