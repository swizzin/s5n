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
	if p.Log != nil {
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
	nameWidth := 0
	for _, r := range rows {
		if w := len(r.Name); w > nameWidth {
			nameWidth = w
		}
	}
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
			fmt.Fprintf(p.out(), "  %s %-*s  %s\n", glyph, nameWidth, r.Name, facts)
		}
	}
}

// Rows is the transcript's status block: the mark in the gutter, the name in
// the fixed column, and the facts after it joined with a middle dot. Nothing is
// wrapped -- a logged run and a golden test read the same bytes whatever the
// terminal was -- but a row's why and its command still hang at its own text
// column, so the layout is the live one with the wrapping taken out.
func (p *Plain) Rows(rows []Row) {
	for _, row := range rows {
		fmt.Fprint(p.out(), PlainRow(row))
	}
}

// PlainRow lays one row out as the transcript prints it, and as the run log
// keeps it: the two never differ.
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

// nameWidth is the name column: fixed, unless one name is wider than it and
// takes the column with it -- a truncated name is a different name.
func (r Row) nameWidth() int {
	if w := len(r.Name); w > NameCol {
		return w
	}
	return NameCol
}

func (r Row) stateWidth() int {
	if w := len(r.State); w > StateCol {
		return w
	}
	return StateCol
}

func pad(s string, width int) string {
	if n := width - len(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
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

func (p *Plain) Chart(name, version, ref string) {
	fmt.Fprintf(p.out(), "chart: %s %s (%s)\n", name, version, ref)
}

func (p *Plain) Created(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(p.out(), "created %s\n", strings.Join(paths, ", "))
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
	fmt.Fprintf(p.out(), "%s [y/N] ", question)
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
