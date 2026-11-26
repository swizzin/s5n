package apply

// grouped.go is the confirm's summary as a person reads it. Summary.Lines remains
// the report's account; this file decides what a review screen shows, never what
// apply does.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/say"
)

// The headings the confirm and the report share: a lowercase noun, no colon
// (rule 9), spelled once so the two cannot drift apart.
const (
	titleApps    = "apps"
	titleLibrary = "library"
	// stateKept is what a row says about a value this run leaves alone: one
	// word in the state column, never a phrase (rule 1).
	stateKept = "kept"
)

// Group is one block: a heading, columns and rows; the block itself is say's, so
// every command's table is one mechanism.
type Group = say.Group

// Grouped is the summary as the one confirm shows it.
type Grouped struct {
	// Lead is the first line, when the rows do not say what the run is.
	Lead string
	// Blocks are the tables in reading order.
	Blocks []Group
	// Footer points at the whole account of the run.
	Footer string
}

// Grouped renders the confirm's shape from the summary alone. Notes are counted,
// not listed (rule 7); what a person must act on keeps its own row.
func (s Summary) Grouped() Grouped {
	var g Grouped
	if s.Rehearsal {
		g.Lead = "rehearsal · nothing leaves this box · no data is renamed"
	}
	// No chart means no version column, rather than a column of question marks.
	versions := s.versions()
	columns := []string{"carries", "changes"}
	if versions {
		columns = []string{"carries", "version", "changes"}
	}
	if rows := s.appRows(versions); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Title: titleApps, Columns: columns, Rows: rows})
	}
	if rows := s.dataRows(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Rows: rows})
	}
	if rows := s.needsYou(); len(rows) > 0 {
		g.Blocks = append(g.Blocks, Group{Rows: rows})
	}
	if footer := s.footer(); footer != "" {
		g.Footer = footer
	}
	return g
}

// versions says the confirm has a version column: this run had a chart, so at
// least one app's pair could be read.
func (s Summary) versions() bool {
	for _, in := range s.Instances {
		if in.Version.Cell() != "" {
			return true
		}
	}
	return false
}

// foldAt folds a list past a dozen into one row (rule 7); the report carries all.
const foldAt = 8

// footer is the line under the blocks: what was not shown, by count, and the flag.
func (s Summary) footer() string {
	var facts []string
	if notes := s.noteCount(); notes > 0 {
		facts = append(facts, counted(notes, "note", "notes"))
	}
	if n := len(s.Outside); n > 0 {
		facts = append(facts, counted(n, "other workload", "other workloads")+" stay behind")
	}
	if n := s.leftCount(); n > 0 {
		facts = append(facts, counted(n, "value", "values")+" left as they are")
	}
	if len(facts) == 0 {
		return ""
	}
	return strings.Join(facts, " · ") + " · --details"
}

// noteCount is how many notes the run has, said once each: a note two apps share
// is one note.
func (s Summary) noteCount() int {
	seen := map[string]bool{}
	for _, in := range s.Instances {
		for _, n := range in.Notes {
			seen[n] = true
		}
	}
	for _, c := range s.Carried {
		seen[c.Note] = true
	}
	return len(seen)
}

// leftCount is how many values this run leaves as they are.
func (s Summary) leftCount() int {
	n := 0
	for _, in := range s.Instances {
		n += len(in.Left)
	}
	return n
}

// appRows is one row per app: carried size and versions in columns, then what
// changes.
func (s Summary) appRows(versions bool) []say.TableRow {
	rows := make([]say.TableRow, 0, len(s.Instances)+1)
	for i, in := range s.Instances {
		if i == foldAt {
			names := make([]string, 0, len(s.Instances)-foldAt)
			for _, rest := range s.Instances[foldAt:] {
				names = append(names, rest.App)
			}
			rows = append(rows, say.TableRow{
				Glyph: "+",
				Facts: []string{counted(len(names), "more", "more"), strings.Join(names, ", ")},
			})
			break
		}
		facts := []string{""}
		if in.Staged.Files > 0 {
			facts[0] = bytesText(in.Staged.Bytes)
		}
		if versions {
			facts = append(facts, in.Version.Cell())
		}
		facts = append(facts, changesOf(in))
		rows = append(rows, say.TableRow{Name: in.App, Facts: facts})
	}
	return rows
}

// changesOf is what a row says after the columns.
func changesOf(in InstanceSummary) string {
	var facts []string
	if in.Carry != "" {
		facts = append(facts, in.Carry)
	}
	if in.Short != "" {
		facts = append(facts, in.Short)
	}
	if in.Deferred != "" {
		facts = append(facts, in.Deferred)
	}
	facts = append(facts, changeFacts(in.Changes)...)
	for _, a := range in.Accounts {
		facts = append(facts, fmt.Sprintf("login → %s", a.To))
	}
	for _, o := range in.Owned {
		facts = append(facts, "keeps "+ownedPhrase(o))
	}
	if len(in.Keys) > 0 {
		facts = append(facts, "keeps its API key")
	}
	if in.Token != "" {
		facts = append(facts, "keeps its Plex token")
	}
	if in.Stopped > 0 {
		facts = append(facts, counted(in.Stopped, "torrent", "torrents")+" stopped")
	}
	return strings.Join(facts, " · ")
}

// appFacts is a row's facts: counts, then what is not a count.
func appFacts(in InstanceSummary) []string {
	var facts []string
	if in.Staged.Files > 0 {
		facts = append(facts, bytesText(in.Staged.Bytes))
	}
	if cell := in.Version.Cell(); cell != "" {
		facts = append(facts, cell)
	}
	if in.Carry != "" {
		facts = append(facts, in.Carry)
	}
	if in.Short != "" {
		facts = append(facts, in.Short)
	}
	if in.Deferred != "" {
		facts = append(facts, in.Deferred)
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
	if in.Token != "" {
		facts = append(facts, "keeps its Plex token")
	}
	if in.Stopped > 0 {
		facts = append(facts, counted(in.Stopped, "torrent", "torrents")+" stopped")
	}
	return facts
}

// changeFacts counts changes by kind; the report carries where they are.
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

// CheckedRows is what the question deferred, said in the confirm's shape: a
// deferred check's finding, and an app the disk forced to carry less.
func (s Summary) CheckedRows(agreed Summary) []say.TableRow {
	var rows []say.TableRow
	for _, was := range agreed.Instances {
		if was.Deferred == "" && was.Short == "" {
			continue
		}
		for _, now := range s.Instances {
			if now.App != was.App {
				continue
			}
			rows = append(rows, say.TableRow{Glyph: "✓", Name: now.App, Facts: appFacts(now)})
		}
	}
	return rows
}

// dataRows is what the run renames; a rehearsal has no row.
func (s Summary) dataRows() []say.TableRow {
	var rows []say.TableRow
	if facts := renameFacts(s.Renames, s.Rehearsal); facts != "" {
		rows = append(rows, say.TableRow{Glyph: "✓", Name: "data", Facts: []string{facts}})
	}
	if facts := renameFacts(s.MediaRenames, s.Rehearsal); facts != "" {
		// The library reads as one line: these are the directories a person
		// sees in every app, and they all become box's own names together.
		rows = append(rows, say.TableRow{Glyph: "✓", Name: titleLibrary, Facts: []string{facts}})
	}
	return rows
}

// renameFacts is one row of renames as the run says it: plainly for the run that
// rehearsal's screen says what will happen to a box, never what this run left
func renameFacts(renames []string, rehearsal bool) string {
	if len(renames) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(renames))
	for _, r := range renames {
		pairs = append(pairs, arrow(r))
	}
	facts := strings.Join(pairs, " · ")
	if rehearsal {
		return "the real run renames " + facts
	}
	return facts
}

// arrow is one rename as a person reads it: "Downloads to downloads" is
func arrow(rename string) string { return strings.Replace(rename, " to ", " → ", 1) }

// needsYou groups every value left as it is by the reason it is left: the apps
// that share the reason are named together, and the values themselves are what
// the operator has to decide about. An app this run would downgrade is the first
// row of the block: it is the one thing here that refuses without --yolo.
func (s Summary) needsYou() []say.TableRow {
	type group struct {
		why  string
		apps map[string]bool
		what []string
		seen map[string]bool
	}
	order := make([]string, 0)
	groups := make(map[string]*group)
	rows := make([]say.TableRow, 0)
	for _, v := range s.Downgrades() {
		rows = append(rows, say.TableRow{Glyph: "!", Name: v.App, Facts: []string{v.Source + " → " + v.Box, "the config is newer than the app"}})
	}
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
	for _, why := range order {
		g := groups[why]
		apps := make([]string, 0, len(g.apps))
		for app := range g.apps {
			apps = append(apps, app)
		}
		sort.Strings(apps)
		// One state word and one reason: the value stays, and the clause says
		fact := strings.Join(g.what, " and ")
		rows = append(rows, say.TableRow{Glyph: "!", Name: strings.Join(apps, ", "), State: stateKept, Facts: []string{fact, why}})
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
