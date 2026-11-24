// Package say is box's one output seam.
//
// The flow packages decide *what* happens (a release is installed, a wait has a
// budget, a plan has a conditional step, a question was answered); this package
// names those events, and the renderer decides how they look. say itself is
// dependency-free: the plain implementation here is the byte-for-byte
// transcript box has always printed, and internal/tui's reporter is the same
// events drawn with box's masthead, accent and spinner.
//
// A caller that has no reporter keeps the old io.Writer contract through Plain,
// which is what the unit tests and every piped run use.
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

// Mark is what a plan item does. The plain renderer prints it before the text;
// a live renderer colours it.
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

// Item is one row of a plan or a diff.
//
// Children are the sub-steps of one row: what a component is made of, printed
// under it and two columns past where its own text starts. They are one level
// deep, and a child never has children of its own.
type Item struct {
	Mark Mark
	Text string
	// Children are the steps that belong to this one, in the order they are
	// applied. Empty for a row that is a step of its own.
	Children []Item
}

// ReceiptRow is one step of the wiring receipt: what was checked, what it cost
// ("ok", "pending", "skipped" or "failed") and, when a live renderer has room
// for it, the reason. Pending means a person still has to do something, which
// a live renderer collects for after the final card.
type ReceiptRow struct {
	Name   string
	Result string
	Detail string
}

// The gutter's marks. One mark sits in a two-column gutter at the start of
// every line a command prints: done, failed, needs you, a note, running, a
// question, a command. A renderer colours them; nothing else about them varies.
const (
	MarkDone    = "✓" // something finished
	MarkFailed  = "✗" // something failed
	MarkNeeds   = "!" // something needs a person
	MarkNote    = "·" // a note, and the default
	MarkRunning = "◐" // the one line that is running
	MarkAsk     = "?" // a question
	MarkCommand = "$" // a command a person can copy
)

// markWidth is the gutter's mark in columns: one, whatever the glyph costs in
// bytes.
const markWidth = 1

// The two fixed columns after the gutter. They are fixed rather than as wide as
// the widest row in the block so two blocks line up, and a run of one row reads
// like a run of ten. NameCol is thirteen columns: the widest name box puts in
// the column is a release range, "0.6.2 → 0.6.3".
const (
	NameCol = 13
	// StateCol is where a state word goes: kept, new, moved, skipped. One
	// word, so a column of them scans like a column of numbers.
	StateCol = 8
)

// Row is one line of box's status output: a mark in the gutter, a name, and the
// facts that belong to it.
//
// Facts are joined with a middle dot and hang under the name on their own
// column. Why is the one reason a refusal gives, on the line under the row;
// Command is a line to copy, under that at the row's own text column. A row
// with no name prints its facts straight after the gutter, which is what a note
// is.
type Row struct {
	Mark  string
	Name  string
	State string
	Facts []string
	Why   string
	// Command is a command that belongs to this row: `box status` under the
	// note that says what needs doing.
	Command string
	// width is the block's name column, set by Block: a block reads down one
	// column, so one long name widens the block rather than pushing its own
	// facts off to a column of its own. Zero is a row on its own.
	width int
}

// TextCol is where a row's text starts, which is where a continuation line
// hangs: past the gutter, the name column and the state column.
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

// TableRow is one row of a table: a status glyph, a name and the facts that
// belong to it, laid out in columns. The glyph is the row's own -- ✓ for
// something carried, ! for something a person has to do, · for a note -- and an
// empty one prints as ·. An empty name prints the facts alone on the glyph's
// line.
type TableRow struct {
	Glyph string
	Name  string
	Facts []string
}

// Group is one block of a review screen: a heading, the headings of the columns
// its rows read in, and the rows. A group with no column headings is a list of
// rows whose facts are one sentence each.
//
// It is the block a screen that compares things down a column is made of -- the
// apps a migration moves, what each carries, what changes -- and it is the same
// mechanism whatever the command: one table, one set of columns, in both
// renderers.
type Group struct {
	Title string
	// Columns are the headings of the rows' facts, in order, when this block
	// reads as a table. Empty for a block whose facts are prose.
	Columns []string
	Rows    []TableRow
}

// Subject is an error that knows what it is about: a renderer draws it as the
// one ✗ row, the subject in the name column, the refusal beside it, and the one
// reason under that.
type Subject struct {
	Name  string
	Facts []string
	Why   string
	Err   error
}

func (s Subject) Error() string { return s.Err.Error() }

func (s Subject) Unwrap() error { return s.Err }

// Reported marks an error the flow has already put on screen itself: box still
// exits non-zero with it, and the reporter adds nothing but the line naming the
// run log. A flow that draws its own failure row and then returns the error
// would otherwise say it twice.
type Reported struct{ Err error }

func (r Reported) Error() string { return r.Err.Error() }

func (r Reported) Unwrap() error { return r.Err }

// SubjectOf is the subject an error chain is about, whether the flow built it as
// a value or a pointer: both end on the one ✗ row.
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

// ReportedBy says whether this error is a failure the flow already drew: box
// adds nothing but the line naming the run log.
func ReportedBy(err error) bool {
	var value Reported
	if errors.As(err, &value) {
		return true
	}
	var pointer *Reported
	return errors.As(err, &pointer) && pointer != nil
}

// Op is one step that starts and then finishes.
//
// Label is the present-tense line a live renderer animates; Plain is the line
// the plain transcript prints when the step starts (empty means the step is
// silent until it finishes, which is what box did for a fast apply). Budget is
// non-zero for a timed wait, where a live renderer shows elapsed against it.
//
// Name makes the step a status row of its own: the finish line is a Row named
// Name whose facts are the duration and whatever the caller passes to Done,
// instead of the one free sentence Done used to print. Source is the newest
// line of the thing being waited on -- the last line of a Job's log, say --
// sampled about once a second: a live renderer shows it beside the running
// line, and the run log gets each distinct line once.
type Op struct {
	Label  string
	Plain  string
	Budget time.Duration
	// Name is the row the finish line is drawn as. Empty keeps the old
	// behaviour: Done prints its text as one line.
	Name string
	// Source returns the newest line of what this step waits on, or "" when
	// there is nothing to say yet. It is called about once a second.
	Source func() string
}

// Handle is a started step. A renderer that draws a spinner stops it on Done
// or Failed; the plain renderer has already printed Op.Plain and prints the
// finish line here.
type Handle interface {
	// Detail is a muted note beside the finish line.
	Detail(text string)
	// Done finishes the step with one line. An empty text only clears a live
	// renderer's spinner.
	Done(text string)
	// DoneAs finishes with different words per renderer: plain keeps the
	// exact transcript, pretty may drop a planning glyph the spinner's check
	// already carries.
	DoneAs(plain, pretty string)
	// Failed stops the step without printing the error; the caller returns it
	// and the command renders it once, at the end.
	Failed(err error)
}

// Reporter is every line box writes while it works.
type Reporter interface {
	// Phase opens a section: "cluster", "platform", "apply".
	Phase(name string)
	// Fact is one key and its value.
	Fact(key, value string)
	// List is a titled list of plain rows ("" omits the title).
	List(title string, rows []string)
	// Receipt is the wiring receipt: the overall status and one row per step.
	// Pending rows wait on a person, so a live renderer may print them after
	// the final card instead of in place.
	Receipt(status string, rows []ReceiptRow)
	// Plan is a titled list of marked items ("" omits the title). An item's
	// Children are printed under it, indented two columns past where the
	// item's own text starts, with their own marks.
	Plan(title string, items []Item)
	// Table is a titled block of rows laid out in columns ("" omits the
	// title): a glyph, a name, and the row's facts joined with a middle dot in
	// the column after the names. The name column is as wide as the longest
	// name in the block, so one block reads as a table and two blocks do not
	// line up with each other.
	Table(title string, rows []TableRow)
	// Group is a titled table whose facts read down columns: the column
	// headings over them, one row per thing, and the last column as the one
	// that carries the words. A group with no headings is a list of rows whose
	// facts are one sentence each.
	Group(g Group)
	// Note is one line of prose under a block, indented to the block's own
	// column.
	Note(text string)
	// Rows is a block of status lines: a mark in the two-column gutter, a
	// name in the fixed name column, an optional state word, and the facts
	// joined with a middle dot. Text that does not fit hangs under itself.
	Rows(rows []Row)
	// Detail is a line for the run log alone: object names, revisions,
	// digests, and the reasoning a person does not need on screen. A reporter
	// with no log drops it.
	Detail(text string)
	// Diff is a values diff: marked items with no title, printed flush left by
	// the plain transcript, which is how `box apply` has always printed them.
	Diff(items []Item)
	// Line is one line of box's own prose.
	Line(text string)
	// Muted is a line of explanation.
	Muted(text string)
	// Warn is a line that is a warning.
	Warn(text string)
	// Success is a line that reports something finished well.
	Success(text string)
	// Chart names the release box pulled.
	Chart(name, version, ref string)
	// Created reports the directories init made.
	Created(paths []string)
	// Start begins a step.
	Start(op Op) Handle
	// Confirm asks one yes/no question and leaves a record of the answer.
	Confirm(question string) (bool, error)
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

// Plain is the transcript: one line per event, no cursor movement, no colour.
// Its output is the contract the e2e scripts and the unit tests read, so its
// wording is the wording box has always used.
type Plain struct {
	W   io.Writer
	Err io.Writer
	// In answers Confirm. Yes answers every question, which is what --yes and
	// an unattended run mean.
	In  io.Reader
	Yes bool
	// Log is the run log: the same lines as the screen, plus the Detail lines,
	// with a timestamp each. The writer behind it does the timestamping; nil
	// is a run with no log.
	Log io.Writer
	// LogPath is that file, for the one line a failed run ends with. Empty
	// with no log.
	LogPath string
	// Now is the clock a step's duration is measured with. Nil is time.Now.
	Now func() time.Time
	// Sample is how often a waiting step's source is read. Zero is a second.
	Sample time.Duration

	reader *bufio.Reader
}

func (p *Plain) out() io.Writer {
	w := p.W
	if w == nil {
		w = io.Discard
	}
	// A caller may hand the same writer for both -- a test reads the transcript
	// the way a log reads -- and then every line is written once.
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

// Phase is a heading: a lowercase noun on a line of its own, with nothing after
// it (rule 9). A live renderer draws the same name in its masthead.
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

// Receipt is the transcript's receipt: the same lines box has always printed,
// with a pending row in place (a live renderer moves it after the card).
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

// The plain transcript's plan columns: an item's mark sits two columns in, and
// a child's two columns past where the item's own text starts -- that is, past
// the mark and the space after it. Spaces only: this rendering is what screen
// readers and logged runs read, so there is nothing else to draw with.
const (
	itemIndent = "  "
	// childIndent is an item's mark and space, then the two columns a child is
	// indented by.
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

// Table is the transcript's table: two columns, the name padded to the widest
// name in the block, and the facts after it joined with a middle dot. Nothing
// is wrapped: a logged run and a golden test read the same bytes whatever the
// terminal was.
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

// Rows is the transcript's status block: the mark in the gutter, the name in
// the fixed column, and the facts after it joined with a middle dot. Nothing is
// wrapped -- a logged run and a golden test read the same bytes whatever the
// terminal was -- but a row's why and its command still hang at its own text
// column, so the layout is the live one with the wrapping taken out.
func (p *Plain) Rows(rows []Row) {
	for _, row := range Block(rows) {
		fmt.Fprint(p.out(), PlainRow(row))
	}
}

// Block gives every row of a block the same name column: as wide as the widest
// name in the block, and never narrower than NameCol. A block is read down its
// column, so one long name widens the whole block rather than leaving one row's
// facts two columns off the others'. Rows that share a block pass through here
// once, in either renderer, so the transcript and the screen agree.
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

// Group is the transcript's columned block: the heading, the column headings
// over the facts they belong to, and one row per thing. The last column is the
// one that carries the words, so it wraps at Width under its own column; a
// list's facts are one sentence each and wrap the same way.
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

// groupList is a block whose rows are a sentence each: the mark two columns in,
// the name in the block's column, and the facts after it.
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
		hang := strings.Repeat(" ", len(head))
		for i, line := range Wrap(strings.Join(r.Facts, " · "), Width-len(head)) {
			if i == 0 {
				fmt.Fprintln(p.out(), head+line)
				continue
			}
			fmt.Fprintln(p.out(), hang+line)
		}
	}
}

// groupTable is a block with columns: the heading is the first column's, so the
// column headings line up over the facts, and every column but the last is
// padded to the widest fact in it.
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
		hang := strings.Repeat(" ", len(line))
		for i, wrapped := range Wrap(r.Facts[len(r.Facts)-1], Width-len(line)) {
			if i == 0 {
				fmt.Fprintln(p.out(), line+wrapped)
				continue
			}
			fmt.Fprintln(p.out(), hang+wrapped)
		}
	}
}

// Note is the transcript's line of prose under a block: two columns in, so it
// belongs to the block above it, and wrapped with the continuation hanging
// under the text.
func (p *Plain) Note(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for i, line := range Wrap(text, Width-2) {
		if i == 0 {
			fmt.Fprintln(p.out(), "  "+line)
			continue
		}
		fmt.Fprintln(p.out(), "    "+line)
	}
}

// columnWidths are the widths of a group's leading columns: as wide as the
// widest fact in each, so the headings sit over the data they name.
func columnWidths(g Group) []int {
	cols := make([]int, len(g.Columns))
	for _, r := range g.Rows {
		if r.Glyph != "" {
			continue
		}
		for i := range g.Columns {
			if i+1 < len(r.Facts) && len(r.Facts[i]) > cols[i] {
				cols[i] = len(r.Facts[i])
			}
		}
	}
	return cols
}

// blockNameWidth is a block's name column: as wide as its longest name.
func blockNameWidth(rows []TableRow) int {
	w := 0
	for _, r := range rows {
		if n := len(r.Name); n > w {
			w = n
		}
	}
	return w
}

// PlainRow lays one row out as the transcript prints it, and as the run log
// keeps it: the two never differ. A row is one line per fact -- the facts, then
// the why, then the command, each hanging under the row's own text column -- and
// nothing is wrapped. A block that carries sentences (Group, Note) wraps at
// Width; a status row is what a person scans, and a golden test reads the same
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

// Fold replaces rows that differ only by their name with one row, because a
// column of identical rows is one fact and its size (rules 3 and 4): the group
// gets Name, its first fact gains "for N unit", and the rest of the facts are
// the ones they all shared. Rows that differ in anything else -- a mark, a
// state, a reason, a command -- are returned as they are, in their own order.
//
// A single row is returned unchanged: a fold of one is the row itself.
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

// plural is "1 host" and "2 hosts": the regular English ending, which is every
// unit box counts with.
func plural(n int, one string) string {
	if n == 1 {
		return one
	}
	return one + "s"
}

// Prefix is the gutter and the name columns of a row, the mark drawn by the
// renderer: one mark, the name padded to the fixed column, and the state, if
// any. Everything after it is the row's text.
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

// nameWidth is the name column: the block's, when Block set one, and otherwise
// this row's own -- fixed, unless its name is wider and takes the column with
// it, because a truncated name is a different name.
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

// columns is a string's width in columns: box writes no double-width glyphs in
// a name or a fact, so counting runes is counting columns, and a mark that costs
// three bytes still costs one column.
func columns(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, width int) string {
	if n := width - columns(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// Width is the column box writes to when a terminal is not the one deciding:
// the plain transcript, the run log and the migration's report all read it, so
// a piped run and the file beside a snapshot break their lines in the same
// places.
const Width = 80

// Wrap breaks text into lines of at most width columns, never inside a word: a
// path, an address or a name that does not fit goes on a line of its own,
// whole, because half a path is a different path.
func Wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	out := []string{}
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= width {
			line += " " + w
			continue
		}
		out = append(out, line)
		line = w
	}
	return append(out, line)
}

// WrapShell breaks a command the way a shell does: at the spaces, with a
// trailing `\` on every line but the last, so a pasted command runs as it
// stands.
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
		if len(line)+1+len(w) <= width {
			line += " " + w
			continue
		}
		out = append(out, line+" \\")
		line = "    " + w
	}
	return append(out, line)
}

// Duration is a span of time as a person reads it: 41s, 6s, 4m10s, 10m. The
// zeros are left out and nothing is left dangling, so elapsed and limit read
// the same way in `4m10s of 10m`.
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

// Watcher reads a waiting step's source and says which sampled lines are new.
// The dedupe is one rule for both renderers: a source that repeats its last
// line for four minutes adds nothing to the log.
type Watcher struct {
	// Source returns the newest line of what is being waited on.
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

// Warn is a line that needs a person: the `!` mark in the gutter, in both
// renderers (rule 1). A line that is only worth knowing is a note, and a note is
// a `·` row.
func (p *Plain) Warn(text string) { fmt.Fprintln(p.out(), MarkNeeds+" "+text) }

// Success is something that finished well: the ✓ mark in the gutter, in both
// renderers.
func (p *Plain) Success(text string)   { fmt.Fprintln(p.out(), MarkDone+" "+text) }
func (p *Plain) Banner(string, string) {}

// Chart names the release box pulled: the name and version on the screen, the
// reference in the log, where plumbing belongs (rule 5).
func (p *Plain) Chart(name, version, ref string) {
	fmt.Fprint(p.out(), PlainRow(Row{Mark: MarkDone, Name: "chart", Facts: []string{name + " " + version}}))
	p.Detail("chart " + name + " " + version + " (" + ref + ")")
}

// Created is one row: how many directories this run made. The paths are the
// log's -- a person wants to know that init made them, not which ones.
func (p *Plain) Created(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprint(p.out(), PlainRow(Row{Mark: MarkDone, Name: "directories", Facts: []string{fmt.Sprintf("%d made", len(paths))}}))
	for _, path := range paths {
		p.Detail("made " + path)
	}
}

// Start prints the step's plain start line, if it has one, and returns a handle
// that prints its finish.
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

// stopSampling ends the sampler and waits for it, so the step's finish line is
// written after the last sampled one: a log written while the process ends is
// a log that lost its tail.
func (h *plainHandle) stopSampling() {
	if h.stop == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.stopped
}

func (h *plainHandle) Detail(string) {}

// Done prints a named step's finish as the row it is: what it did, and how long
// it took. An unnamed step keeps the sentence it always printed.
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
// reporter draws a form for, answered on the transcript. It names Confirm
// because that is what the plain path asks; the flows call Ask.
func (p *Plain) Ask(question string) (bool, error) { return p.Confirm(question) }

// Confirm is one yes/no question. --yes answers it, and there is no answer
// without a reader: a prompt that cannot be shown is a refusal rather than an
// implicit yes, because box writes to a cluster as root, so "nobody was there"
// must never mean "go ahead". cmd/box only gives a Plain a reader when stdin is
// a terminal -- a character device is not enough, because /dev/null is one and
// a question on it fails with an EOF nobody typed.
func (p *Plain) Confirm(question string) (bool, error) {
	if p.Yes {
		return true, nil
	}
	if p.In == nil {
		return false, errors.New("not a terminal: pass --yes to apply without a prompt")
	}
	if p.reader == nil {
		p.reader = bufio.NewReader(p.In)
	}
	// The question mark in the gutter, then the question as the flow wrote it:
	// one mark per line, and a question is a mark of its own (rule 1).
	fmt.Fprintf(p.out(), "%s %s [y/N] ", MarkAsk, question)
	line, err := p.reader.ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("read the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Error is the one failure line, in box's voice. The plain path keeps it on
// stderr exactly as cmd/box always printed it.
// Error is the one failure line, in box's voice: a ✗ row, its reason hanging
// under it, and the run log's path last. A plain path keeps it on stderr
// exactly as cmd/box always printed it.
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
