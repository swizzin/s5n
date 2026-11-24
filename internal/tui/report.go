package tui

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/theme"
)

// ReporterOptions is one live reporter: the terminal, whether colour is
// allowed, and the answer --yes already gave.
type ReporterOptions struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
	// Width is the terminal's columns, as cmd/box measured it. Zero is the
	// narrowest width box supports.
	Width int
	// NoColor keeps the layout and drops the colours (NO_COLOR).
	NoColor bool
	// Theme names the palette this run draws with; empty is s5n. The name is
	// cmd/box's to read -- the values' `theme:` key -- and theme.Parse is what
	// decides whether it is one. A name that does not resolve is reported by
	// Err and by the first question asked.
	Theme string
	// Background paints the terminal's background with that palette's, for the
	// whole run. Off by default; see Options.Background.
	Background bool
	// Yes answers every confirm without a form, and leaves the same ✓ record
	// line an answered form would.
	Yes bool
	// Log is the run log: the same finished lines as the screen, plus the
	// Detail lines, with a timestamp each. The writer behind it does the
	// timestamping and the redaction; nil is a run with no log.
	Log io.Writer
	// LogPath is that file, for the one line a failed run ends with.
	LogPath string
	// Now is the clock a step's duration is measured with, and Sample how
	// often a waiting step's source is read. Zero is time.Now and a second.
	Now    func() time.Time
	Sample time.Duration
}

// indent is where every line the reporter prints under a phase frame starts.
// It is the same two columns huh's fields and the setup record lines use, so a
// whole run reads as one block; the frames themselves stay at column 0, like
// the masthead.
const indent = "  "

// Reporter is say.Reporter in box's own dress: the masthead frame for a phase,
// a spinner for the one step that is running, a ✓ line left in scrollback for
// each finished step, plans with coloured marks, and the same huh confirm init
// asks with.
//
// Exactly one line animates. Everything finished is written once and stays in
// scrollback, so a long bootstrap is still reviewable by scrolling back.
type Reporter struct {
	out    io.Writer
	errOut io.Writer
	width  int

	chrome chrome
	ui     *UI
	// yes answers every confirm without a form (--yes).
	yes bool
	// space is the empty line before every masthead. It is the UI's, because
	// the sections this reporter draws and the questions that UI asks are one
	// run and must not stack two gaps between them.
	space *space

	// log is the run log, nil for a run that does not keep one.
	log     io.Writer
	logPath string
	// now and sample are the clock and the sampling interval; the zero values
	// mean time.Now and a second.
	now    func() time.Time
	sample time.Duration

	// pending is the wiring receipt's rows that wait on a person. They are
	// printed after the success card, because they are what to do next.
	pending []say.ReceiptRow

	mu     sync.Mutex
	active *live
	closed bool

	sig     chan os.Signal
	sigDone chan struct{}
}

// NewReporter builds the live reporter. In and Out default to the process's
// own terminal, which is also the only case where it installs the Ctrl-C
// handler: a reporter drawn on a buffer has no cursor to clean up.
func NewReporter(o ReporterOptions) *Reporter {
	in, out, errOut := o.In, o.Out, o.Err
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	if o.Width <= 0 {
		o.Width = defaultWidth
	}
	ui := New(Options{In: in, Out: out, NoColor: o.NoColor, Width: o.Width, Theme: o.Theme, Background: o.Background})
	r := &Reporter{
		out:     out,
		errOut:  errOut,
		width:   o.Width,
		chrome:  chrome{noColor: o.NoColor},
		ui:      ui,
		space:   ui.space,
		yes:     o.Yes,
		closed:  false,
		log:     o.Log,
		logPath: o.LogPath,
		now:     o.Now,
		sample:  o.Sample,
	}
	if o.In == nil && o.Out == nil {
		r.watchInterrupt()
	}
	return r
}

// watchInterrupt stops the spinner and leaves the cursor on a fresh line when
// the operator presses Ctrl-C. box has written nothing from the reporter, so
// there is nothing to roll back here; the commands' own defer Close does the
// same on every normal error path.
func (r *Reporter) watchInterrupt() {
	r.sig = make(chan os.Signal, 1)
	r.sigDone = make(chan struct{})
	signal.Notify(r.sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-r.sig:
			r.interrupted()
		case <-r.sigDone:
		}
	}()
}

// exit is os.Exit, replaced in tests: the interrupt path has to put the
// terminal back before it goes, and nothing else can run after it.
var exit = os.Exit

// interrupted is the Ctrl-C and SIGTERM path: stop the spinner, leave the
// cursor on a fresh line, put the terminal's background back and exit. A
// deferred Close does not run through os.Exit, so the paint is undone by hand
// here; the deferred one still covers a return, an error and a panic.
func (r *Reporter) interrupted() {
	r.mu.Lock()
	if r.active != nil {
		r.clearLocked()
		r.active = nil
	}
	_, _ = io.WriteString(r.out, "\n")
	r.mu.Unlock()
	r.Close()
	exit(130)
}

var _ say.Reporter = (*Reporter)(nil)

// Palette is the variant this run draws with.
func (r *Reporter) Palette() theme.Variant { return r.ui.Palette() }

// Err is a theme name that did not resolve, or nil.
func (r *Reporter) Err() error { return r.ui.Err() }

func (r *Reporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Reporter) interval() time.Duration {
	if r.sample > 0 {
		return r.sample
	}
	return time.Second
}

// Detail is a line for the run log alone: nothing about it reaches the screen.
func (r *Reporter) Detail(text string) {
	if r.log == nil || strings.TrimSpace(text) == "" {
		return
	}
	_, _ = io.WriteString(r.log, text+"\n")
}

// logged copies a finished block to the run log. The live line is never copied:
// it is redrawn many times a second, and the log keeps what the screen left in
// scrollback.
func (r *Reporter) logged(text string) {
	if r.log == nil || text == "" {
		return
	}
	_, _ = io.WriteString(r.log, text)
}

// SetYes makes every confirm answer itself, which is what --yes means on a
// terminal. It is set after the flags are parsed.
func (r *Reporter) SetYes(v bool) { r.yes = v }

// SetPhase names the section a following confirm belongs to without drawing a
// header. The setup questions' own masthead is their header, so cmd/box never
// draws a separate one for it.
func (r *Reporter) SetPhase(name string) {
	r.chrome.phase = name
	r.ui.chrome.phase = name
}

// Phase draws the masthead for a section. The same frame as init's step
// header, with the section name where "seedbox setup" would be, and the empty
// line above it that every masthead opens with.
func (r *Reporter) Phase(name string) {
	r.chrome.phase = name
	r.ui.chrome.phase = name
	r.stopActive()
	r.emit(r.space.outset() + r.chrome.phaseHead(name) + "\n\n")
}

// Fact is one key and its value: a muted column and an ink value, wrapped
// under itself so a long kubeconfig path never runs off the edge.
func (r *Reporter) Fact(key, value string) {
	const kw = 14
	k := fmt.Sprintf("%-*s", kw, key)
	var b strings.Builder
	cont := indent + strings.Repeat(" ", kw) + "  "
	for i, line := range wrapText(value, r.width-2-kw-2) {
		if i == 0 {
			fmt.Fprintf(&b, "%s%s  %s\n", indent, r.chrome.paint(k, mutedStyle.Render(k)), r.chrome.paint(line, inkStyle.Render(line)))
			continue
		}
		fmt.Fprintf(&b, "%s%s\n", cont, r.chrome.paint(line, inkStyle.Render(line)))
	}
	r.emit(b.String())
}

// List is a muted title and plain rows, indented and wrapped. An empty title
// keeps the indent: it is a detail block, not a heading.
func (r *Reporter) List(title string, rows []string) {
	var b strings.Builder
	if title != "" {
		b.WriteString(indent + r.chrome.paint(title, accentStyle.Bold(true).Render(title)) + "\n")
	}
	for _, row := range rows {
		for _, line := range wrapText(row, r.width-lipgloss.Width(indent)) {
			b.WriteString(indent + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
	}
	r.emit(b.String())
}

// Plan is the list box already prints before a confirm, with its marks
// coloured: + installs, ~ changes, ? depends on an answer, ! is a warning,
// = verifies and - removes. A component's sub-steps are drawn under it, two
// columns past its text, with marks of their own.
func (r *Reporter) Plan(title string, items []say.Item) {
	r.plan(title, items)
}

// Diff is a values diff: the same marks and the same indent. The plain
// transcript has always printed these flush left, and still does; a live
// renderer keeps its one block at box's two columns.
func (r *Reporter) Diff(items []say.Item) { r.plan("", items) }

// Table is a titled block of rows in two columns: the glyph in its status
// colour, the name in ink, and the facts joined with a middle dot and wrapped
// under themselves. The name column is as wide as the longest name that fits,
// so a block reads as a table and a host name that would push everything off
// the edge takes its own line.
func (r *Reporter) Table(title string, rows []say.TableRow) {
	if len(rows) == 0 {
		return
	}
	var b strings.Builder
	if title != "" {
		b.WriteString(r.chrome.paint(title, accentStyle.Bold(true).Render(title)) + "\n")
	}
	nameWidth := 0
	for _, row := range rows {
		if w := lipgloss.Width(row.Name); w > nameWidth {
			nameWidth = w
		}
	}
	const nameMax = 24
	if nameWidth > nameMax {
		nameWidth = nameMax
	}
	for _, row := range rows {
		glyph := row.Glyph
		if glyph == "" {
			glyph = "·"
		}
		facts := strings.Join(row.Facts, " · ")
		prefix := indent + glyph + " "
		if row.Name == "" {
			for i, line := range wrapText(facts, r.width-lipgloss.Width(prefix)) {
				if i == 0 {
					b.WriteString(indent + r.chrome.paint(glyph, glyphStyle(glyph).Render(glyph)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
					continue
				}
				b.WriteString(strings.Repeat(" ", lipgloss.Width(prefix)) + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
			}
			continue
		}
		// A name longer than the column keeps its line to itself, with the
		// facts under it: a table whose names push the facts off the edge is
		// not a table any more.
		if lipgloss.Width(row.Name) > nameWidth {
			b.WriteString(indent + r.chrome.paint(glyph, glyphStyle(glyph).Render(glyph)) + " " + r.chrome.paint(row.Name, inkStyle.Render(row.Name)) + "\n")
			pad := indent + "  "
			for _, line := range wrapText(facts, r.width-lipgloss.Width(pad)) {
				b.WriteString(pad + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
			}
			continue
		}
		pad := indent + strings.Repeat(" ", lipgloss.Width(glyph)+1+nameWidth+2)
		for i, line := range wrapText(facts, r.width-lipgloss.Width(pad)) {
			if i == 0 {
				b.WriteString(indent + r.chrome.paint(glyph, glyphStyle(glyph).Render(glyph)) + " ")
				b.WriteString(r.chrome.paint(fmt.Sprintf("%-*s", nameWidth, row.Name), inkStyle.Render(row.Name)))
				b.WriteString("  " + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(pad + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
		}
	}
	r.emit(b.String())
}

// Rows is a block of status lines: the mark in the gutter, the name in the
// fixed column, and the facts after it, wrapped under the row's own text column
// so the gutter and the columns stay columns. It sits flush left, like the
// masthead: a status block is the run's frame, not its content.
func (r *Reporter) Rows(rows []say.Row) {
	if len(rows) == 0 {
		return
	}
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(r.rowBlock(row))
	}
	r.emit(b.String())
}

// rowBlock draws one row: the mark and the columns, then the facts wrapped under
// them, then the why and the command at the same column. A command is broken the
// way a shell would break it, never inside a token.
func (r *Reporter) rowBlock(row say.Row) string {
	mark := row.Mark
	if mark == "" {
		mark = say.MarkNote
	}
	prefix := row.Prefix(mark)
	col := row.TextCol()
	avail := r.width - col
	if avail < 8 {
		avail = 8
	}
	paintedMark := r.chrome.paint(mark, glyphStyle(mark).Render(mark))
	head := paintedMark + prefix[len(mark):]
	var b strings.Builder
	// A command breaks the way a shell breaks one; nothing else does.
	lines := wrapText(row.Text(), avail)
	cont := col
	if mark == say.MarkCommand {
		lines = wrapShell(row.Text(), avail)
		cont = col + len(say.MarkCommand) + 1
	}
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		b.WriteString(strings.TrimRight(head, " ") + "\n")
	} else {
		for i, line := range lines {
			text := r.chrome.paint(line, inkStyle.Render(line))
			if i == 0 {
				if row.Text() == "" {
					b.WriteString(strings.TrimRight(head, " ") + text + "\n")
					continue
				}
				b.WriteString(head + text + "\n")
				continue
			}
			b.WriteString(strings.Repeat(" ", cont) + text + "\n")
		}
	}
	hang := strings.Repeat(" ", col)
	if row.Why != "" {
		for _, line := range wrapText(row.Why, avail) {
			b.WriteString(hang + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
		}
	}
	if row.Command != "" {
		for i, line := range wrapShell(row.Command, avail-2) {
			if i == 0 {
				b.WriteString(hang + r.chrome.paint(say.MarkCommand, glyphStyle(say.MarkCommand).Render(say.MarkCommand)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(hang + "  " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
	}
	return b.String()
}

func (r *Reporter) plan(title string, items []say.Item) {
	var b strings.Builder
	if title != "" {
		b.WriteString(indent + r.chrome.paint(title, accentStyle.Bold(true).Render(title)) + "\n")
	}
	for _, it := range items {
		r.planItem(&b, indent, it)
		for _, child := range it.Children {
			// A child's mark sits past the item's own text: its mark and a
			// space, then the two columns the indent is.
			r.planItem(&b, indent+"    ", child)
		}
	}
	r.emit(b.String())
}

// planItem draws one row of a plan at level: its mark coloured by what it does,
// its text wrapped under itself. level is where the mark goes, so a child sits
// two columns past the text of the row above it.
func (r *Reporter) planItem(b *strings.Builder, level string, it say.Item) {
	mark := string(it.Mark)
	if mark == "" {
		mark = "·"
	}
	prefix := level + mark + " "
	cont := strings.Repeat(" ", lipgloss.Width(prefix))
	style := r.planStyle(it.Mark)
	for i, line := range wrapText(it.Text, r.width-lipgloss.Width(prefix)) {
		if i == 0 {
			b.WriteString(level + r.chrome.paint(mark, style.Render(mark)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
			continue
		}
		b.WriteString(cont + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
	}
}

// planStyle colours a plan mark by what it does to the cluster.
func (r *Reporter) planStyle(m say.Mark) lipgloss.Style {
	switch m {
	case say.Install, say.Verify:
		return goodStyle
	case say.Change:
		return accentStyle
	case say.Warning, say.Remove:
		return badStyle
	case say.Question:
		return mutedStyle
	}
	return inkStyle
}

// glyphStyle is the one rule for a status glyph: ✓ is accent, ✗ and ! are bad,
// ◌ waits on a person and is accent too, · is a skip and muted. Every place a
// glyph is drawn goes through here, so one run never shows a check in two
// colours.
func glyphStyle(g string) lipgloss.Style {
	switch g {
	case "✓", "◌", "◐":
		return accentStyle
	case "✗", "!", "⚠":
		return badStyle
	case "·", "?":
		return mutedStyle
	}
	return inkStyle
}

// Line is one line of box's prose, wrapped with its indent kept.
func (r *Reporter) Line(text string) { r.block(text, "", inkStyle, inkStyle) }

// Muted is explanation.
func (r *Reporter) Muted(text string) { r.block(text, "", mutedStyle, mutedStyle) }

// Warn is a line that is a warning: marked and painted bad, wrapped.
func (r *Reporter) Warn(text string) { r.block(text, "⚠ ", badStyle, badStyle) }

// Success is something that finished well: one accent ✓, then the words.
func (r *Reporter) Success(text string) { r.block(text, "✓ ", glyphStyle("✓"), inkStyle) }

func (r *Reporter) block(text, mark string, markStyle, textStyle lipgloss.Style) {
	prefixPlain := indent + mark
	cont := strings.Repeat(" ", lipgloss.Width(prefixPlain))
	var b strings.Builder
	for i, line := range wrapText(text, r.width-lipgloss.Width(prefixPlain)) {
		if i == 0 {
			b.WriteString(indent)
			if mark != "" {
				b.WriteString(r.chrome.paint(mark, markStyle.Render(mark)))
			}
			b.WriteString(r.chrome.paint(line, textStyle.Render(line)) + "\n")
			continue
		}
		b.WriteString(cont + r.chrome.paint(line, textStyle.Render(line)) + "\n")
	}
	r.emit(b.String())
}

// Chart names the release box pulled. The reference is the muted detail; the
// name and version are what a person needs.
func (r *Reporter) Chart(name, version, ref string) {
	mark := r.chrome.paint("✓", glyphStyle("✓").Render("✓"))
	head := indent + mark + " chart " + name + " " + version
	tail := " (" + ref + ")"
	if lipgloss.Width(head+tail) <= r.width {
		r.emit(head + r.chrome.paint(tail, mutedStyle.Render(tail)) + "\n")
		return
	}
	var b strings.Builder
	b.WriteString(head + "\n")
	for _, line := range wrapText("("+ref+")", r.width-4) {
		b.WriteString("    " + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
	}
	r.emit(b.String())
}

// Created reports the directories init made. A live renderer says how many,
// under which root, and lists the names muted below, wrapped between names and
// never inside one.
func (r *Reporter) Created(paths []string) {
	if len(paths) == 0 {
		return
	}
	if len(paths) == 1 {
		r.Success("created " + paths[0])
		return
	}
	base := commonDir(paths)
	if base == "" {
		base = "/"
	}
	summary := fmt.Sprintf("created %d directories under %s", len(paths), base)
	var b strings.Builder
	prefix := indent + "✓ "
	cont := strings.Repeat(" ", lipgloss.Width(prefix))
	for i, line := range wrapText(summary, r.width-lipgloss.Width(prefix)) {
		if i == 0 {
			b.WriteString(indent + r.chrome.paint("✓", glyphStyle("✓").Render("✓")) + " ")
			b.WriteString(r.chrome.paint(line, inkStyle.Render(line)) + "\n")
			continue
		}
		b.WriteString(cont + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
	}
	var names []string
	for _, p := range paths {
		name := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
		if name != "" {
			names = append(names, name)
		}
	}
	detail := indent + "  "
	for _, raw := range wrapList(names, " · ", r.width-lipgloss.Width(detail)) {
		b.WriteString(detail + r.chrome.paint(raw, mutedStyle.Render(raw)) + "\n")
	}
	r.emit(b.String())
}

// commonDir is the deepest directory every path shares.
func commonDir(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	parts := strings.Split(filepath.Clean(paths[0]), string(filepath.Separator))
	for _, p := range paths[1:] {
		other := strings.Split(filepath.Clean(p), string(filepath.Separator))
		n := 0
		for n < len(parts) && n < len(other) && parts[n] == other[n] {
			n++
		}
		parts = parts[:n]
	}
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, string(filepath.Separator))
	if out == "" {
		return string(filepath.Separator)
	}
	return out
}

// Receipt draws the wiring receipt: one status glyph per step, the name in ink
// and any reason muted under it. A row that is pending waits on a person, so
// it is held back for the card and printed as the "to finish" list: it is what
// to do next, not something that happened in the middle of the run.
func (r *Reporter) Receipt(status string, rows []say.ReceiptRow) {
	mark := "✓"
	if status != "ok" {
		mark = "✗"
	}
	var b strings.Builder
	b.WriteString(r.row(mark, "wiring "+status, "", boldInkStyle))
	var pending []say.ReceiptRow
	for _, row := range rows {
		switch row.Result {
		case "pending":
			pending = append(pending, row)
		case "failed":
			b.WriteString(r.row("✗", row.Name, row.Detail, inkStyle))
		case "skipped":
			b.WriteString(r.row("·", row.Name, row.Detail, inkStyle))
		default:
			b.WriteString(r.row("✓", row.Name, row.Detail, inkStyle))
		}
	}
	r.pending = pending
	r.emit(b.String())
	if status != "ok" {
		r.flushPending()
	}
}

// row is one status line: a glyph in its status colour, a name, and a detail
// muted beside it or wrapped under it. Nothing is ever flush left.
func (r *Reporter) row(mark, name, detail string, nameStyle lipgloss.Style) string {
	prefix := indent + mark + " "
	cont := strings.Repeat(" ", lipgloss.Width(prefix))
	detailPad := cont + "  "
	avail := r.width - lipgloss.Width(prefix)
	if avail < 8 {
		avail = 8
	}
	nameLines := wrapText(name, avail)
	var b strings.Builder
	for i, line := range nameLines {
		if i == 0 {
			b.WriteString(indent + r.chrome.paint(mark, glyphStyle(mark).Render(mark)) + " ")
		} else {
			b.WriteString(cont)
		}
		b.WriteString(r.chrome.paint(line, nameStyle.Render(line)))
		if len(nameLines) == 1 && detail != "" && lipgloss.Width(name)+2+lipgloss.Width(detail) <= avail {
			b.WriteString("  " + r.chrome.paint(detail, mutedStyle.Render(detail)) + "\n")
			return b.String()
		}
		b.WriteString("\n")
	}
	if detail != "" {
		w := r.width - lipgloss.Width(detailPad)
		if w < 8 {
			w = 8
		}
		for _, line := range wrapText(detail, w) {
			b.WriteString(detailPad + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
		}
	}
	return b.String()
}

// flushPending prints the rows the receipt held back, under a short heading.
// The card is drawn first: these are the next things to do, so they belong
// after it and not wedged into the receipt.
func (r *Reporter) flushPending() {
	if len(r.pending) == 0 {
		return
	}
	rows := r.pending
	r.pending = nil
	var b strings.Builder
	b.WriteString(indent + r.chrome.paint("to finish", accentStyle.Bold(true).Render("to finish")) + "\n")
	for _, row := range rows {
		b.WriteString(r.row("◌", row.Name, row.Detail, inkStyle))
	}
	r.emit(b.String())
}

// Start begins a step. The previous one, if any, is cleared: box runs one
// operation at a time and a second spinner would fight the first for the line.
func (r *Reporter) Start(op say.Op) say.Handle {
	r.stopActive()
	if op.Label == "" || r.closed {
		return noHandle{}
	}
	l := &live{label: op.Label, budget: op.Budget, start: r.clock(), done: make(chan struct{}), watch: say.Watcher{Source: op.Source}}
	r.mu.Lock()
	r.active = l
	r.drawLocked()
	r.mu.Unlock()
	go r.animate(l)
	return &liveHandle{r: r, l: l, op: op}
}

func (r *Reporter) animate(l *live) {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	next := r.clock()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			// The source is read about once a second, not once a frame:
			// it is somebody's log, and the line only has to keep up.
			l.mu.Lock()
			due := l.watch.Source != nil && !r.clock().Before(next)
			var tail string
			if due {
				if line, ok := l.watch.Next(); ok {
					tail = line
				}
				next = r.clock().Add(r.interval())
			}
			if tail != "" {
				l.tail = tail
				r.Detail(tail)
			}
			l.mu.Unlock()
			r.mu.Lock()
			if r.active != l {
				r.mu.Unlock()
				return
			}
			l.frame++
			r.drawLocked()
			r.mu.Unlock()
		}
	}
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// drawLocked paints the active line in place. The caller holds r.mu. The line
// is what a wait is never silent about: what is happening, how long it has
// taken against its limit, and the newest line of the thing being waited on.
func (r *Reporter) drawLocked() {
	l := r.active
	if l == nil {
		return
	}
	frame := spinnerFrames[l.frame%len(spinnerFrames)]
	counter := ""
	if l.budget > 0 {
		counter = say.Duration(r.clock().Sub(l.start)) + " of " + say.Duration(l.budget)
	}
	l.mu.Lock()
	tail := l.tail
	l.mu.Unlock()
	prefixWidth := lipgloss.Width(indent + frame + " ")
	room := r.width - prefixWidth - lipgloss.Width(counter) - 2
	if room < 8 {
		room = 8
	}
	tailWidth := 0
	if tail != "" {
		// The newest line of the wait is the detail: give it half of
		// what the label does not need, so neither one is squeezed out.
		tailWidth = room / 2
		room = room - tailWidth - 2
		if room < 8 {
			room = 8
		}
		detail := clip(tail, tailWidth)
		tail = detail
	}
	label := clip(l.label, room)
	line := indent + r.chrome.paint(frame, accentStyle.Render(frame)) + " " + r.chrome.paint(label, inkStyle.Render(label))
	if counter != "" {
		line += "  " + r.chrome.paint(counter, mutedStyle.Render(counter))
	}
	if tail != "" {
		line += "  " + r.chrome.paint(tail, mutedStyle.Render(tail))
	}
	fmt.Fprint(r.out, "\r\x1b[2K"+line)
}

func (r *Reporter) clearLocked() {
	if r.active != nil {
		fmt.Fprint(r.out, "\r\x1b[2K")
		r.space.cleared()
	}
}

// stopActive finishes the live line silently. Nothing finished is erased: the
// caller writes the ✓ record immediately after.
func (r *Reporter) stopActive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.active
	if l == nil {
		return
	}
	r.clearLocked()
	r.active = nil
	close(l.done)
}

// emit writes finished output, then restores the active spinner under it.
func (r *Reporter) emit(text string) {
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil {
		r.clearLocked()
	}
	_, _ = io.WriteString(r.out, text)
	r.logged(text)
	r.space.wrote(text)
	if r.active != nil {
		r.drawLocked()
	}
}

type live struct {
	mu     sync.Mutex
	label  string
	budget time.Duration
	start  time.Time
	frame  int
	detail string
	// tail is the newest line of what this step waits on, as the live line
	// shows it.
	tail  string
	watch say.Watcher
	done  chan struct{}
}

type liveHandle struct {
	r  *Reporter
	l  *live
	op say.Op
}

func (h *liveHandle) Detail(text string) {
	h.l.mu.Lock()
	h.l.detail = text
	h.l.mu.Unlock()
}

func (h *liveHandle) Done(text string) {
	if h.op.Name == "" {
		h.finish("✓", text, h.detail())
		return
	}
	facts := []string{}
	if text != "" {
		facts = append(facts, text)
	}
	if d := h.detail(); d != "" {
		facts = append(facts, d)
	}
	facts = append(facts, say.Duration(h.r.clock().Sub(h.l.start)))
	h.finishRow("✓", say.Row{Name: h.op.Name, Facts: facts})
}

func (h *liveHandle) DoneAs(_, pretty string) { h.Done(pretty) }

func (h *liveHandle) Failed(err error) {
	// The error itself is rendered once by the command; the step line says
	// which step died.
	if h.op.Name == "" {
		h.finish("✗", h.l.label, "")
		return
	}
	h.finishRow("✗", say.Row{Name: h.op.Name})
}

// finishRow ends a named step as the row it is: a mark in the gutter, the
// step's name, and its facts.
func (h *liveHandle) finishRow(mark string, row say.Row) {
	r := h.r
	r.mu.Lock()
	if r.active == h.l {
		r.clearLocked()
		r.active = nil
	}
	select {
	case <-h.l.done:
	default:
		close(h.l.done)
	}
	row.Mark = mark
	text := r.rowBlock(row)
	_, _ = io.WriteString(r.out, text)
	r.logged(text)
	r.space.wrote(text)
	r.mu.Unlock()
}

func (h *liveHandle) detail() string {
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	return h.l.detail
}

func (h *liveHandle) finish(mark, text, detail string) {
	r := h.r
	r.mu.Lock()
	if r.active == h.l {
		r.clearLocked()
		r.active = nil
	}
	select {
	case <-h.l.done:
	default:
		close(h.l.done)
	}
	var b strings.Builder
	if text != "" {
		cont := indent + "  "
		plainPrefix := indent + mark + " "
		style := glyphStyle(mark)
		for i, line := range wrapText(text, r.width-lipgloss.Width(plainPrefix)) {
			if i == 0 {
				b.WriteString(indent + r.chrome.paint(mark, style.Render(mark)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(cont + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
		if detail != "" {
			for _, line := range wrapText(detail, r.width-lipgloss.Width(cont)) {
				b.WriteString(cont + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
			}
		}
	}
	_, _ = io.WriteString(r.out, b.String())
	r.logged(b.String())
	r.space.wrote(b.String())
	r.mu.Unlock()
}

type noHandle struct{}

func (noHandle) Detail(string)         {}
func (noHandle) Done(string)           {}
func (noHandle) DoneAs(string, string) {}
func (noHandle) Failed(error)          {}

// Confirm asks with the same huh confirm init uses, on no by default, and
// leaves the same ✓ record line a setup question does. --yes answers without a
// form and records the answer the same way.
func (r *Reporter) Confirm(question string) (bool, error) {
	r.stopActive()
	r.ui.chrome.phase = r.chrome.phase
	r.ui.chrome.question = ""
	r.ui.chrome.step, r.ui.chrome.total = 0, 0
	r.ui.chrome.noColor = r.chrome.noColor
	if r.yes {
		r.ui.record(question, "yes")
		return true, nil
	}
	return r.ui.Confirm(setup.Confirm{Prompt: question, Default: false})
}

// Ask is release.Confirmer and platform.Confirmer: the same question, the same
// huh form, the same record line.
func (r *Reporter) Ask(question string) (bool, error) { return r.Confirm(question) }

// Error is the end of a failed run: one ✗ row, its reason hanging under it, and
// the run log's path last. A failure is a row like any other -- there is no
// frame around it, and the reason is what the command can say, not a paragraph.
func (r *Reporter) Error(err error) {
	r.stopActive()
	if err == nil {
		return
	}
	r.flushPending()
	if say.ReportedBy(err) {
		r.logRow()
		return
	}
	if subject, ok := say.SubjectOf(err); ok {
		r.emit(r.rowBlock(say.Row{Mark: say.MarkFailed, Name: subject.Name, Facts: subject.Facts, Why: subject.Why}))
		r.logRow()
		return
	}
	var b strings.Builder
	for i, raw := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
		mark := ""
		if i == 0 {
			mark = say.MarkFailed
		}
		col := strings.Repeat(" ", 2)
		for j, line := range wrapKeepingBlanks(raw, lineRoom(mark+" ", r.width)) {
			if j == 0 {
				if mark == "" {
					b.WriteString(col + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
					continue
				}
				b.WriteString(r.chrome.paint(mark, glyphStyle(mark).Render(mark)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(col + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
		}
	}
	r.emit(b.String())
	r.logRow()
}

// logRow is the last line of a failed run: where the transcript is.
func (r *Reporter) logRow() {
	if r.logPath == "" {
		return
	}
	r.emit(r.rowBlock(say.Row{Mark: say.MarkNote, Name: "log", Facts: []string{r.logPath}}))
}

// lineRoom is how many columns a line has to work with, after its mark.
func lineRoom(mark string, width int) int {
	w := width - lipgloss.Width(mark)
	if w < 8 {
		return 8
	}
	return w
}

// wrapKeepingBlanks is wrapText that keeps a blank line a blank line: an
// error's empty line is a break between two facts, not a word to skip.
func wrapKeepingBlanks(s string, width int) []string {
	if strings.TrimSpace(s) == "" {
		return []string{""}
	}
	return wrapText(s, width)
}

// Banner closes a good run with the one fact worth repeating: the domain.
func (r *Reporter) Banner(title, detail string) {
	r.stopActive()
	width := mastheadWidth
	if r.width > 0 && r.width < width {
		width = r.width
	}
	inner := width - 4
	var b strings.Builder
	b.WriteString(r.chrome.paint("╭"+strings.Repeat("─", width-2)+"╮", mutedStyle.Render("╭"+strings.Repeat("─", width-2)+"╮")) + "\n")
	line := "✓ " + title
	pad := inner - lipgloss.Width(line)
	if pad < 0 {
		pad = 0
	}
	b.WriteString(r.chrome.paint("│ ", mutedStyle.Render("│ ")))
	b.WriteString(r.chrome.paint("✓", accentStyle.Render("✓")) + " " + r.chrome.paint(title, boldInkStyle.Render(title)))
	b.WriteString(strings.Repeat(" ", pad) + r.chrome.paint(" │", mutedStyle.Render(" │")) + "\n")
	if detail != "" {
		for _, l := range wrapText(detail, inner-2) {
			gap := inner - lipgloss.Width(l)
			if gap < 0 {
				gap = 0
			}
			b.WriteString(r.chrome.paint("│ ", mutedStyle.Render("│ ")) + r.chrome.paint(l, inkStyle.Render(l)) + strings.Repeat(" ", gap) + r.chrome.paint(" │", mutedStyle.Render(" │")) + "\n")
		}
	}
	b.WriteString(r.chrome.paint("╰"+strings.Repeat("─", width-2)+"╯", mutedStyle.Render("╰"+strings.Repeat("─", width-2)+"╯")) + "\n")
	r.emit(b.String())
	r.flushPending()
}

// Close stops the spinner and the interrupt handler and puts the terminal's
// background back if this run painted it. Safe to call twice, and deferred by
// cmd/box so a return, an error and a panic all restore the terminal.
func (r *Reporter) Close() {
	defer r.ui.Close()
	r.flushPending()
	r.stopActive()
	r.mu.Lock()
	already := r.closed
	r.closed = true
	r.mu.Unlock()
	if already {
		return
	}
	if r.sig != nil {
		signal.Stop(r.sig)
		select {
		case <-r.sigDone:
		default:
			close(r.sigDone)
		}
	}
}
