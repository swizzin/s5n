package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// defaultWidth is what a caller that did not measure the terminal gets: the
// narrowest width init supports. Wrapping to it is always safe; an unwrapped
// line is not.
const defaultWidth = 80

// wrapText breaks plain text into lines of at most width columns. It is fed
// unstyled text on purpose: lipgloss.Width ignores escapes, but a wrap that
// has to count them is one more place to get wrong, so every caller wraps
// first and paints each line afterwards.
//
// The breakpoints keep a word whole where it fits: a space, a slash and a
// hyphen are where a path or a sentence may reasonably split. A single word
// longer than the line is hard-broken by lipgloss, so nothing can overflow.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if lipgloss.Width(s) <= width {
		return []string{s}
	}
	return strings.Split(lipgloss.Wrap(s, width, " /-"), "\n")
}
