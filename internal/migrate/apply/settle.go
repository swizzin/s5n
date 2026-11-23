package apply

// settle.go is the read-only pass that runs before an install: every rewriter's
// Check over a copy of every app's config, and the plan that comes out of it.
//
// Apply's first write is on a host that has already been prepared, and its
// refusal is the first thing that ever reads the addresses and paths an app
// stores (docs/box.md, "Only local addresses are rewritten"). So the gaps a
// migration meets are met at apply, after the install, which is too late to ask
// about and too late to be a surprise. Settle runs the same checks earlier, on
// the same bytes, where nothing is written and nothing can break: every value
// the plan does not settle becomes a decision in the plan (a default rule with a
// reason) and a line in the list this function returns, so the operator sees
// what happens to it before the downtime rather than as a refusal after it.
//
// A default is a decision, not a guess. Where the field a value was found in
// says which app it can only mean, the rule points there (a PeerBanHelper
// downloader of type qBittorrent is the box's qBittorrent). Everywhere else the
// only safe rewrite is none at all, so the rule is an identity: the path or
// address is left exactly as it is, and the reason says so. A default never
// suggests a rewrite that would write a broken config.
//
// What Settle cannot check is recorded, not failed: the one app whose database
// only its own image may read is "checked at apply" when this run has no runner.
// What a rewriter left on purpose (a disabled client, a client of a type box
// does not run) is passed through with the rewriter's own reason.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/work"
)

// The three decisions the called-out list groups by.
const (
	// actionLeave is a value box leaves exactly as it is.
	actionLeave = "left as it is"
	// actionRewrite is a value box rewrites to one app; the app's name follows.
	actionRewrite = "rewritten to "
	// actionAtApply is a check that only apply can make.
	actionAtApply = "checked at apply"
)

// SettleInput is everything Settle needs that the plan does not hold.
type SettleInput struct {
	// Catalog is this chart's app names: how an instance name becomes the
	// rewriter that reads it (CatalogApp).
	Catalog []string
	// Configs answers the host path one instance's source config is at. nil
	// reads the plan's own app.Config, which is right when the host the plan
	// describes is this host.
	Configs func(instance string) string
	// Targets is where every instance answers on the box (Targets). Every app
	// in the plan needs one: a rewriter writes every address it settles, so an
	// endpoint this run does not know is a check it cannot make.
	Targets map[string]rewrite.Endpoint
	// Run is the runner a rewriter needs for a format only the app's own image
	// may write. nil records those apps as checked at apply.
	Run rewrite.Runner
	// Work is the directory the private copies are made in: the run's own working
	// directory beside its snapshot (internal/migrate/work), never the system's
	// temporary directory. A config tree is where a real source kept 8.5 GiB, and
	// /tmp is a tmpfs on a Fedora or an Arch host.
	Work string
}

// Settled is what Settle called out, grouped by decision: one line per host or
// path, and beneath it every place that holds it. It is data, so a caller can
// print it, count it or keep it in report.txt; Lines is the text a report
// carries.
type Settled []SettledGroup

// SettledGroup is one decision, with every value that decision was made about.
type SettledGroup struct {
	// Action is what box will do: "left as it is", "rewritten to <app>", or
	// "checked at apply".
	Action string
	Items  []SettledItem
}

// SettledItem is one host, path or app the decision is about.
type SettledItem struct {
	// Value is what the decision is about: an address as stored, a path, or
	// the instance name of a check.
	Value string
	// Why is the reason, one line, without the action in front of it.
	Why string
	// Holds is where the value is stored: one entry per app, file and place.
	Holds []SettledHold
	// Host and Port are the address a decision is about, as stored: what a
	// guided run asks about and what the plan's rule matches. Empty for a path
	// and for a check, which are not addresses.
	Host string
	Port int
	// App is what a rewrite points the value at, and empty for a leave and a
	// check.
	App string
	// Default says box chose this decision. A guided run clears it when the
	// operator answers, so the summary of defaults taken names only what
	// nobody looked at.
	Default bool
}

// SettledHold is one place one value is stored.
type SettledHold struct {
	App   string
	File  string
	Where string
}

func (h SettledHold) String() string {
	switch {
	case h.File != "" && h.Where != "":
		return h.App + ": " + h.File + " " + h.Where
	case h.File != "":
		return h.App + ": " + h.File
	case h.Where != "":
		return h.App + ": " + h.Where
	}
	return h.App
}

// Empty reports whether Settle called nothing out.
func (s Settled) Empty() bool { return len(s) == 0 }

// Defaults is the decisions box took without the operator answering: what an
// unattended run has to name, and what the end of a guided run still lists.
func (s Settled) Defaults() Settled {
	var out Settled
	for _, g := range s {
		var items []SettledItem
		for _, it := range g.Items {
			if it.Default {
				items = append(items, it)
			}
		}
		if len(items) > 0 {
			out = append(out, SettledGroup{Action: g.Action, Items: items})
		}
	}
	return out
}

// Lines is the called-out list as text: one header per decision, then one line
// per host or path with its reason, then the places that hold it.
func (s Settled) Lines() []string {
	var out []string
	for _, g := range s {
		if len(g.Items) == 0 {
			continue
		}
		out = append(out, g.Action+":")
		for _, it := range g.Items {
			line := "  " + it.Value
			if it.Why != "" {
				line += " (" + it.Why + ")"
			}
			out = append(out, line)
			for _, h := range it.Holds {
				out = append(out, "    "+h.String())
			}
		}
	}
	return out
}

// add files one called-out value under its decision, merging into a line that
// is already there so the same address in three apps reads once.
func (s *Settled) add(action string, item SettledItem) {
	for i := range *s {
		g := &(*s)[i]
		if g.Action != action {
			continue
		}
		for j := range g.Items {
			if g.Items[j].Value != item.Value {
				continue
			}
			g.Items[j].Holds = append(g.Items[j].Holds, item.Holds...)
			return
		}
		g.Items = append(g.Items, item)
		return
	}
	*s = append(*s, SettledGroup{Action: action, Items: []SettledItem{item}})
}

// sort orders the groups by decision and the values under them, so two runs of
// the same plan read the same, and orders each item's holds the same way.
func (s Settled) sort() {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Action < s[j].Action })
	for _, g := range s {
		sort.SliceStable(g.Items, func(i, j int) bool { return g.Items[i].Value < g.Items[j].Value })
		for _, it := range g.Items {
			sort.SliceStable(it.Holds, func(i, j int) bool { return it.Holds[i].String() < it.Holds[j].String() })
		}
	}
}

// Settle runs every rewriter's Check over a private copy of every app's config,
// and returns the plan with everything the checks did not settle written into it
// as a default, plus the list of what it called out.
//
// The plan handed in is never modified: the one returned is a copy. Settle writes
// nothing outside its own working directory -- which is the caller's, beside the
// snapshot the run is reading -- so a run that stops here leaves the source
// exactly as it was.
func Settle(ctx context.Context, p *plan.Plan, in SettleInput) (*plan.Plan, Settled, error) {
	if p == nil {
		return nil, nil, errors.New("settle: no plan: there is nothing to settle")
	}
	dir, done, err := work.New(in.Work)
	if err != nil {
		return nil, nil, fmt.Errorf("settle: %w", err)
	}
	defer done()
	s := &settler{in: in, plan: copyPlan(p), dir: dir}
	if err := s.checkAll(ctx); err != nil {
		return nil, nil, err
	}
	if err := s.settleAddresses(); err != nil {
		return nil, nil, err
	}
	if err := s.settlePaths(); err != nil {
		return nil, nil, err
	}
	s.settleExisting()
	s.settled.sort()
	return s.plan, s.settled, nil
}

// settler is one Settle run: the plan it is building, the private copies of the
// configs, and the reports they produced.
type settler struct {
	in      SettleInput
	plan    *plan.Plan
	dir     string
	reports []rewrite.Report
	settled Settled
}

// copyPlan is the plan Settle hands back. The apps' path tables and the host
// rules are copied so a rule can be added to one app without touching another,
// and the caller's plan keeps exactly what it was read from.
func copyPlan(p *plan.Plan) *plan.Plan {
	out := *p
	out.Apps = make(map[string]plan.App, len(p.Apps))
	for name, app := range p.Apps {
		app.Paths = append([]plan.PathRule(nil), app.Paths...)
		out.Apps[name] = app
	}
	out.Hosts = append([]plan.HostRule(nil), p.Hosts...)
	out.Outside = append([]string(nil), p.Outside...)
	return &out
}

// checkAll copies each app's config and runs its Check. Every check runs against
// the plan as it was handed in, so each one sees the same plan and every place
// an address is stored is reported, whichever app is checked first.
func (s *settler) checkAll(ctx context.Context) error {
	runs := CatalogRuns(s.plan, s.in.Catalog)
	for _, name := range planApps(s.plan) {
		if err := ctx.Err(); err != nil {
			return err
		}
		catalog := CatalogApp(name, s.in.Catalog)
		if catalog == "" {
			return fmt.Errorf("settle: apps.%s: no app in this chart's catalog is named that (the catalog has %s)",
				name, strings.Join(s.in.Catalog, ", "))
		}
		rw, ok := rewrite.For(catalog)
		if !ok {
			return fmt.Errorf("settle: apps.%s: this box has no rewriter for %s, so its config cannot be checked (it has %s)",
				name, catalog, strings.Join(rewrite.Apps(), ", "))
		}
		if _, ok := s.in.Targets[name]; !ok {
			return fmt.Errorf("settle: apps.%s: the release has no endpoint for it, so the addresses it stores cannot be checked", name)
		}
		if NeedsRunner(catalog) && s.in.Run == nil {
			s.settled.add(actionAtApply, SettledItem{
				Value: name, Default: true,
				Why: "its database is only readable through its own image, and this run has no way to start one",
			})
			continue
		}
		src := s.plan.Apps[name].Config
		if s.in.Configs != nil {
			src = s.in.Configs(name)
		}
		if !isDir(src) {
			// A read with no source tree behind it (--from-dir, or a survey on
			// a host that does not hold the configs) has no bytes to check.
			// That is recorded, not failed: it is what apply checks, and a
			// survey that refused to be written would be a read with nothing
			// to show for it.
			s.settled.add(actionAtApply, SettledItem{
				Value: name, Default: true,
				Why: fmt.Sprintf("its config is not at %s on this host, so the addresses and paths it stores are checked at apply", src),
			})
			continue
		}
		dir := filepath.Join(s.dir, name)
		if err := s.stageForCheck(catalog, dir, src); err != nil {
			return fmt.Errorf("settle %s from %s: %w", name, src, err)
		}
		rep, err := rw.Check(ctx, rewrite.Input{App: name, Dir: dir, Plan: s.plan, Targets: s.in.Targets, Runs: runs, Run: s.in.Run})
		if err != nil {
			return fmt.Errorf("settle %s: %w", name, err)
		}
		rep.App = name
		s.reports = append(s.reports, rep)
	}
	return nil
}

// stageForCheck copies what one app's check reads into dst: its whole config, or
// -- for an app whose config is a library as well as its state -- the state alone,
// with the directories above it. A Plex config directory is tens of gigabytes of
// artwork no check opens, and copying all of it to query two databases is how a
// read of a source filled a host's disk. The copy is taken as a reader takes it
// (stageTrimmed's rule: kernel files left out, links carried or skipped by
// internal/linktree), because the source may still be running.
func (s *settler) stageForCheck(app, dst, src string) error {
	if app == appPlex {
		_, err := stageTrimmed(dst, src, plex.Snapshot(false), os.Geteuid(), os.Getegid())
		return err
	}
	_, err := copyForCheck(dst, src, os.Geteuid(), os.Getegid())
	return err
}

// settleAddresses writes a host rule for every address no rule settles: a
// rewrite where a rewriter proposed the app the field can only mean, a leave
// everywhere else. Each one is called out either way.
func (s *settler) settleAddresses() error {
	type group struct {
		host  string
		port  int
		holds []SettledHold
		apps  []rewrite.Default
	}
	groups := map[string]*group{}
	var order []string
	for _, rep := range s.reports {
		for _, a := range rep.Unclassified {
			key := addressKey(a.Host, a.Port)
			g, ok := groups[key]
			if !ok {
				g = &group{host: a.Host, port: a.Port}
				groups[key] = g
				order = append(order, key)
			}
			g.holds = append(g.holds, SettledHold{App: rep.App, File: a.File, Where: a.Where})
		}
	}
	// A proposal is about an address some app reported, and the app that
	// proposed it may not be the app that reported it, so they are matched once
	// every address is known.
	for _, rep := range s.reports {
		for _, d := range rep.Defaults {
			if g, ok := groups[addressKey(d.Host, d.Port)]; ok {
				g.apps = append(g.apps, d)
			}
		}
	}
	// The rewriters' own left-on-purpose values are decisions too, passed on
	// with their reasons.
	for _, rep := range s.reports {
		for _, l := range rep.Left {
			s.settled.add(actionLeave, SettledItem{
				Value: l.What, Why: l.Why, Default: true,
				Holds: []SettledHold{{App: rep.App, File: l.File, Where: l.Where}},
			})
		}
	}
	for _, key := range order {
		g := groups[key]
		text := addressText(g.host, g.port)
		if loopback(g.host) {
			// A localhost address is settled by the apps' ports and by nothing
			// else, and the plan's rules may not name one (plan.checkHosts).
			return fmt.Errorf("settle: %s is a localhost address, which only the apps' ports can settle: this plan has no app whose port it is (or two), so the plan's ports have to say which one %d means",
				text, g.port)
		}
		if app, why, ok := proposal(g.apps); ok {
			// A proposal names an app, and a rule may only name one the plan
			// has: a rewriter that proposed something else is a proposal this
			// plan cannot carry, so it is left rather than written as a file
			// the plan validation refuses.
			if _, ok := s.plan.Apps[app]; ok {
				s.plan.Hosts = append(s.plan.Hosts, plan.HostRule{
					Host: g.host, Port: g.port, App: app, Default: true,
					Why: actionRewrite + app + ": " + why,
				})
				s.settled.add(actionRewrite+app, SettledItem{
					Value: text, Why: why, Holds: g.holds,
					Host: g.host, Port: g.port, App: app, Default: true,
				})
				continue
			}
		}
		why := "box cannot tell what " + text + " is, so it is left as stored"
		if len(g.apps) > 1 {
			why = "the apps that hold " + text + " do not agree about which app it is, so it is left as stored"
		}
		s.plan.Hosts = append(s.plan.Hosts, plan.HostRule{
			Host: g.host, Port: g.port, Leave: true, Default: true,
			Why: actionLeave + ": " + why,
		})
		s.settled.add(actionLeave, SettledItem{
			Value: text, Why: why, Holds: g.holds,
			Host: g.host, Port: g.port, Default: true,
		})
	}
	return nil
}

// proposal is the one app every proposal for an address names. Two apps
// proposing different ones is a disagreement, and a disagreement is a leave.
func proposal(defaults []rewrite.Default) (string, string, bool) {
	app, why := "", ""
	for _, d := range defaults {
		if app == "" {
			app, why = d.App, d.Why
			continue
		}
		if d.App != app {
			return "", "", false
		}
	}
	return app, why, app != ""
}

// settlePaths writes an identity rule for every path no rule covers, in the
// table of the app whose filesystem it is on: leaving it exactly as it is is the
// only rewrite that cannot make that app worse, and the rule says so.
func (s *settler) settlePaths() error {
	// One rule per app and directory: two keys can hold the same path, and two
	// rules for one From is a plan the validation refuses. Every key that holds
	// it is still listed, because those are the places to look at.
	ruled := map[string]bool{}
	for _, rep := range s.reports {
		for _, u := range rep.Uncovered {
			dir := path.Clean(u.Path)
			key := u.App + "\x00" + dir
			why := "box cannot tell what " + dir + " is on this box, so it is left as stored"
			if !ruled[key] {
				ruled[key] = true
				app, ok := s.plan.Apps[u.App]
				if !ok {
					return fmt.Errorf("settle: %s: no app in the plan is named that, so the rule its path needs has nowhere to go", u.App)
				}
				app.Paths = append(app.Paths, plan.PathRule{
					From: dir, To: dir, Default: true,
					Why: actionLeave + ": " + why,
				})
				s.plan.Apps[u.App] = app
			}
			s.settled.add(actionLeave, SettledItem{
				Value: dir, Why: why, Default: true,
				Holds: []SettledHold{{App: u.App, File: u.File, Where: u.Key}},
			})
		}
	}
	return nil
}

// settleExisting calls out the defaults the plan already carried -- an address
// discovery proposed for one app, a path it left where it was -- so a read
// reports, and a guided run asks about, every decision the plan was written
// with, not only the ones this pass made. A default is never silent, whichever
// step took it.
//
// An item this pass already added merges into one line: the value, its reason
// and every place that holds it read once.
func (s *settler) settleExisting() {
	for _, r := range s.plan.Hosts {
		if !r.Default || r.Host == "" {
			continue
		}
		item := SettledItem{
			Value: addressText(r.Host, r.Port), Why: ruleReason(r.Why),
			Host: r.Host, Port: r.Port, Default: true,
		}
		if r.Leave {
			s.settled.add(actionLeave, item)
			continue
		}
		item.App = r.App
		s.settled.add(actionRewrite+r.App, item)
	}
	for _, name := range planApps(s.plan) {
		for _, r := range s.plan.Apps[name].Paths {
			if !r.Default {
				continue
			}
			s.settled.add(actionLeave, SettledItem{
				Value: r.From, Why: ruleReason(r.Why), Default: true,
			})
		}
	}
}

// ruleReason is a rule's why without the action a rule writes in front of it:
// discovery and Settle both begin with the decision, and the called-out list
// already says it in the group's header.
func ruleReason(why string) string {
	for _, action := range []string{actionLeave + ": ", "rewrite to "} {
		if !strings.HasPrefix(why, action) {
			continue
		}
		rest := strings.TrimPrefix(why, action)
		if action == "rewrite to " {
			if _, reason, ok := strings.Cut(rest, ": "); ok {
				return reason
			}
			continue
		}
		return rest
	}
	return why
}

// addressKey is how two reports' addresses are known to be the same one: DNS
// names are case-insensitive, and the port is part of the address.
func addressKey(host string, port int) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), ".")) + ":" + strconv.Itoa(port)
}

// addressText is a host and port as one line an operator reads.
func addressText(host string, port int) string {
	if port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

// loopback reports whether a stored host is a localhost form, which the plan
// settles by port and never by a rule.
func loopback(host string) bool {
	h := strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}
