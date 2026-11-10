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
	// Yes answers every confirm without a form, and leaves the same ✓ record
	// line an answered form would.
	Yes bool
}

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
	ui := New(Options{In: in, Out: out, NoColor: o.NoColor, Width: o.Width})
	r := &Reporter{
		out:    out,
		errOut: errOut,
		width:  o.Width,
		chrome: chrome{noColor: o.NoColor},
		ui:     ui,
		yes:    o.Yes,
		closed: false,
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
			r.mu.Lock()
			if r.active != nil {
				r.clearLocked()
				r.active = nil
			}
			io.WriteString(r.out, "\n")
			r.mu.Unlock()
			os.Exit(130)
		case <-r.sigDone:
		}
	}()
}

var _ say.Reporter = (*Reporter)(nil)

// SetYes makes every confirm answer itself, which is what --yes means on a
// terminal. It is set after the flags are parsed.
func (r *Reporter) SetYes(v bool) { r.yes = v }

// Phase draws the masthead for a section. The same frame as init's step
// header, with the section name where "seedbox setup" would be.
func (r *Reporter) Phase(name string) {
	r.chrome.phase = name
	r.ui.chrome.phase = name
	r.stopActive()
	r.emit(r.chrome.phaseHead(name) + "\n\n")
}

// Fact is one key and its value: a muted column and an ink value, wrapped
// under itself so a long kubeconfig path never runs off the edge.
func (r *Reporter) Fact(key, value string) {
	const kw = 14
	k := fmt.Sprintf("%-*s", kw, key)
	var b strings.Builder
	for i, line := range wrapText(value, r.width-2-kw-2) {
		if i == 0 {
			fmt.Fprintf(&b, "  %s  %s\n", r.chrome.paint(k, mutedStyle.Render(k)), r.chrome.paint(line, inkStyle.Render(line)))
			continue
		}
		fmt.Fprintf(&b, "  %s  %s\n", strings.Repeat(" ", kw), r.chrome.paint(line, inkStyle.Render(line)))
	}
	r.emit(b.String())
}

// List is a muted title and plain rows, indented and wrapped. An empty title
// keeps the indent: it is a detail block, not a heading.
func (r *Reporter) List(title string, rows []string) {
	var b strings.Builder
	if title != "" {
		b.WriteString("  " + r.chrome.paint(title, accentStyle.Bold(true).Render(title)) + "\n")
	}
	for _, row := range rows {
		for i, line := range wrapText(row, r.width-2) {
			prefix := "  "
			if i > 0 {
				prefix = "  "
			}
			b.WriteString(prefix + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
	}
	r.emit(b.String())
}

// Plan is the list box already prints before a confirm, with its marks
// coloured: + installs, ~ changes, ? depends on an answer, ! is a warning,
// = verifies and - removes.
func (r *Reporter) Plan(title string, items []say.Item) {
	r.plan(title, items, 2)
}

// Diff is a values diff: the same marks, no title and no indent, which is what
// the plain transcript has always printed.
func (r *Reporter) Diff(items []say.Item) { r.plan("", items, 0) }

func (r *Reporter) plan(title string, items []say.Item, indent int) {
	var b strings.Builder
	pad := strings.Repeat(" ", indent)
	if title != "" {
		b.WriteString(pad + r.chrome.paint(title, accentStyle.Bold(true).Render(title)) + "\n")
	}
	for _, it := range items {
		mark := string(it.Mark)
		if mark == "" {
			mark = "·"
		}
		prefix := pad + mark + " "
		cont := strings.Repeat(" ", lipgloss.Width(prefix))
		style := r.markStyle(it.Mark)
		for i, line := range wrapText(it.Text, r.width-lipgloss.Width(prefix)) {
			if i == 0 {
				b.WriteString(pad + r.chrome.paint(mark, style.Render(mark)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(cont + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
	}
	r.emit(b.String())
}

func (r *Reporter) markStyle(m say.Mark) lipgloss.Style {
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

// Line is one line of box's prose, wrapped with its indent kept.
func (r *Reporter) Line(text string) {
	r.block("", text, inkStyle, "")
}

// Muted is explanation.
func (r *Reporter) Muted(text string) {
	r.block("", text, mutedStyle, "")
}

// Warn is a line that is a warning: marked red, wrapped.
func (r *Reporter) Warn(text string) {
	r.block("", text, badStyle, "⚠ ")
}

// Success is something that finished well.
func (r *Reporter) Success(text string) {
	r.block("", text, inkStyle, "✓ ")
}

func (r *Reporter) block(indent, text string, style lipgloss.Style, mark string) {
	const pad = "  "
	prefixPlain := pad + mark
	cont := strings.Repeat(" ", lipgloss.Width(prefixPlain))
	var b strings.Builder
	for i, line := range wrapText(text, r.width-lipgloss.Width(prefixPlain)) {
		if i == 0 {
			if mark != "" {
				b.WriteString(pad + r.chrome.paint(mark, style.Render(mark)))
			} else {
				b.WriteString(pad)
			}
			b.WriteString(r.chrome.paint(line, style.Render(line)) + "\n")
			continue
		}
		b.WriteString(cont + r.chrome.paint(line, style.Render(line)) + "\n")
	}
	r.emit(b.String())
}

// Chart names the release box pulled. The reference is the muted detail; the
// name and version are what a person needs.
func (r *Reporter) Chart(name, version, ref string) {
	mark := r.chrome.paint("✓", accentStyle.Render("✓"))
	head := "  " + mark + " chart " + name + " " + version
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

// Created reports the directories init made. A long comma list runs off the
// screen, so a live renderer says how many, under which root, and wraps the
// names in muted text below.
func (r *Reporter) Created(paths []string) {
	if len(paths) == 0 {
		return
	}
	if len(paths) == 1 {
		r.Success("created " + paths[0])
		return
	}
	base := commonDir(paths)
	noun := "directories"
	if base == "" {
		base = "/"
	}
	title := fmt.Sprintf("created %d %s under %s", len(paths), noun, base)
	var b strings.Builder
	line := title
	for _, raw := range wrapText(line, r.width-2) {
		b.WriteString("  " + r.chrome.paint(raw, inkStyle.Render(raw)) + "\n")
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimPrefix(strings.TrimPrefix(p, base), "/"))
	}
	for _, raw := range wrapText(strings.Join(names, " · "), r.width-4) {
		b.WriteString("    " + r.chrome.paint(raw, mutedStyle.Render(raw)) + "\n")
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

// Start begins a step. The previous one, if any, is cleared: box runs one
// operation at a time and a second spinner would fight the first for the line.
func (r *Reporter) Start(op say.Op) say.Handle {
	r.stopActive()
	if op.Label == "" || r.closed {
		return noHandle{}
	}
	l := &live{label: op.Label, budget: op.Budget, start: time.Now(), done: make(chan struct{})}
	r.mu.Lock()
	r.active = l
	r.drawLocked()
	r.mu.Unlock()
	go r.animate(l)
	return &liveHandle{r: r, l: l}
}

func (r *Reporter) animate(l *live) {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
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

// drawLocked paints the active line in place. The caller holds r.mu.
func (r *Reporter) drawLocked() {
	l := r.active
	if l == nil {
		return
	}
	frame := spinnerFrames[l.frame%len(spinnerFrames)]
	counter := ""
	if l.budget > 0 {
		counter = elapsedLabel(time.Since(l.start)) + " / " + durationLabel(l.budget)
	}
	prefixWidth := lipgloss.Width("  " + frame + " ")
	avail := r.width - prefixWidth - lipgloss.Width(counter) - 2
	if avail < 8 {
		avail = 8
	}
	label := clip(l.label, avail)
	line := "  " + r.chrome.paint(frame, accentStyle.Render(frame)) + " " + r.chrome.paint(label, inkStyle.Render(label))
	if counter != "" {
		line += "  " + r.chrome.paint(counter, mutedStyle.Render(counter))
	}
	fmt.Fprint(r.out, "\r\x1b[2K"+line)
}

func (r *Reporter) clearLocked() {
	if r.active != nil {
		fmt.Fprint(r.out, "\r\x1b[2K")
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
	io.WriteString(r.out, text)
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
	done   chan struct{}
}

type liveHandle struct {
	r *Reporter
	l *live
}

func (h *liveHandle) Detail(text string) {
	h.l.mu.Lock()
	h.l.detail = text
	h.l.mu.Unlock()
}

func (h *liveHandle) Done(text string) { h.finish("✓", text, h.detail()) }

func (h *liveHandle) DoneAs(_, pretty string) { h.Done(pretty) }

func (h *liveHandle) Failed(err error) {
	// The error itself is rendered once by the command; the step line says
	// which step died.
	h.finish("✗", h.l.label, "")
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
		pad := "  "
		cont := "    "
		plainPrefix := pad + mark + " "
		style := accentStyle
		if mark == "✗" {
			style = badStyle
		}
		for i, line := range wrapText(text, r.width-lipgloss.Width(plainPrefix)) {
			if i == 0 {
				b.WriteString(pad + r.chrome.paint(mark, style.Render(mark)) + " " + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
				continue
			}
			b.WriteString(cont + r.chrome.paint(line, inkStyle.Render(line)) + "\n")
		}
		if detail != "" {
			for _, line := range wrapText(detail, r.width-4) {
				b.WriteString("    " + r.chrome.paint(line, mutedStyle.Render(line)) + "\n")
			}
		}
	}
	io.WriteString(r.out, b.String())
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
	r.ui.chrome.question = question
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

// Error is the end of a failed run: box's voice in a bad frame, wrapped.
func (r *Reporter) Error(err error) {
	r.stopActive()
	if err == nil {
		return
	}
	width := mastheadWidth
	if r.width > 0 && r.width < width {
		width = r.width
	}
	inner := width - 4
	var body []string
	for _, raw := range strings.Split(err.Error(), "\n") {
		if strings.TrimSpace(raw) == "" {
			body = append(body, "")
			continue
		}
		for _, line := range wrapText(raw, inner-2) {
			body = append(body, line)
		}
	}
	var b strings.Builder
	b.WriteString(r.chrome.paint("╭"+strings.Repeat("─", width-2)+"╮", badStyle.Render("╭"+strings.Repeat("─", width-2)+"╮")) + "\n")
	for i, line := range body {
		prefix := "│ "
		if i == 0 {
			prefix = "│ ✗ "
		}
		gap := inner - 2 - lipgloss.Width(line)
		if gap < 0 {
			gap = 0
		}
		row := prefix + line + strings.Repeat(" ", gap) + " │"
		b.WriteString(r.chrome.paint(row, badStyle.Render(row)) + "\n")
	}
	b.WriteString(r.chrome.paint("╰"+strings.Repeat("─", width-2)+"╯", badStyle.Render("╰"+strings.Repeat("─", width-2)+"╯")) + "\n")
	r.emit(b.String())
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
}

// Close stops the spinner and the interrupt handler. Safe to call twice.
func (r *Reporter) Close() {
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

func elapsedLabel(d time.Duration) string { return durationLabel(d) }

func durationLabel(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return d.String()
}
