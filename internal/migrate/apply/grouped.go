package apply

// grouped.go is the confirm's summary as a person reads it: one row per app
// with what it carries and what changes, one block for what needs them, one for
// the notes, and what was not migrated folded by namespace. The per-column list
// (Summary.Lines) stays the report's and `--details`' account of the same run:
// this file only decides what a review screen shows, never what apply does.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/say"
)

// Group is one titled block of the confirm: the rows of a table and the line
// above them.
type Group struct {
	Title string
	Rows  []say.TableRow
}

// Grouped is the summary as the one confirm shows it.
type Grouped struct {
	// Lead is the first line: what this run is, when that is not obvious from
	// the rows (a rehearsal leaves this box and renames nothing).
	Lead string
	// Blocks are the tables, in the order a person reads them: the apps,
	// the data that moves, what needs them, what to know, what is not coming.
	Blocks []Group
	// Footer points at the whole account of the run.
	Footer string
}

// Grouped renders the summary in the shape the confirm uses. It reads nothing
// but the summary: the same run rendered with --details prints Lines instead.
func (s Summary) Grouped() Grouped {
	var g Grouped
	if s.Rehearsal {
		g.Lead = "rehearsal · nothing leaves this box · no data is renamed"
	}
	if rows := s.appRows(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: counted(len(rows), "app", "apps"), Rows: rows})
	}
	if rows := s.dataRows(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: "data", Rows: rows})
	}
	if rows := s.needsYou(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: "needs you", Rows: rows})
	}
	if rows := s.goodToKnow(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: "good to know", Rows: rows})
	}
	if rows := s.notMigrated(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: fmt.Sprintf("not migrated (%d)", len(s.Outside)), Rows: rows})
	}
	if len(g.Blocks) > 0 {
		g.Footer = "every rewrite, column by column: report.txt in the snapshot, or --details"
	}
	return g
}

// appRows is one row per app: what it carries, how many stored values change,
// and the one or two facts a person acts on.
func (s Summary) appRows() []say.TableRow {
	rows := make([]say.TableRow, 0, len(s.Instances))
	for _, in := range s.Instances {
		rows = append(rows, say.TableRow{Glyph: "✓", Name: in.App, Facts: appFacts(in)})
	}
	return rows
}

// appFacts is one app's row: carried size, the counts of what changes, then the
// things that are not a count -- an account renamed, a connection kept, its own
// key kept, torrents stopped.
func appFacts(in InstanceSummary) []string {
	var facts []string
	if in.Staged.Files > 0 {
		facts = append(facts, bytesText(in.Staged.Bytes))
	}
	facts = append(facts, changeFacts(in.Changes)...)
	for _, a := range in.Accounts {
		facts = append(facts, fmt.Sprintf("account %q becomes %s (sign in through the box)", a.From, a.To))
	}
	for _, o := range in.Owned {
		facts = append(facts, "keeps "+ownedPhrase(o))
	}
	if len(in.Keys) > 0 {
		facts = append(facts, "keeps its API key")
	}
	if in.Stopped > 0 {
		facts = append(facts, counted(in.Stopped, "torrent", "torrents")+" stopped")
	}
	return facts
}

// changeFacts counts a report's changes by what they are: paths in a database
// or a config, addresses between apps, and everything else a setting. A count
// is what a person reviews; which column and which row is the report's.
func changeFacts(changes []rewrite.Change) []string {
	var paths, addresses, settings int
	for _, c := range changes {
		switch c.What {
		case "path":
			paths += c.Count
		case "host", "port", "url", "url base", "url base mode":
			addresses += c.Count
		default:
			settings += c.Count
		}
	}
	var facts []string
	if paths > 0 {
		facts = append(facts, counted(paths, "path", "paths"))
	}
	if addresses > 0 {
		facts = append(facts, counted(addresses, "address", "addresses"))
	}
	if settings > 0 {
		facts = append(facts, counted(settings, "setting", "settings"))
	}
	return facts
}

// ownedPhrase is one kept connection in the words of the app it is in.
func ownedPhrase(o rewrite.Owned) string {
	kind := o.Kind
	switch kind {
	case "downloadclient":
		kind = "download client"
	case "application":
		kind = "Prowlarr application"
	case "pbh-downloader":
		kind = "downloader"
	}
	return fmt.Sprintf("%s %q", kind, o.Name)
}

// dataRows is what the run does to the data root: the two directories renamed
// to box's names. A rehearsal renames nothing and has no row.
func (s Summary) dataRows() []say.TableRow {
	if len(s.Renames) == 0 {
		return nil
	}
	facts := make([]string, 0, len(s.Renames))
	for _, r := range s.Renames {
		facts = append(facts, strings.Replace(r, " to ", " → ", 1))
	}
	return []say.TableRow{{Glyph: "✓", Name: "data", Facts: facts}}
}

// needsYou groups every value left as it is by the reason it is left: the apps
// that share the reason are named together, and the values themselves are what
// the operator has to decide about.
func (s Summary) needsYou() []say.TableRow {
	type group struct {
		why  string
		apps map[string]bool
		what []string
		seen map[string]bool
	}
	order := make([]string, 0)
	groups := make(map[string]*group)
	for _, in := range s.Instances {
		for _, l := range in.Left {
			g, ok := groups[l.Why]
			if !ok {
				g = &group{why: l.Why, apps: map[string]bool{}, seen: map[string]bool{}}
				groups[l.Why] = g
				order = append(order, l.Why)
			}
			g.apps[in.App] = true
			if !g.seen[l.What] {
				g.seen[l.What] = true
				g.what = append(g.what, l.What)
			}
		}
	}
	rows := make([]say.TableRow, 0, len(order))
	for _, why := range order {
		g := groups[why]
		apps := make([]string, 0, len(g.apps))
		for app := range g.apps {
			apps = append(apps, app)
		}
		sort.Strings(apps)
		fact := strings.Join(g.what, " and ") + " stay as it is: " + g.why
		if len(g.what) == 1 {
			fact = g.what[0] + " stays as it is: " + g.why
		}
		rows = append(rows, say.TableRow{Glyph: "!", Name: strings.Join(apps, ", "), Facts: []string{fact}})
	}
	return rows
}

// goodToKnow is every note, said once: a note two apps share names both, and
// the part of a config a snapshot did not carry is theirs. The plan's own
// decisions that left a stored value alone are here too, counted by what they
// are; the ones that rewrote an address are already in that app's count.
func (s Summary) goodToKnow() []say.TableRow {
	type group struct {
		text string
		apps map[string]bool
	}
	order := make([]string, 0)
	groups := make(map[string]*group)
	add := func(text, app string) {
		g, ok := groups[text]
		if !ok {
			g = &group{text: text, apps: map[string]bool{}}
			groups[text] = g
			order = append(order, text)
		}
		if app != "" {
			g.apps[app] = true
		}
	}
	for _, in := range s.Instances {
		for _, note := range in.Notes {
			add(note, in.App)
		}
	}
	for _, c := range s.Carried {
		add(c.Note, c.App)
	}
	// Defaults that rewrote an app's own address are counted in that app's row
	// (the rewrite itself is one of its changes); the ones that left a value
	// alone are said here, once per reason.
	counts := map[string]int{}
	var defaultOrder []string
	for _, d := range s.DefaultRules {
		if d.App != "" {
			continue
		}
		key := d.Kind + "\x00" + d.Why
		if counts[key] == 0 {
			defaultOrder = append(defaultOrder, key)
		}
		counts[key]++
	}
	rows := make([]say.TableRow, 0, len(order)+len(defaultOrder))
	for _, text := range order {
		g := groups[text]
		name := ""
		if len(g.apps) > 0 {
			apps := make([]string, 0, len(g.apps))
			for app := range g.apps {
				apps = append(apps, app)
			}
			sort.Strings(apps)
			name = strings.Join(apps, ", ")
		}
		rows = append(rows, say.TableRow{Glyph: "·", Name: name, Facts: []string{g.text}})
	}
	for _, key := range defaultOrder {
		kind, why, _ := strings.Cut(key, "\x00")
		n := counts[key]
		one, many := kind, kind+"s"
		if kind == "address" {
			many = "addresses"
		}
		rows = append(rows, say.TableRow{Glyph: "·", Name: counted(n, one, many), Facts: []string{why}})
	}
	return rows
}

// notMigrated folds the source's other workloads by namespace, one row each:
// the names that are not coming are what a person brings back by hand.
func (s Summary) notMigrated() []say.TableRow {
	if len(s.Outside) == 0 {
		return nil
	}
	byNamespace := map[string][]string{}
	var order []string
	for _, raw := range s.Outside {
		parts := strings.Split(raw, "/")
		ns := parts[0]
		name := raw
		if len(parts) > 1 {
			name = strings.Join(parts[1:], "/")
		}
		if _, ok := byNamespace[ns]; !ok {
			order = append(order, ns)
		}
		byNamespace[ns] = append(byNamespace[ns], name)
	}
	sort.Strings(order)
	rows := make([]say.TableRow, 0, len(order))
	for _, ns := range order {
		rows = append(rows, say.TableRow{Glyph: "·", Name: ns, Facts: []string{strings.Join(byNamespace[ns], ", ")}})
	}
	return rows
}

// countText is a count the way a person reads one: 1,532, not 1532.
func countText(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// counted is "1 app" and "2 apps".
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return countText(n) + " " + many
}
