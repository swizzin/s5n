package apply

// report.go is a run's own account of itself: what it moves, renames, keeps and
// leaves behind, as the sectioned page a person reads in full (docs/output.md,
// rule 7).
//
// The snapshot already carries the read's report -- what discovery found and what
// its checks settled -- and this is the other half: the rows the confirm shows
// folded, every rewrite counted, the notes said once each, the values left as they
// are, and the workloads that stay behind. A screen keeps the counts and points
// here; --details prints the per-rewrite list for someone checking one row.

import (
	"strconv"
	"time"

	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/say"
)

// Report is this run's report. It is written beside the snapshot before the one
// question, so the page a person is shown is the page they can read afterwards.
func (s Summary) Report(kind, snapshot string, taken time.Time, box string) report.Doc {
	d := report.Doc{Kind: kind, Snapshot: snapshot, Taken: taken, Box: box}
	d.Counts = s.reportCounts()
	for _, sec := range []report.Section{
		{Title: "apps", Rows: s.reportApps()},
		{Title: "library", Rows: s.reportLibrary()},
		{Title: "needs you", Rows: tableRows(s.needsYou())},
		{Title: "left as they are", Rows: s.reportLeft()},
		{Title: "notes", Rows: tableRows(s.goodToKnow())},
		{Title: "not migrated", Rows: tableRows(s.notMigrated())},
		{Title: "decisions", Rows: s.reportDefaults()},
	} {
		if len(sec.Rows) > 0 {
			d.Sections = append(d.Sections, sec)
		}
	}
	return d
}

// reportCounts is the summary at the top: the numbers a person takes in before
// deciding whether to read the rest.
func (s Summary) reportCounts() []report.Count {
	var bytes int64
	for _, in := range s.Instances {
		bytes += in.Staged.Bytes
	}
	kept := 0
	for _, in := range s.Instances {
		kept += len(in.Keys) + len(in.Owned)
		if in.Token != "" {
			kept++
		}
	}
	counts := []report.Count{
		{Label: "apps", Value: strconv.Itoa(len(s.Instances))},
		{Label: "carried", Value: bytesText(bytes)},
	}
	if n := len(s.Renames) + len(s.MediaRenames); n > 0 {
		counts = append(counts, report.Count{Label: "renamed", Value: counted(n, "directory", "directories")})
	}
	if kept > 0 {
		counts = append(counts, report.Count{Label: "kept", Value: counted(kept, "credential", "credentials")})
	}
	if n := s.leftCount(); n > 0 {
		counts = append(counts, report.Count{Label: "left as they are", Value: counted(n, "value", "values")})
	}
	if n := s.noteCount(); n > 0 {
		counts = append(counts, report.Count{Label: "notes", Value: strconv.Itoa(n)})
	}
	if n := len(s.Outside); n > 0 {
		counts = append(counts, report.Count{Label: "not migrated", Value: counted(n, "workload", "workloads")})
	}
	return counts
}

// reportApps is one row per planned app with every fact the run knows about it:
// the size, what it carries, what changes, what is kept and what was stopped.
func (s Summary) reportApps() []report.Row {
	rows := make([]report.Row, 0, len(s.Instances))
	for _, in := range s.Instances {
		rows = append(rows, report.Row{Name: in.App, Facts: appFacts(in)})
	}
	return rows
}

// reportLibrary is what the run renames under the data root.
func (s Summary) reportLibrary() []report.Row {
	var rows []report.Row
	if len(s.Renames) > 0 {
		facts := make([]string, 0, len(s.Renames))
		for _, r := range s.Renames {
			facts = append(facts, arrow(r))
		}
		rows = append(rows, report.Row{Name: "data", Facts: facts})
	}
	if len(s.MediaRenames) > 0 {
		facts := make([]string, 0, len(s.MediaRenames))
		for _, r := range s.MediaRenames {
			facts = append(facts, arrow(r))
		}
		rows = append(rows, report.Row{Name: "library", Facts: facts})
	}
	return rows
}

// reportLeft is every value this run leaves as it is, per app, with the reason:
// the list a person checks before the downtime, not after it.
func (s Summary) reportLeft() []report.Row {
	var rows []report.Row
	for _, in := range s.Instances {
		for _, l := range in.Left {
			rows = append(rows, report.Row{Name: in.App + " " + l.What, Facts: []string{"left as it is", l.Why}})
		}
	}
	return rows
}

// reportDefaults is the plan's own decisions, each with its reason: the values
// nobody answered, which the run took.
func (s Summary) reportDefaults() []report.Row {
	rows := make([]report.Row, 0, len(s.Defaults))
	for _, d := range s.Defaults {
		rows = append(rows, report.Row{Facts: []string{d}})
	}
	return rows
}

// tableRows is a grouped block's rows as report rows: the apps that share a
// decision, and the sentence about it.
func tableRows(rows []say.TableRow) []report.Row {
	out := make([]report.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, report.Row{Name: r.Name, Facts: r.Facts})
	}
	return out
}
