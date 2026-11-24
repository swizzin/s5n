package platform

import (
	"strings"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
)

// planComponent is one piece of the platform layer as the plan introduces it:
// what box calls it, and what it is for. The phrase is a short label for the
// job it does for this box, in parentheses after the version -- enough for a
// person to place it, while the sub-steps under it name the Kubernetes kinds. A component's step stays where BuildPlan put it: the grouping is
// the printed plan's, never the application order's.
type planComponent struct {
	key    string
	name   string
	phrase string
}

const (
	groupCertManager  = "cert-manager"
	groupGateway      = "gateway"
	groupLoadBalancer = "loadbalancer"
)

// planComponents is the order the plan introduces them: what issues the
// certificates, what every app is reached through, and what gives it an
// address -- the order a person explains them in. A component with no step in
// the plan is not printed at all.
var planComponents = []planComponent{
	{groupCertManager, "cert-manager", "certificates"},
	{groupGateway, "Envoy Gateway", "the front door"},
	{groupLoadBalancer, "MetalLB", "the gateway's address"},
}

// planGroupOf is the component a step belongs under, or "" for a step that
// stands on its own. The admission policy is box's own line: it is tagged with
// the Gateway API capability because that is what it polices, not because it
// configures Envoy Gateway.
func planGroupOf(s Step) string {
	switch s.Kind {
	case StepGatewayAddress, StepReachable:
		// Both are about the gateway: the question asks whether it will get an
		// address without box's MetalLB, and the last step proves it answers.
		return groupGateway
	}
	switch s.Component {
	case bootstrap.ComponentCertManager:
		return groupCertManager
	case bootstrap.ComponentGateway:
		return groupGateway
	case bootstrap.ComponentLoadBalancer:
		return groupLoadBalancer
	case bootstrap.ComponentGatewayAPI:
		// Envoy Gateway is both the Gateway API CRDs and the controller, so
		// its one release is the gateway's own line.
		if s.Kind == StepRelease {
			return groupGateway
		}
	}
	return ""
}

// planText is what the plan calls a step. A step whose What names objects -- for
// the progress and done lines, which are where exact names belong -- says what
// it does in Plan instead.
func planText(s Step) string {
	if s.Plan != "" {
		return s.Plan
	}
	return s.What
}

// componentLine is a component's own line: what it is, its version, and what
// this run does to it. The phrase that says what the component is for belongs to
// the log: on the screen a column of names and versions is what a person scans,
// and three phrases after three versions is the paragraph rule 13 is about.
func componentLine(c planComponent, s Step) say.Item {
	base := c.name + " " + s.Release.Version
	switch {
	case s.Foreign:
		// Only a conditional foreign release reaches the plan: the
		// unconditional one is refused in resolve.
		text := base
		if s.Unless != "" {
			text += ", " + string(s.Unless)
		}
		return say.Item{Mark: say.Question, Text: text + " (a release box did not install is already there, and box will not upgrade it)"}
	case AtPin(s.InstalledVersion, s.Release.Version):
		return say.Item{Mark: say.Verify, Text: base}
	case s.Unless != "":
		// A conditional release is never an upgrade: resolve() drops the
		// condition as soon as the release is there, so `?` never has to
		// carry an install-or-upgrade mark.
		return say.Item{Mark: say.Question, Text: base + ", " + string(s.Unless)}
	}
	if s.InstalledVersion == "" {
		return say.Item{Mark: say.Install, Text: base}
	}
	text := base
	if why := notAtPin(s.InstalledVersion, s.Release.Version); why != "" {
		text += "; " + why
	}
	return say.Item{Mark: say.Change, Text: text}
}

// usedLine is a component box found and leaves alone. There is no release step
// to print its version from, and saying so is the point: the operator's
// implementation is what the sub-steps below go through.
func usedLine(c planComponent) say.Item {
	return say.Item{Mark: say.Verify, Text: c.name + " (already here, left alone)"}
}

// planGroup is the component being built and the sub-steps that belong to it.
// parent is nil for a component box uses: nothing in the plan installs it.
type planGroup struct {
	component planComponent
	parent    *say.Item
	children  []say.Item
	// unless is the condition the component's own line carries, so a sub-step
	// under a conditional component does not repeat it.
	unless Condition
	// gateway is the index of the child that writes the gateway, or -1, and
	// reachable is the HTTP check that proves that same object: one line, so
	// the plan promises the Gateway and its proof together.
	gateway   int
	reachable say.Item
}

// planBuilder groups a plan's steps the way the printed plan reads. It holds no
// state beyond one plan.
type planBuilder struct {
	groups map[string]*planGroup
	// loose is the plan's own lines -- the admission policy -- which belong
	// under nothing.
	loose []say.Item
}

// planItems is the plan as printing sees it: each component with the sub-steps
// that configure it, the lines that stand on their own, and the warnings last.
// It is pure, so what a plan looks like can be read in a test without a
// cluster, a reporter or a terminal.
func planItems(plan Plan) []say.Item {
	b := planBuilder{groups: make(map[string]*planGroup, len(planComponents))}
	for _, s := range plan.Steps {
		b.add(s)
	}
	return b.items(plan.Warnings, plan.NotHandled)
}

// printPlan is the plan's own printer: what this run is about to do, one line
// for the installs, one for each thing the plan cannot decide yet, and the
// warnings last. The sub-steps that make up a component are the log's (rule 5),
// so the screen says cert-manager 1.21.3 rather than the four Kubernetes kinds
// that carry it.
func printPlan(r say.Reporter, plan Plan) {
	items := summaryItems(plan)
	if len(items) == 0 {
		return
	}
	r.Plan("", items)
	for _, line := range detailLines(plan) {
		r.Detail(line)
	}
}

// PrintPlan is the plan's printer for a flow that shows it itself and asks its
// one question later: a guided fresh install shows the platform and the seedbox
// in one plan, so the plan is printed where the questions are rather than by
// Decision.Apply. The detail lines go to the run log either way.
func PrintPlan(r say.Reporter, plan Plan) { printPlan(r, plan) }

// summaryItems is the plan as the screen shows it: everything this run installs
// on one line, everything it only verifies on another, each conditional install
// on its own (it is a question, and the answer changes what happens), then what
// box saw and will not change. It is pure, so the shape can be read in a test
// without a cluster or a terminal.
func summaryItems(plan Plan) []say.Item {
	var installs, changes, verifies, questions, warnings []string
	for _, it := range planItems(plan) {
		switch it.Mark {
		case say.Install:
			installs = append(installs, it.Text)
		case say.Change:
			changes = append(changes, it.Text)
		case say.Verify:
			verifies = append(verifies, it.Text)
		case say.Question:
			questions = append(questions, it.Text)
		default:
			warnings = append(warnings, it.Text)
		}
	}
	var items []say.Item
	if len(installs) > 0 {
		items = append(items, say.Item{Mark: say.Install, Text: strings.Join(installs, " · ")})
	}
	if len(changes) > 0 {
		items = append(items, say.Item{Mark: say.Change, Text: strings.Join(changes, " · ")})
	}
	if len(verifies) > 0 {
		items = append(items, say.Item{Mark: say.Verify, Text: strings.Join(verifies, " · ")})
	}
	for _, text := range questions {
		items = append(items, say.Item{Mark: say.Question, Text: text})
	}
	for _, text := range warnings {
		items = append(items, say.Item{Mark: say.Warning, Text: text})
	}
	return items
}

// detailLines is the plan's plumbing, for the log: each component's own line and
// the sub-steps under it, in the order they are applied.
func detailLines(plan Plan) []string {
	var lines []string
	for _, it := range planItems(plan) {
		lines = append(lines, "plan: "+string(it.Mark)+" "+it.Text)
		for _, child := range it.Children {
			lines = append(lines, "plan: "+string(child.Mark)+" "+child.Text)
		}
	}
	return lines
}

// Items is the plan as a reader sees it, for a flow that shows the platform
// plan inside its own confirm instead of letting this package print it
// (Decision.CallerShowsPlan). It is the detailed plan: one line per component
// and the sub-steps under it.
func (p Plan) Items() []say.Item { return planItems(p) }

// Summary is the plan as bootstrap prints it after its confirm: what this run
// installs on one line, each conditional install on its own, and what box saw
// and will not change. A migration's confirm shows the same summary, so the one
// question a person answers reads like the run that follows it.
func (p Plan) Summary() []say.Item { return summaryItems(p) }

// group is the component's builder, made on first use.
func (b *planBuilder) group(key string) *planGroup {
	if g, ok := b.groups[key]; ok {
		return g
	}
	g := &planGroup{gateway: -1}
	for _, c := range planComponents {
		if c.key == key {
			g.component = c
			break
		}
	}
	b.groups[key] = g
	return g
}

// add places one step: a component's own line, a sub-step of the component it
// configures, or a line of the plan itself.
func (b *planBuilder) add(s Step) {
	key := planGroupOf(s)
	if key == "" {
		b.loose = append(b.loose, say.Item{Mark: say.Install, Text: planText(s)})
		return
	}
	g := b.group(key)
	switch s.Kind {
	case StepRelease:
		it := componentLine(g.component, s)
		g.parent = &it
		g.unless = s.Unless
	case StepGatewayAddress:
		// A question the plan cannot answer yet: box finds out after the
		// Gateway is applied, and the steps it decides follow it.
		g.children = append(g.children, say.Item{Mark: say.Question, Text: planText(s)})
	case StepReachable:
		mark, text := conditionMark(s, g)
		g.reachable = say.Item{Mark: mark, Text: text}
	default:
		mark, text := conditionMark(s, g)
		g.children = append(g.children, say.Item{Mark: mark, Text: text})
		if s.Component == bootstrap.ComponentGateway {
			g.gateway = len(g.children) - 1
		}
	}
}

// conditionMark is a sub-step's mark and words. A conditional step under a
// component whose own line already carries the condition is `?` without saying
// it twice; anywhere else the condition travels with the step.
func conditionMark(s Step, g *planGroup) (say.Mark, string) {
	if s.Unless == "" {
		return say.Install, planText(s)
	}
	text := planText(s)
	if s.Unless != g.unless {
		text += ", " + string(s.Unless)
	}
	return say.Question, text
}

// items finishes the plan: the components in their narrative order, each with
// the sub-steps that configure it, then the lines that stand on their own, then
// what box saw and will not change, then what box will not handle -- the two
// things a person may have to act on.
func (b *planBuilder) items(warnings, notHandled []string) []say.Item {
	var items []say.Item
	for _, c := range planComponents {
		g, ok := b.groups[c.key]
		if !ok {
			continue
		}
		parent := usedLine(c)
		if g.parent != nil {
			parent = *g.parent
		}
		children := g.children
		if g.reachable.Text != "" {
			if g.gateway >= 0 {
				children[g.gateway].Text += ", " + g.reachable.Text
			} else {
				children = append(children, g.reachable)
			}
		}
		parent.Children = children
		items = append(items, parent)
	}
	items = append(items, b.loose...)
	for _, line := range warnings {
		items = append(items, say.Item{Mark: say.Warning, Text: line})
	}
	for _, line := range notHandled {
		items = append(items, say.Item{Mark: say.Warning, Text: line})
	}
	return items
}
