package main

// migrate_report.go builds the migration report (docs/output.md, rule 7) and the
// screen's digest. The same bytes are the snapshot's report.txt and the file
// beside it, from one builder, so they cannot disagree.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/say"
)

// Two words shared with the settle screen.
const (
	rowRoot    = "root"
	leftAsItIs = "left as it is"
)

// readReport is one read's page. failure is why a read stopped; a report never
// written still says why and what was read.
func readReport(read migrationRead, mode archive.Mode, snapshot string, taken time.Time, box string, failure error) report.Doc {
	d := report.Doc{
		Kind:     string(mode),
		Snapshot: snapshot,
		Taken:    taken,
		Box:      box,
	}
	if failure != nil {
		d.Sections = append(d.Sections, report.Section{
			Title: "plan",
			Rows:  []report.Row{{Name: "not written", Facts: []string{failure.Error()}}},
		})
	}
	if read.plan == nil {
		return d
	}
	p := read.plan
	d.Counts = readCounts(p, read)
	for _, s := range []report.Section{
		{Title: "apps", Rows: appReportRows(p)},
		{Title: "data", Rows: dataReportRows(p)},
		{Title: "paths", Rows: pathReportRows(p)},
		{Title: "addresses", Rows: addressReportRows(p)},
		{Title: "checks", Rows: checkReportRows(read.settled)},
		{Title: "notes", Rows: noteReportRows(read.report)},
		{Title: "not migrated", Rows: leftReportRows(p, read.report)},
	} {
		if len(s.Rows) > 0 {
			d.Sections = append(d.Sections, s)
		}
	}
	return d
}

// readCounts is the summary numbers.
func readCounts(p *plan.Plan, read migrationRead) []report.Count {
	rewritten, left := 0, 0
	for _, r := range p.Hosts {
		if r.Leave {
			left++
			continue
		}
		rewritten++
	}
	renames, folders := 0, 0
	if p.Data != nil {
		renames = len(p.Data.Renames())
		folders = len(p.Data.MediaRenames())
	}
	decided := 0
	for _, g := range read.settled {
		decided += len(g.Items)
	}
	before := len(p.Outside) + len(read.report.Skipped) + len(read.report.Ignored)
	return []report.Count{
		{Label: "apps", Value: strconv.Itoa(len(p.Apps))},
		{Label: "addresses", Value: fmt.Sprintf("%d rewritten · %d left as it is", rewritten, left)},
		{Label: "data", Value: fmt.Sprintf("%d renames · %d library directories", renames, folders)},
		{Label: "decisions", Value: strconv.Itoa(decided)},
		{Label: "notes", Value: strconv.Itoa(len(read.report.Notes))},
		{Label: "left", Value: fmt.Sprintf("%d workloads and mounts", before)},
	}
}

// appReportRows is one row per app: name, config, library and carry.
func appReportRows(p *plan.Plan) []report.Row {
	rows := make([]report.Row, 0, len(p.Apps))
	for _, name := range sortedKeys(p.Apps) {
		app := p.Apps[name]
		facts := []string{"from " + app.Source}
		if app.Namespace != "" {
			facts = append(facts, "in "+app.Namespace)
		}
		facts = append(facts, "config "+app.Config)
		if app.Category != "" {
			facts = append(facts, "category "+app.Category)
		}
		switch app.Carry {
		case plan.CarryFull:
			facts = append(facts, "carries its whole data")
		case plan.CarryMinimal:
			facts = append(facts, "carries its state")
		}
		rows = append(rows, report.Row{Name: name, Facts: facts})
	}
	return rows
}

// dataReportRows is the data root and its renamed directories.
func dataReportRows(p *plan.Plan) []report.Row {
	if p.Data == nil {
		return nil
	}
	rows := []report.Row{{Name: rowRoot, Facts: []string{p.Data.Root}}}
	if renames := pairs(p.Data.Renames()); len(renames) > 0 {
		rows = append(rows, report.Row{Name: "renames", Facts: []string{renames}})
	}
	if folders := pairs(p.Data.MediaRenames()); len(folders) > 0 {
		rows = append(rows, report.Row{Name: "library", Facts: []string{folders}})
	}
	for _, f := range p.Data.Folders {
		if f.From != f.To || f.Why == "" {
			continue
		}
		rows = append(rows, report.Row{Name: f.From, Facts: []string{"stays", f.Why}})
	}
	return rows
}

// pairs is rename pairs as one row reads them: `Downloads → downloads`.
func pairs(list [][2]string) string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r[0]+" → "+r[1])
	}
	return strings.Join(out, " · ")
}

// pathReportRows is every path rule; an unchanged path says so.
func pathReportRows(p *plan.Plan) []report.Row {
	var rows []report.Row
	for _, name := range sortedKeys(p.Apps) {
		for _, r := range p.Apps[name].Paths {
			facts := []string{"→ " + r.To}
			if r.From == r.To {
				facts = []string{leftAsItIs}
			}
			if r.Why != "" {
				facts = append(facts, reasonOf(r.Why))
			}
			rows = append(rows, report.Row{Name: name + " " + r.From, Facts: facts})
		}
	}
	return rows
}

// addressReportRows is every address rule and its app or leave.
func addressReportRows(p *plan.Plan) []report.Row {
	var rows []report.Row
	for _, r := range p.Hosts {
		name := r.Host
		if r.Service != "" {
			name = r.Service
		}
		if r.Port != 0 {
			name += ":" + strconv.Itoa(r.Port)
		}
		if r.Path != "" {
			name += r.Path
		}
		facts := []string{leftAsItIs}
		if r.App != "" {
			facts = []string{"→ " + r.App}
		}
		if r.Why != "" {
			facts = append(facts, reasonOf(r.Why))
		}
		rows = append(rows, report.Row{Name: name, Facts: facts})
	}
	return rows
}

// checkReportRows is unsettled checks, grouped as Settle grouped them.
func checkReportRows(settled apply.Settled) []report.Row {
	var rows []report.Row
	for _, g := range settled {
		for _, it := range g.Items {
			facts := []string{g.Action}
			if it.Why != "" {
				facts = append(facts, it.Why)
			}
			for _, h := range it.Holds {
				facts = append(facts, "held by "+h.String())
			}
			rows = append(rows, report.Row{Name: it.Value, Facts: facts})
		}
	}
	return rows
}

// noteReportRows is the read's own notes.
func noteReportRows(rep discover.Report) []report.Row {
	rows := make([]report.Row, 0, len(rep.Notes))
	for _, n := range rep.Notes {
		rows = append(rows, report.Row{Facts: []string{n}})
	}
	return rows
}

// leftReportRows is what the migration leaves: unknown workloads, skips and
// mounts without a path rule.
func leftReportRows(p *plan.Plan, rep discover.Report) []report.Row {
	var rows []report.Row
	byNamespace := map[string][]string{}
	var order []string
	for _, raw := range p.Outside {
		ns, name := splitWorkload(raw)
		if _, ok := byNamespace[ns]; !ok {
			order = append(order, ns)
		}
		byNamespace[ns] = append(byNamespace[ns], name)
	}
	sort.Strings(order)
	for _, ns := range order {
		rows = append(rows, report.Row{Name: ns, Facts: []string{strings.Join(byNamespace[ns], " · ")}})
	}
	for _, s := range rep.Skipped {
		rows = append(rows, report.Row{Name: s.Workload, Facts: []string{s.Why}})
	}
	for _, i := range rep.Ignored {
		where := i.App + " " + i.Mount
		if i.Host != "" {
			where += " (" + i.Host + ")"
		}
		rows = append(rows, report.Row{Name: where, Facts: []string{i.Why + " on " + i.Volume}})
	}
	return rows
}

// splitWorkload is one outside entry as its namespace and its name: the container
// of a migrated app's pod is named with the workload it belongs to.
func splitWorkload(raw string) (string, string) {
	ns, name, ok := strings.Cut(raw, "/")
	if !ok {
		return "", raw
	}
	return ns, name
}

// reasonOf drops the action a rule already writes in front of its reason.
func reasonOf(why string) string {
	for _, action := range []string{"left as it is: ", "rewrite to "} {
		rest, ok := strings.CutPrefix(why, action)
		if !ok {
			continue
		}
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

// readDigest is the screen's rows: counts and a pointer (rule 7); the rest is the
// in the file.
func readDigest(d report.Doc, out say.Reporter) {
	if len(d.Sections) == 0 {
		return
	}
	counts := map[string]string{}
	for _, c := range d.Counts {
		counts[c.Label] = c.Value
	}
	rows := []say.Row{
		{Mark: say.MarkDone, Name: "read", Facts: []string{counts["apps"] + " apps", counts["addresses"]}},
	}
	if n := counts["decisions"]; n != "" && n != "0" {
		rows = append(rows, say.Row{Mark: say.MarkDone, Name: "decided", Facts: []string{n}})
	}
	if left := counts["left"]; left != "" && !strings.HasPrefix(left, "0 ") {
		rows = append(rows, say.Row{Mark: say.MarkNote, Name: "left", Facts: []string{left + " behind"}})
	}
	if d.Snapshot != "" {
		rows = append(rows, say.Row{Mark: say.MarkNote, Name: "snapshot", Facts: []string{d.Snapshot}})
	}
	path := report.Name(d.Snapshot)
	rows = append(rows, say.Row{Mark: say.MarkNote, Name: "report", Facts: []string{path}, Command: "less " + path})
	out.Rows(rows)
}

// digest prints a read's screen: one heading line naming the command and what it
// read, then the counts and where the report is.
func (r migrationRead) digest(command string, out say.Reporter) {
	if r.doc.Kind != "" {
		out.Line(fmt.Sprintf("%s · %s from %s", command, r.doc.Kind, r.doc.Taken.UTC().Format("2006-01-02 15:04 UTC")))
	}
	readDigest(r.doc, out)
}

// defaultsRow is the last line of a read that took decisions nobody answered: how
// many, and that the report names each one with its reason. The lines themselves
// go to the run log, so the account is there in full without the screen reading
// like one.
func (r migrationRead) defaultsRow(out say.Reporter) {
	defaults := r.settled.Defaults()
	if defaults.Empty() {
		return
	}
	n := 0
	for _, g := range defaults {
		n += len(g.Items)
	}
	for _, line := range defaults.Lines() {
		out.Detail(line)
	}
	out.Rows([]say.Row{{
		Mark:  say.MarkNeeds,
		Name:  "defaults",
		Facts: []string{countText(n) + " taken", "the report names each one"},
	}})
}

// countText is a count the way a person reads it, and its noun: one default, two
// defaults.
func countText(n int) string {
	if n == 1 {
		return "1 default"
	}
	return fmt.Sprintf("%d defaults", n)
}

// commandWidth is the column a command fits: the report's, so a pasted command is
// one a terminal holds.
const commandWidth = report.Width

// commandIndent is where a command's continuation lines hang: the two columns a
// row's command sits in, the mark and its space, and the two columns the
// transcript indents a plan item by.
const commandIndent = 6

// commandLines prints one command the way a person copies it (rule 3): the mark
// in the gutter, the command two columns in, and a long one continued with a
// trailing backslash the way a shell does -- never broken inside a word.
func commandLines(out say.Reporter, command string) {
	for i, line := range wrapCommand(command, commandWidth) {
		if i == 0 {
			out.Line("  " + say.MarkCommand + " " + line)
			continue
		}
		out.Line(strings.Repeat(" ", commandIndent) + line)
	}
}

// wrapCommand breaks one command into lines of at most width columns, ending each
// but the last with ` \`: a command is copied as it stands, so the backslash is
// part of the line it ends. The command's own columns stop short of the width by
// the gutter and the six columns a continuation hangs at.
func wrapCommand(command string, width int) []string {
	words := strings.Fields(command)
	if len(words) == 0 {
		return nil
	}
	// The gutter is two columns and the mark one; a continuation hangs six in;
	// and a line that is not the last ends with " \".
	limit := width - commandIndent - 2
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= limit {
			line += " " + w
			continue
		}
		out = append(out, line+" \\")
		line = w
	}
	return append(out, line)
}
