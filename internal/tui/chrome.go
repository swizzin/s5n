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

// trail is the answers-so-far line, or "" when there is nothing to show yet.
func (c *chrome) trail() string {
	var parts []string
	for _, a := range c.answers {
		if a.step < c.step {
			parts = append(parts, a.value)
		}
	}
	return strings.Join(parts, " · ")
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
	lines := []string{padTo("  "+mark+"   "+tag, counter, mastheadWidth-4)}
	if c.question != "" {
		lines = append(lines, padTo("  "+c.paint(c.question, boldInkStyle.Render(c.question)), "", mastheadWidth-4))
	}
	if trail := c.trail(); trail != "" {
		lines = append(lines, padTo("  "+c.paint("→ "+trail, mutedStyle.Render("→ "+trail)), "", mastheadWidth-4))
	}
	return c.boxed(lines)
}

// padTo is a row with its left text and right text at the edges of w columns.
func padTo(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}
