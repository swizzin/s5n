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

// painter paints the terminal's own background with the theme's, so box's
// colours sit on the ground they were chosen for instead of on whatever the
// terminal had.
//
// It is off unless someone asks for it. Painting is a change to the terminal
// that outlives the process: there is no way to undo it after kill -9, a
// dropped SSH session or a terminal that stops reading, and the colour is left
// behind until the operator's own reset. The terminal's background is also the
// one thing box does not own, which is why the palettes come in pairs in the
// first place.
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
	// Lowercase, like the hex Bubble Tea writes: the sequence is the same one
	// its renderer emits, so a terminal cannot end up with two spellings of
	// the same colour.
	return &painter{out: out, bg: strings.ToLower(p.BG)}
}

// enabled says whether this painter does anything.
func (p *painter) enabled() bool { return p.bg != "" }

// hook is huh's Form.WithViewHook. It is how Bubble Tea is told the background:
// every frame a form draws is then painted by its own renderer, including the
// ones drawn after a resize or a clear.
//
// The colour is the run's, resolved once; the hook itself has to be attached to
// every form because huh runs one Bubble Tea program per form and a view hook
// belongs to a program. That is not per-form painting: nothing here reads a
// form, and the value never changes.
func (p *painter) hook(v tea.View) tea.View {
	if !p.enabled() {
		return v
	}
	v.BackgroundColor = lipgloss.Color(p.bg)
	return v
}

// begin paints the background once, before the run's first line. A run that
// asks for painting asks for it for every line it prints, not only the ones
// whose form happens to be on screen.
func (p *painter) begin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live {
		return
	}
	p.paintLocked()
}

// again repaints after a form's program has exited.
//
// Bubble Tea resets every background it set when a program shuts down, and box
// runs one program per form: the reset would land in the gap between a form and
// the reporter's own lines, which is every transcript line a person scrolls
// back through. Repainting there is invisible -- it is the same colour -- and
// it is what keeps the run one ground from the first masthead to the last card.
func (p *painter) again() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paintLocked()
}

// paintLocked writes the background and remembers it. The caller holds p.mu.
func (p *painter) paintLocked() {
	if !p.enabled() {
		return
	}
	_, _ = io.WriteString(p.out, ansi.SetBackgroundColor(p.bg))
	p.live = true
}

// reset puts the terminal's own background back. It is safe to call more than
// once and does nothing when the painter never painted.
func (p *painter) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.live {
		return
	}
	_, _ = io.WriteString(p.out, ansi.ResetBackgroundColor)
	p.live = false
}
