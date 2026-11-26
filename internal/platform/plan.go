package platform

import (
	"strings"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
)

// planComponent is one piece of the platform layer as the plan introduces it: its name
// and a short phrase for what it does. A component's step stays where BuildPlan put it:
// the grouping is the printed plan's, never the application order's.
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

// planComponents is the order the plan introduces them: certificates, the front door,
// its address -- the order a person explains them in. No step, not printed.
var planComponents = []planComponent{
	{groupCertManager, "cert-manager", "certificates"},
	{groupGateway, "Envoy Gateway", "the front door"},
	{groupLoadBalancer, "MetalLB", "the gateway's address"},
}

// planGroupOf is the component a step belongs under, or "" for its own line. The
// admission policy is box's own line: tagged Gateway API because that is what it
// polices, not what it configures.
func planGroupOf(s Step) string {
	switch s.Kind {
	case StepGatewayAddress, StepReachable:
		// Both are about the gateway: the question and the proof it answers.
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
		// Envoy Gateway is the CRDs and the controller: its release is the gateway.
		if s.Kind == StepRelease {
			return groupGateway
		}
	}
	return ""
}

// planText is what the plan calls a step: Plan when What names objects for progress.
func planText(s Step) string {
	if s.Plan != "" {
		return s.Plan
	}
	return s.What
}

// componentLine is a component's own line: what it is, its version, what this run does
// to it. The phrase for what it is belongs to the log -- a column of names and versions
// is what a person scans (rule 13).
func componentLine(c planComponent, s Step) say.Item {
	base := c.name + " " + s.Release.Version
	switch {
	case s.Foreign:
		// Only a conditional foreign release reaches the plan; the unconditional one
		// is refused in resolve.
		text := base
		if s.Unless != "" {
			text += ", " + string(s.Unless)
		}
		return say.Item{Mark: say.Question, Text: text + " (a release box did not install is already there, and box will not upgrade it)"}
	case AtPin(s.InstalledVersion, s.Release.Version):
		return say.Item{Mark: say.Verify, Text: base}
	case s.Unless != "":
		// A conditional release is never an upgrade: resolve() drops the condition as
		// soon as the release is there.
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

// usedLine is a component box found and leaves alone: the operator's implementation is
// what the sub-steps go through.
func usedLine(c planComponent) say.Item {
	return say.Item{Mark: say.Verify, Text: c.name + " (already here, left alone)"}
}

// planGroup is the component being built and its sub-steps; parent is nil for one box
// uses.
type planGroup struct {
	component planComponent
	parent    *say.Item
	children  []say.Item
	// unless is the condition the component's own line carries, so a sub-step does not
	// repeat it.
	unless Condition
	// gateway is the index of the child writing the gateway, or -1, and reachable the
	// HTTP check proving it: one line promises the Gateway and its proof.
	gateway   int
	reachable say.Item
}

// planBuilder groups a plan's steps as the printed plan reads, over one plan.
type planBuilder struct {
	groups map[string]*planGroup
	// loose is the plan's own lines (the admission policy), under nothing.
	loose []say.Item
}

// planItems is the plan as printing sees it, warnings last. Pure, so a plan's shape is
// readable in a test without a cluster or a terminal.
func planItems(plan Plan) []say.Item {
	b := planBuilder{groups: make(map[string]*planGroup, len(planComponents))}
	for _, s := range plan.Steps {
		b.add(s)
	}
	return b.items(plan.Warnings, plan.NotHandled)
}

// printPlan is the plan's own printer: one line for the installs, one per open
// question, warnings last. A component's sub-steps are the log's (rule 5), so the
// screen says cert-manager 1.21.3 rather than the four kinds that carry it.
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

// PrintPlan is the printer for a flow that shows the plan and asks its question later:
// a guided fresh install shows platform and seedbox in one plan, printed where the
// questions are. Detail lines go to the log either way.
func PrintPlan(r say.Reporter, plan Plan) { printPlan(r, plan) }

// summaryItems is the plan as the screen shows it: installs on one line, verifies on
// another, each conditional install on its own (it is a question), then the warnings.
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

// detailLines is the log's plumbing: each component's line and its sub-steps, in
// application order.
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

// Items is the detailed plan, for a flow that shows it inside its own confirm.
func (p Plan) Items() []say.Item { return planItems(p) }

// Summary is the plan as bootstrap prints it after its confirm, and as a migration's
// confirm shows it, so the one question reads like the run that follows.
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

// add places one step: a component's line, its sub-step, or a plan line.
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
		// A question the plan cannot answer yet: box finds out after the Gateway is
		// applied, and the steps it decides follow it.
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

// conditionMark is a sub-step's mark and words: `?` alone under a component whose line
// already carries the condition, elsewhere with the words.
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

// items finishes the plan: components in narrative order with their sub-steps, the
// plan's own lines, the warnings, then what box will not handle.
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
