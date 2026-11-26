package tui

import (
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/swizzin/s5n/internal/theme"
)

// painter paints the terminal's own background with the theme's, so box's colours
// sit on the ground they were chosen for.
//
// Off unless someone asks. Painting outlives the process: nothing undoes it after
// kill -9, a dropped SSH session or a terminal that stops reading, and the
// terminal's background is the one thing box does not own, which is why the
// palettes come in pairs.
type painter struct {
	mu   sync.Mutex
	out  io.Writer
	bg   string // #rrggbb, lowercase; empty is off
	live bool   // the paint is on the terminal now
}

// newPainter is off unless on is asked for and the variant has a background.
func newPainter(out io.Writer, p theme.Variant, on bool) *painter {
	if !on || p.BG == "" {
		return &painter{}
	}
	// Lowercase, like the hex Bubble Tea writes: the sequence is the one its
	// renderer emits, so a terminal never gets two spellings of one colour.
	return &painter{out: out, bg: strings.ToLower(p.BG)}
}

// enabled says whether this painter does anything.
func (p *painter) enabled() bool { return p.bg != "" }

// hook is huh's Form.WithViewHook: Bubble Tea is told the background, and every
// frame a form draws is painted by its own renderer, including after a resize or a
// clear. The colour is the run's, resolved once; the hook is attached to every
// form because huh runs one Bubble Tea program per form.
func (p *painter) hook(v tea.View) tea.View {
	if !p.enabled() {
		return v
	}
	v.BackgroundColor = lipgloss.Color(p.bg)
	return v
}

// begin paints the background once, before the run's first line: a run that asks
// for painting asks for every line it prints.
func (p *painter) begin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live {
		return
	}
	p.paintLocked()
}

// again repaints after a form's program exits. Bubble Tea resets every background
// it set when a program shuts down, and box runs one program per form, so the
// reset would land in the gap before the reporter's own lines; repainting the same
// colour keeps the run one ground from the first masthead to the last card.
func (p *painter) again() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paintLocked()
}

// paintLocked writes the background and remembers it; the caller holds p.mu.
func (p *painter) paintLocked() {
	if !p.enabled() {
		return
	}
	_, _ = io.WriteString(p.out, ansi.SetBackgroundColor(p.bg))
	p.live = true
}

// reset puts the terminal's own background back: safe to call more than once, and
// doing nothing when the painter never painted.
func (p *painter) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.live {
		return
	}
	_, _ = io.WriteString(p.out, ansi.ResetBackgroundColor)
	p.live = false
}
