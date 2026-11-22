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

// Op is one step that starts and then finishes.
//
// Label is the present-tense line a live renderer animates; Plain is the line
// the plain transcript prints when the step starts (empty means the step is
// silent until it finishes, which is what box did for a fast apply). Budget is
// non-zero for a timed wait, where a live renderer shows elapsed against it.
type Op struct {
	Label  string
	Plain  string
	Budget time.Duration
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

	reader *bufio.Reader
}

func (p *Plain) out() io.Writer {
	if p.W == nil {
		return io.Discard
	}
	return p.W
}

func (p *Plain) Phase(string) {}

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

func (p *Plain) Line(text string)      { fmt.Fprintln(p.out(), text) }
func (p *Plain) Muted(text string)     { fmt.Fprintln(p.out(), text) }
func (p *Plain) Warn(text string)      { fmt.Fprintln(p.out(), text) }
func (p *Plain) Success(text string)   { fmt.Fprintln(p.out(), text) }
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
	return plainHandle{p: p}
}

type plainHandle struct{ p *Plain }

func (h plainHandle) Detail(string) {}
func (h plainHandle) Done(text string) {
	if text != "" {
		fmt.Fprintln(h.p.out(), text)
	}
}
func (h plainHandle) DoneAs(plain, _ string) { h.Done(plain) }
func (h plainHandle) Failed(error)           {}

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
func (p *Plain) Error(err error) {
	w := p.Err
	if w == nil {
		w = p.out()
	}
	fmt.Fprintf(w, "box: %v\n", err)
}

func (p *Plain) Close() {}
