// Package say is box's one output seam: the flows emit events, and the reporter
// decides how they look. Plain is the byte-for-byte transcript every non-TTY,
// --plain and TERM=dumb run (and every golden test) reads; internal/tui's
// reporter draws the same events. Dependency-free on purpose: bootstrap and
// platform may not import a TUI.
package say

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Mark is what a plan item does: printed before the text, coloured when live.
type Mark string

const (
	// Install adds something that is not there.
	Install Mark = "+"
	// Question is a step the plan cannot answer before the confirm.
	Question Mark = "?"
	// Warning is something box will not do, said out loud.
	Warning Mark = "!"
	// Change upgrades something already there.
	Change Mark = "~"
	// Verify checks something already there without writing it.
	Verify Mark = "="
	// Remove deletes something.
	Remove Mark = "-"
)

// Item is one row of a plan or a diff. Children are its sub-steps, printed under
// it; they are one level deep, and a child never has children of its own.
type Item struct {
	Mark Mark
	Text string
	// Children are the steps that belong to this one, in application order.
	Children []Item
}

// ReceiptRow is one wiring step: what was checked, its result ("ok", "pending",
// "skipped", "failed") and the reason. Pending means a person must act, and a live
// renderer collects those for after the final card.
type ReceiptRow struct {
	Name   string
	Result string
	Detail string
}

// The gutter's marks: one sits at the start of every line a command prints, and a
// renderer colours it; nothing else about them varies.
const (
	MarkDone    = "✓" // something finished
	MarkFailed  = "✗" // something failed
	MarkNeeds   = "!" // something needs a person
	MarkNote    = "·" // a note, and the default
	MarkRunning = "◐" // the one line that is running
	MarkAsk     = "?" // a question
	MarkCommand = "$" // a command a person can copy
)

// markWidth is the gutter's mark in columns, whatever the glyph costs in bytes.
const markWidth = 1

// The two fixed columns after the gutter: fixed rather than as wide as the widest
// row so two blocks line up and a run of one row reads like a run of ten. NameCol
// is as wide as the widest name box puts there, a release range "0.6.2 → 0.6.3".
const (
	NameCol = 13
	// StateCol is where a one-word state goes (kept, new, moved, skipped), so a
	// column of them scans like a column of numbers.
	StateCol = 8
)

// Row is one line of status output: a mark, a name, and its facts. Why is a refusal's
// one reason, under the row; Command a line to copy under that. No name prints the facts
// after the gutter: a note.
type Row struct {
	Mark  string
	Name  string
	State string
	Facts []string
	Why   string
	// Command is a command that belongs to this row, hanging under it.
	Command string
	// width is the block's name column, set by Block: one long name widens the
	// block rather than pushing its own facts aside. Zero is a row on its own.
	width int
}

// TextCol is where a row's text starts, and where a continuation line hangs.
func (r Row) TextCol() int {
	col := markWidth + 1
	if r.Name == "" {
		return col
	}
	col += NameCol + 2
	if r.State != "" {
		col += StateCol + 2
	}
	return col
}

// TableRow is one row of a table: a status glyph, a name and its facts, laid out
// in columns. An empty glyph prints as ·, and an empty name prints the facts alone.
type TableRow struct {
	Glyph string
	Name  string
	// State is one word on what happened to the row, in its own column after the
	// name. Empty when the heading already says it.
	State string
	Facts []string
}

// Group is one block of a review screen: a heading, the column headings its rows
// read under, and the rows; with no column headings it is a list of one-sentence
// rows. The same mechanism serves every command, in both renderers.
type Group struct {
	Title string
	// Columns are the headings of the rows' facts, in order; empty for prose.
	Columns []string
	Rows    []TableRow
}

// Subject is an error that knows what it is about: it renders as one ✗ row, the
// subject in the name column, the refusal beside it, the reason under that.
type Subject struct {
	Name  string
	Facts []string
	Why   string
	Err   error
}

func (s Subject) Error() string { return s.Err.Error() }

func (s Subject) Unwrap() error { return s.Err }

// Reported marks an error the flow already put on screen: box still exits
// non-zero, and the reporter adds nothing but the line naming the run log, so a
// flow that drew its own failure row does not say it twice.
type Reported struct{ Err error }

func (r Reported) Error() string { return r.Err.Error() }

func (r Reported) Unwrap() error { return r.Err }

// SubjectOf is the subject an error chain is about, built as value or pointer.
func SubjectOf(err error) (Subject, bool) {
	var value Subject
	if errors.As(err, &value) {
		return value, true
	}
	var pointer *Subject
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	return Subject{}, false
}

// ReportedBy says whether this failure was already drawn on screen.
func ReportedBy(err error) bool {
	var value Reported
	if errors.As(err, &value) {
		return true
	}
	var pointer *Reported
	return errors.As(err, &pointer) && pointer != nil
}

// Op is one step that starts and finishes: Label animates when live, Plain is what the
// transcript prints at the start, Budget a timed wait shown as elapsed. Name makes the
// finish line a Row; Source is the newest line of what is waited on, sampled about once
// a second, shown beside the running line and logged once each.
type Op struct {
	Label  string
	Plain  string
	Budget time.Duration
	// Name is the row the finish line is drawn as; empty prints Done's text as
	// one line.
	Name string
	// Source returns the newest line of what this step waits on, or "".
	Source func() string
}

// Handle is a started step: a spinner stops on Done or Failed, and the plain
// renderer prints the finish line.
type Handle interface {
	// Detail is a muted note beside the finish line.
	Detail(text string)
	// Done finishes the step with one line. An empty text only clears a live
	// renderer's spinner.
	Done(text string)
	// DoneAs finishes with different words per renderer: plain keeps the exact
	// transcript, pretty may drop a glyph the spinner's check already carries.
	DoneAs(plain, pretty string)
	// Failed stops the step without printing the error; the caller returns it
	// and the command renders it once, at the end.
	Failed(err error)
}

// Reporter is every line box writes while it works.
type Reporter interface {
	// Phase opens a section: "cluster", "platform", "apply".
	Phase(name string)
	Fact(key, value string)
	// List is a titled list of plain rows ("" omits the title).
	List(title string, rows []string)
	// Receipt is the wiring receipt: the overall status and one row per step.
	// Pending rows wait on a person, so a live renderer may print them after the
	// final card.
	Receipt(status string, rows []ReceiptRow)
	// Plan is a titled list of marked items ("" omits the title); an item's
	// Children print under it with their own marks.
	Plan(title string, items []Item)
	// Table is a titled block ("" omits the title): a glyph, a name, and the facts
	// after it. The name column is as wide as the longest name in the block, so
	// two blocks do not line up with each other.
	Table(title string, rows []TableRow)
	// Group is a titled table read down columns, the last one carrying the words;
	// with no headings it is a list of one-sentence rows.
	Group(g Group)
	// Note is one line of prose under a block, indented to the block's own
	// column.
	Note(text string)
	// ToFinish is the block a run ends with when it leaves something for a person:
	// a label, one row per thing, and the command that clears it under its row.
	ToFinish(rows []Row)
	// Rows is a block of status lines: a mark in the gutter, a name in the fixed
	// column, an optional state word, and the facts. Long text hangs under itself.
	Rows(rows []Row)
	// Detail is a line for the run log alone (object names, revisions, digests,
	// reasoning); a reporter with no log drops it.
	Detail(text string)
	// Diff is a values diff: marked items with no title, flush left in the
	// transcript.
	Diff(items []Item)
	Line(text string)
	Muted(text string)
	// Warn is a line that is a warning.
	Warn(text string)
	Success(text string)
	// Chart names the release box pulled.
	Chart(name, version, ref string)
	// Created reports the directories init made.
	Created(paths []string)
	Start(op Op) Handle
	// Confirm asks one yes/no question and leaves a record of the answer. def is
	// what a bare enter gives, chosen per question: yes for what the run exists to
	// do, no for changing a box that is serving.
	Confirm(question string, def bool) (bool, error)
	// Banner is the final success card. The plain transcript has no card.
	Banner(title, detail string)
	// Error renders a failure once, at the end.
	Error(err error)
	// Close stops a live renderer. It is safe to call twice.
	Close()
}

// Or returns r, or a plain reporter over w when the caller has no reporter.
func Or(r Reporter, w io.Writer) Reporter {
	if r != nil {
		return r
	}
	return &Plain{W: w}
}

// Plain is the transcript: one line per event, no cursor movement, no colour. Its
// output is the contract the e2e scripts and the golden tests read.
type Plain struct {
	W   io.Writer
	Err io.Writer
	// In answers Confirm; Yes answers every question, as --yes does.
	In  io.Reader
	Yes bool
	// Log is the run log: the screen's lines plus Detail, timestamped by the
	// writer behind it. Nil is a run with no log.
	Log io.Writer
	// LogPath is that file, for the one line a failed run ends with. Empty
	// with no log.
	LogPath string
	// Now is the clock a step's duration is measured with; nil is time.Now.
	Now func() time.Time
	// Sample is how often a waiting step's source is read; zero is a second.
	Sample time.Duration

	reader *bufio.Reader
}

func (p *Plain) out() io.Writer {
	w := p.W
	if w == nil {
		w = io.Discard
	}
	// A caller may hand the same writer for both, and then every line is written
	// once.
	if p.Log != nil && p.Log != w {
		return io.MultiWriter(w, p.Log)
	}
	return w
}

func (p *Plain) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Plain) every() time.Duration {
	if p.Sample > 0 {
		return p.Sample
	}
	return time.Second
}

// Detail is a line for the log alone: nothing about it reaches the screen.
func (p *Plain) Detail(text string) {
	if p.Log == nil || strings.TrimSpace(text) == "" {
		return
	}
	fmt.Fprintln(p.Log, text)
}

// Phase is a heading: a lowercase noun alone on its line (rule 9), which a live
// renderer draws in its masthead instead.
func (p *Plain) Phase(name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	fmt.Fprintln(p.out(), name)
}

func (p *Plain) Fact(key, value string) {
	fmt.Fprintf(p.out(), "%s: %s\n", key, value)
}

func (p *Plain) List(title string, rows []string) {
	if title != "" {
		fmt.Fprintf(p.out(), "%s:\n", title)
	}
	for _, row := range rows {
		fmt.Fprintf(p.out(), "  %s\n", row)
	}
}

// Receipt is the transcript's receipt, a pending row in place.
func (p *Plain) Receipt(status string, rows []ReceiptRow) {
	fmt.Fprintf(p.out(), "wiring %s\n", status)
	for _, row := range rows {
		if row.Detail != "" {
			fmt.Fprintf(p.out(), "  %s: %s (%s)\n", row.Name, row.Result, row.Detail)
			continue
		}
		fmt.Fprintf(p.out(), "  %s: %s\n", row.Name, row.Result)
	}
}

func (p *Plain) Plan(title string, items []Item) {
	if len(items) == 0 {
		return
	}
	if title != "" {
		fmt.Fprintf(p.out(), "%s:\n", title)
	}
	for _, it := range items {
		p.planItem(itemIndent, it)
		for _, child := range it.Children {
			p.planItem(childIndent, child)
		}
	}
}

// The plain transcript's plan columns: an item two columns in, a child two
// columns past where the item's text starts. Spaces only, for screen readers and
// logged runs.
const (
	itemIndent = "  "
	// childIndent is an item's mark and space, then the child's two columns.
	childIndent = "      "
)

func (p *Plain) planItem(level string, it Item) {
	fmt.Fprintf(p.out(), "%s%s %s\n", level, it.Mark, it.Text)
}

func (p *Plain) Diff(items []Item) {
	for _, it := range items {
		fmt.Fprintf(p.out(), "%s %s\n", it.Mark, it.Text)
	}
}

// Table is the transcript's table: the name padded to the block's widest, then
// the facts. Nothing is wrapped: a logged run and a golden test read the same
// bytes whatever the terminal was.
func (p *Plain) Table(title string, rows []TableRow) {
	if len(rows) == 0 {
		return
	}
	if title != "" {
		fmt.Fprintf(p.out(), "%s\n", title)
	}
	nameWidth := blockNameWidth(rows)
	for _, r := range rows {
		glyph := r.Glyph
		if glyph == "" {
			glyph = "·"
		}
		facts := strings.Join(r.Facts, " · ")
		switch {
		case r.Name == "":
			fmt.Fprintf(p.out(), "  %s %s\n", glyph, facts)
		case facts == "":
			fmt.Fprintf(p.out(), "  %s %s\n", glyph, r.Name)
		default:
			fmt.Fprintf(p.out(), "  %s %s  %s\n", glyph, pad(r.Name, nameWidth), facts)
		}
	}
}

// Rows is the transcript's status block: nothing is wrapped, so a golden test
// reads the same bytes whatever the terminal was, but a row's why and command
// still hang at its own text column: the live layout without the wrapping.
func (p *Plain) Rows(rows []Row) {
	for _, row := range Block(rows) {
		fmt.Fprint(p.out(), PlainRow(row))
	}
}

// Block gives every row of a block the same name column, as wide as the widest
// name and never narrower than NameCol. Both renderers pass through here, so the
// transcript and the screen agree.
func Block(rows []Row) []Row {
	width := NameCol
	for _, row := range rows {
		if n := columns(row.Name); n > width {
			width = n
		}
	}
	out := make([]Row, len(rows))
	for i, row := range rows {
		row.width = width
		out[i] = row
	}
	return out
}

// Group is the transcript's columned block. The last column carries the words and
// wraps at Width under its own column.
func (p *Plain) Group(g Group) {
	if len(g.Rows) == 0 {
		return
	}
	nameWidth := blockNameWidth(g.Rows)
	if len(g.Columns) == 0 {
		p.groupList(g, nameWidth)
		return
	}
	p.groupTable(g, nameWidth)
}

// groupList is a block whose rows are a sentence each.
func (p *Plain) groupList(g Group, nameWidth int) {
	if g.Title != "" {
		fmt.Fprintln(p.out(), g.Title)
	}
	for _, r := range g.Rows {
		glyph := r.Glyph
		if glyph == "" {
			glyph = MarkNote
		}
		head := "  " + glyph + " "
		if r.Name != "" {
			head += pad(r.Name, nameWidth) + "  "
		}
		if r.State != "" {
			// One word in its own column, so a block of them scans like numbers (rule 1).
			head += pad(r.State, stateWidth(r.State)) + "  "
		}
		hang := strings.Repeat(" ", columns(head))
		for i, line := range Wrap(strings.Join(r.Facts, " · "), Width-columns(head)) {
			if i == 0 {
				fmt.Fprintln(p.out(), head+line)
				continue
			}
			fmt.Fprintln(p.out(), hang+line)
		}
	}
}

// groupTable is a block with columns: the heading is the first column's, and
// every column but the last is padded to the widest fact in it.
func (p *Plain) groupTable(g Group, nameWidth int) {
	cols := columnWidths(g)
	head := pad(g.Title, 2+nameWidth)
	for i, c := range g.Columns {
		head += "  " + pad(c, cols[i])
	}
	fmt.Fprintln(p.out(), strings.TrimRight(head, " "))
	for _, r := range g.Rows {
		if r.Glyph != "" {
			// The fold: "+ 5 more" and the names that did not fit.
			fmt.Fprintf(p.out(), "  %s %s\n", r.Glyph, strings.Join(r.Facts, "  "))
			continue
		}
		line := "  " + pad(r.Name, nameWidth)
		for i := 0; i+1 < len(r.Facts); i++ {
			col := 0
			if i < len(cols) {
				col = cols[i]
			}
			line += "  " + pad(r.Facts[i], col)
		}
		if len(r.Facts) == 0 || strings.TrimSpace(r.Facts[len(r.Facts)-1]) == "" {
			fmt.Fprintln(p.out(), strings.TrimRight(line, " "))
			continue
		}
		line += "  "
		hang := strings.Repeat(" ", columns(line))
		for i, wrapped := range Wrap(r.Facts[len(r.Facts)-1], Width-columns(line)) {
			if i == 0 {
				fmt.Fprintln(p.out(), line+wrapped)
				continue
			}
			fmt.Fprintln(p.out(), hang+wrapped)
		}
	}
}

// stateWidth is the state column, widened by a word that does not fit.
func stateWidth(state string) int {
	if w := columns(state); w > StateCol {
		return w
	}
	return StateCol
}

// Note is a line of prose under a block: two columns in, so it belongs to the
// block above, its continuation hanging under the text.
func (p *Plain) Note(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// The continuation hangs four columns in: wrap where that still fits.
	for i, line := range Wrap(text, Width-4) {
		if i == 0 {
			fmt.Fprintln(p.out(), "  "+line)
			continue
		}
		fmt.Fprintln(p.out(), "    "+line)
	}
}

// columnWidths are a group's leading columns, as wide as each one's widest fact.
func columnWidths(g Group) []int {
	cols := make([]int, len(g.Columns))
	for _, r := range g.Rows {
		if r.Glyph != "" {
			continue
		}
		for i := range g.Columns {
			if i+1 < len(r.Facts) && columns(r.Facts[i]) > cols[i] {
				cols[i] = columns(r.Facts[i])
			}
		}
	}
	return cols
}

// ToFinish is the transcript's block of what is left for a person: the label
// once (rule 1), each row two columns in, and its command under it.
func (p *Plain) ToFinish(rows []Row) {
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, row := range rows {
		if w := columns(row.Name); w > width {
			width = w
		}
	}
	fmt.Fprintln(p.out(), MarkNeeds+" to finish")
	for _, row := range rows {
		head := "  "
		if row.Name != "" {
			head += pad(row.Name, width) + "  "
		}
		hang := strings.Repeat(" ", columns(head))
		facts := strings.Join(row.Facts, " · ")
		for i, line := range Wrap(facts, Width-columns(head)) {
			if i == 0 {
				fmt.Fprintln(p.out(), head+line)
				continue
			}
			fmt.Fprintln(p.out(), hang+line)
		}
		for i, line := range WrapShell(row.Command, Width-columns(hang)-2) {
			if row.Command == "" {
				break
			}
			if i == 0 {
				fmt.Fprintln(p.out(), hang+MarkCommand+" "+line)
				continue
			}
			fmt.Fprintln(p.out(), hang+"  "+line)
		}
	}
}

// blockNameWidth is a block's name column, as wide as its longest name.
func blockNameWidth(rows []TableRow) int {
	w := 0
	for _, r := range rows {
		if n := columns(r.Name); n > w {
			w = n
		}
	}
	return w
}

// PlainRow lays one row out for the transcript and the log alike: facts, why and
// command under the row's text column, never wrapped, so a golden test reads the same
// bytes whatever the terminal was.
func PlainRow(r Row) string {
	mark := r.Mark
	if mark == "" {
		mark = MarkNote
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(r.Prefix(mark)+r.Text(), " ") + "\n")
	cont := strings.Repeat(" ", r.TextCol())
	if r.Why != "" {
		b.WriteString(cont + r.Why + "\n")
	}
	if r.Command != "" {
		b.WriteString(cont + MarkCommand + " " + r.Command + "\n")
	}
	return b.String()
}

// Fold replaces rows that differ only by name with one: Name, its first fact gaining
// "for N unit", the rest as they were (rules 3 and 4). A lone row is itself.
func Fold(name, unit string, rows []Row) []Row {
	if len(rows) < 2 || len(rows[0].Facts) == 0 {
		return rows
	}
	first := rows[0]
	for _, row := range rows[1:] {
		if row.Mark != first.Mark || row.State != first.State || row.Why != first.Why ||
			row.Command != first.Command || len(row.Facts) != len(first.Facts) ||
			strings.Join(row.Facts, "\x00") != strings.Join(first.Facts, "\x00") {
			return rows
		}
	}
	facts := make([]string, 0, len(first.Facts))
	facts = append(facts, first.Facts[0]+" for "+fmt.Sprintf("%d", len(rows))+" "+plural(len(rows), unit))
	facts = append(facts, first.Facts[1:]...)
	return []Row{{Mark: first.Mark, Name: name, Facts: facts}}
}

// plural is "1 host" and "2 hosts": the regular ending, which is every unit box
// counts with.
func plural(n int, one string) string {
	if n == 1 {
		return one
	}
	return one + "s"
}

// Prefix is the gutter and name columns of a row, the mark drawn by the renderer.
// Everything after it is the row's text.
func (r Row) Prefix(mark string) string {
	head := mark + " "
	if r.Name == "" {
		return head
	}
	head += pad(r.Name, r.nameWidth())
	if r.State != "" {
		head += "  " + pad(r.State, r.stateWidth())
	}
	return head + "  "
}

// Text is the row's facts, joined the way a table's are.
func (r Row) Text() string { return strings.Join(r.Facts, " · ") }

// nameWidth is the block's name column when Block set one, else this row's own:
// fixed, unless its name is wider and takes the column with it, because a
// truncated name is a different name.
func (r Row) nameWidth() int {
	if r.width > 0 {
		return r.width
	}
	if w := columns(r.Name); w > NameCol {
		return w
	}
	return NameCol
}

func (r Row) stateWidth() int {
	if w := columns(r.State); w > StateCol {
		return w
	}
	return StateCol
}

// columns is a string's width: box writes no double-width glyphs in a name or a
// fact, so runes are columns.
func columns(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, width int) string {
	if n := width - columns(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// Width is the column box writes to when no terminal decides: the transcript, the
// run log and a migration's report break their lines in the same places.
const Width = 80

// Wrap breaks text into lines of at most width columns, never inside a word: half
// a path is a different path.
func Wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := glueSeparators(strings.Fields(text))
	if len(words) == 0 {
		return []string{""}
	}
	out := []string{}
	line := words[0]
	for _, w := range words[1:] {
		if columns(line)+1+columns(w) <= width {
			line += " " + w
			continue
		}
		out = append(out, line)
		line = w
	}
	return append(out, line)
}

// WrapShell breaks a command at the spaces with a trailing `\` on every line but
// the last, so a pasted command runs as it stands.
func WrapShell(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	if width < 4 {
		width = 4
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if columns(line)+1+columns(w) <= width {
			line += " " + w
			continue
		}
		out = append(out, line+" \\")
		line = "    " + w
	}
	return append(out, line)
}

// Duration is a span as a person reads it: 41s, 4m10s, 10m. Zeros are left out,
// so elapsed and limit read alike in `4m10s of 10m`.
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d / time.Hour)
	m := int(d%time.Hour) / int(time.Minute)
	s := int(d%time.Minute) / int(time.Second)
	switch {
	case h > 0 && m > 0 && s > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0 && s > 0:
		return fmt.Sprintf("%dh%ds", h, s)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%dm", m)
}

// Watcher reads a waiting step's source and says which sampled lines are new: one
// dedupe rule for both renderers, so a line repeated for four minutes is logged
// once.
type Watcher struct {
	// Source returns the newest line of what is waited on.
	Source func() string
	last   string
	seen   bool
}

// Next is the source's newest line, when it is not the one already reported.
func (w *Watcher) Next() (string, bool) {
	if w.Source == nil {
		return "", false
	}
	line := strings.TrimSpace(w.Source())
	if line == "" || (w.seen && line == w.last) {
		return "", false
	}
	w.last = line
	w.seen = true
	return line, true
}

func (p *Plain) Line(text string)  { fmt.Fprintln(p.out(), text) }
func (p *Plain) Muted(text string) { fmt.Fprintln(p.out(), text) }

// Warn is a line that needs a person: the `!` mark in both renderers (rule 1). A
// line only worth knowing is a note, and a note is a `·` row.
func (p *Plain) Warn(text string) { fmt.Fprintln(p.out(), MarkNeeds+" "+text) }

// Success is something that finished well: the ✓ mark in the gutter, in both
// renderers.
func (p *Plain) Success(text string)   { fmt.Fprintln(p.out(), MarkDone+" "+text) }
func (p *Plain) Banner(string, string) {}

// Chart names the release box pulled: name and version on screen, the reference in
// the log, where plumbing belongs (rule 5).
func (p *Plain) Chart(name, version, ref string) {
	fmt.Fprint(p.out(), PlainRow(Row{Mark: MarkDone, Name: "chart", Facts: []string{name + " " + version}}))
	p.Detail("chart " + name + " " + version + " (" + ref + ")")
}

// Created is one row: how many directories this run made. The paths are the log's:
// a person wants to know init made them, not which.
func (p *Plain) Created(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprint(p.out(), PlainRow(Row{Mark: MarkDone, Name: "directories", Facts: []string{fmt.Sprintf("%d made", len(paths))}}))
	for _, path := range paths {
		p.Detail("made " + path)
	}
}

// Start prints the step's plain start line, if any, and returns its handle.
func (p *Plain) Start(op Op) Handle {
	if op.Plain != "" {
		fmt.Fprintln(p.out(), op.Plain)
	}
	h := &plainHandle{p: p, op: op, start: p.now()}
	if op.Source != nil {
		h.stop = make(chan struct{})
		h.stopped = make(chan struct{})
		go h.sample()
	}
	return h
}

type plainHandle struct {
	p       *Plain
	op      Op
	start   time.Time
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// sample reads the source about once a second, for as long as the step runs.
func (h *plainHandle) sample() {
	w := &Watcher{Source: h.op.Source}
	t := time.NewTicker(h.p.every())
	defer t.Stop()
	defer close(h.stopped)
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			if line, ok := w.Next(); ok {
				h.p.Detail(line)
			}
		}
	}
}

// stopSampling ends the sampler and waits for it, so the finish line is written
// after the last sampled one: a log still being written while the process ends
// loses its tail.
func (h *plainHandle) stopSampling() {
	if h.stop == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.stopped
}

func (h *plainHandle) Detail(string) {}

// Done prints a named step's finish as its row, what it did and how long it took;
// an unnamed step prints the sentence.
func (h *plainHandle) Done(text string) {
	h.stopSampling()
	if h.op.Name == "" {
		if text != "" {
			fmt.Fprintln(h.p.out(), text)
		}
		return
	}
	fmt.Fprint(h.p.out(), PlainRow(Row{Mark: MarkDone, Name: h.op.Name, Facts: h.facts(text)}))
}

// facts are a named step's: what it did, then how long it took.
func (h *plainHandle) facts(text string) []string {
	var facts []string
	if text != "" {
		facts = append(facts, text)
	}
	return append(facts, Duration(h.p.now().Sub(h.start)))
}

func (h *plainHandle) DoneAs(plain, _ string) { h.Done(plain) }

func (h *plainHandle) Failed(error) {
	h.stopSampling()
	if h.op.Name == "" {
		return
	}
	fmt.Fprint(h.p.out(), PlainRow(Row{Mark: MarkFailed, Name: h.op.Name}))
}

// Ask is release.Confirmer and platform.Confirmer: the same question the live
// reporter draws a form for, answered on the transcript.
func (p *Plain) Ask(question string, def bool) (bool, error) { return p.Confirm(question, def) }

// Confirm is one yes/no question. --yes answers it, and there is no answer without a
// reader: a prompt that cannot be shown is a refusal, never an implicit yes, because box
// writes to a cluster as root. cmd/box gives a Plain a reader only when stdin is a
// terminal (a character device is not enough: /dev/null is one). def is what an empty
// line answers, shown as [Y/n] or [y/N]; an explicit y or n wins.
func (p *Plain) Confirm(question string, def bool) (bool, error) {
	if p.Yes {
		return true, nil
	}
	if p.In == nil {
		return false, errors.New("not a terminal: pass --yes to apply without a prompt")
	}
	if p.reader == nil {
		p.reader = bufio.NewReader(p.In)
	}
	// The mark in the gutter, then the question as the flow wrote it (rule 1).
	defaultHint := "[y/N]"
	if def {
		defaultHint = "[Y/n]"
	}
	fmt.Fprintf(p.out(), "%s %s %s ", MarkAsk, question, defaultHint)
	line, err := p.reader.ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("read the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Error is the one failure line, in box's voice: a ✗ row, its reason hanging under
// it, and the run log's path last, on stderr.
func (p *Plain) Error(err error) {
	w := p.Err
	if w == nil {
		w = p.out()
	} else if p.Log != nil {
		w = io.MultiWriter(w, p.Log)
	}
	if ReportedBy(err) {
		p.logRow()
		return
	}
	if subject, ok := SubjectOf(err); ok {
		fmt.Fprint(w, PlainRow(Row{Mark: MarkFailed, Name: subject.Name, Facts: subject.Facts, Why: subject.Why}))
		p.logRow()
		return
	}
	for i, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
		if i == 0 {
			fmt.Fprintf(w, "%s %s\n", MarkFailed, line)
			continue
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
	p.logRow()
}

// logRow is the last line of a failed run: where the transcript is.
func (p *Plain) logRow() {
	if p.LogPath == "" {
		return
	}
	fmt.Fprint(p.out(), PlainRow(Row{Mark: MarkNote, Name: "log", Facts: []string{p.LogPath}}))
}

func (p *Plain) Close() {}

// glueSeparators keeps a middle dot on the line of the fact before it: a wrapped
// line starting with "·" would read as a row of its own.
func glueSeparators(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w == "·" && len(out) > 0 {
			out[len(out)-1] += " ·"
			continue
		}
		out = append(out, w)
	}
	return out
}
