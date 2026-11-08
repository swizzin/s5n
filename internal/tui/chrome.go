package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// mastheadWidth is the box around the masthead. It is fixed rather than the
// terminal's width so a 100- and an 80-column terminal draw the same frame, and
// it fits the narrowest terminal init claims to support.
const mastheadWidth = 66

var (
	wordmarkStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	boldInkStyle  = lipgloss.NewStyle().Foreground(ink).Bold(true)
	inkStyle      = lipgloss.NewStyle().Foreground(ink)
	mutedStyle    = lipgloss.NewStyle().Foreground(muted)
	accentStyle   = lipgloss.NewStyle().Foreground(accent)
	badStyle      = lipgloss.NewStyle().Foreground(bad)
	goodStyle     = lipgloss.NewStyle().Foreground(good)
)

// chrome is box's own furniture around huh: the masthead and the step counter.
// It is kept apart from the forms so the copy and the frame are one place, and
// so the plain (NO_COLOR) drawing is the same code with the colours dropped.
type chrome struct {
	noColor     bool
	step, total int
	question    string
	// answers are the text answers already given. A secret never goes in
	// here: the breadcrumb is shown on screen.
	answers []answered
	// last is the step this chrome has already printed, so a step with several
	// fields (the two paths, then the confirm) gets one masthead, the way one
	// box per step reads.
	last string
}

// head is the masthead to print, or false when this step's is already on
// screen above the field about to be drawn.
func (c *chrome) head() (string, bool) {
	key := fmt.Sprintf("%d/%d\x00%s", c.step, c.total, c.question)
	if key == c.last {
		return "", false
	}
	c.last = key
	return c.masthead(), true
}

// answered is one text answer and the step it belongs to. The breadcrumb only
// shows earlier steps, so a step's own answers do not appear until it is done.
type answered struct {
	step  int
	value string
}

// remember records a text answer for the breadcrumb.
func (c *chrome) remember(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	c.answers = append(c.answers, answered{step: c.step, value: value})
}

// paint applies a style unless NO_COLOR asked for none.
func (c *chrome) paint(plain, styled string) string {
	if c.noColor {
		return plain
	}
	return styled
}

// textWidth is how many columns a masthead line has to work with, after the
// frame and its padding.
func textWidth() int { return mastheadWidth - 4 }

// trail is the answers-so-far line, or "" when there is nothing to show yet. It
// keeps the newest answers and drops the oldest, because a long breadcrumb must
// not push the frame open.
func (c *chrome) trail(max int) string {
	var parts []string
	dropped := false
	for i := len(c.answers) - 1; i >= 0; i-- {
		if c.answers[i].step >= c.step {
			continue
		}
		candidate := strings.Join(append([]string{c.answers[i].value}, parts...), " · ")
		if lipgloss.Width("→ "+candidate) > max {
			dropped = true
			break
		}
		parts = append([]string{c.answers[i].value}, parts...)
	}
	trail := strings.Join(parts, " · ")
	if dropped && trail != "" && lipgloss.Width("→ … · "+trail) <= max {
		trail = "… · " + trail
	}
	return trail
}

// boxed draws the masthead frame: a rounded border in the muted colour, every
// line padded to the same width. lipgloss.Width ignores escapes, so a styled
// line pads like a plain one.
func (c *chrome) boxed(lines []string) string {
	edge := func(s string) string { return c.paint(s, mutedStyle.Render(s)) }
	var b strings.Builder
	b.WriteString(edge("╭" + strings.Repeat("─", mastheadWidth-2) + "╮"))
	b.WriteString("\n")
	for _, l := range lines {
		gap := mastheadWidth - 4 - lipgloss.Width(l)
		if gap < 0 {
			gap = 0
		}
		b.WriteString(edge("│ "))
		b.WriteString(l)
		b.WriteString(strings.Repeat(" ", gap))
		b.WriteString(edge(" │"))
		b.WriteString("\n")
	}
	b.WriteString(edge("╰" + strings.Repeat("─", mastheadWidth-2) + "╯"))
	return b.String()
}

// masthead is the chrome above every field: the wordmark, what this is, the
// step, the step's question and, once a step is answered, the answers so far,
// so the operator can see where they are without scrolling back.
func (c *chrome) masthead() string {
	mark := c.paint("b o x", wordmarkStyle.Render("b o x"))
	tag := c.paint("s5n · seedbox setup", mutedStyle.Render("s5n · seedbox setup"))
	counter := ""
	if c.total > 0 {
		n := fmt.Sprintf("%d/%d", c.step, c.total)
		counter = c.paint(n, mutedStyle.Render(n))
	}
	// Every row after the wordmark is indented two columns, so it has two
	// fewer columns to work with.
	inner := textWidth() - 2
	lines := []string{padTo("  "+mark+"   "+tag, counter, textWidth())}
	if c.question != "" {
		q := clip(c.question, inner)
		lines = append(lines, padTo("  "+c.paint(q, boldInkStyle.Render(q)), "", textWidth()))
	}
	if trail := c.trail(inner); trail != "" {
		t := "→ " + trail
		lines = append(lines, padTo("  "+c.paint(t, mutedStyle.Render(t)), "", textWidth()))
	}
	return c.boxed(lines)
}

// clip shortens a line to max columns, with an ellipsis. The frame must never
// be pushed open by an answer somebody typed.
func clip(s string, max int) string {
	if lipgloss.Width(s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)+"…") > max {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// padTo is a row with its left text and right text at the edges of w columns.
func padTo(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}
