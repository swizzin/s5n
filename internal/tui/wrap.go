package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// defaultWidth is what a caller that did not measure the terminal gets: the
// narrowest width init supports. Wrapping to it is safe; an unwrapped line is not.
const defaultWidth = 80

// wrapText breaks plain text into lines of at most width columns, fed unstyled
// text on purpose: every caller wraps first and paints each line afterwards, so a
// wrap never has to count escapes.
//
// A line breaks only at a space, and a token is never split to fit: a wrapped
// `manager.mode=extern` / `al` reads as two different settings, and that is how
// someone copies the wrong one. Only a token wider than the whole line is
// hard-broken, by display width.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if lipgloss.Width(s) <= width {
		return []string{s}
	}
	var lines []string
	cur := ""
	for _, word := range strings.Split(strings.TrimSpace(s), " ") {
		if word == "" {
			continue
		}
		if cur == "" {
			if lipgloss.Width(word) <= width {
				cur = word
				continue
			}
			parts := hardBreak(word, width)
			lines = append(lines, parts[:len(parts)-1]...)
			cur = parts[len(parts)-1]
			continue
		}
		if lipgloss.Width(cur)+1+lipgloss.Width(word) <= width {
			cur += " " + word
			continue
		}
		lines = append(lines, cur)
		if lipgloss.Width(word) <= width {
			cur = word
			continue
		}
		parts := hardBreak(word, width)
		lines = append(lines, parts[:len(parts)-1]...)
		cur = parts[len(parts)-1]
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// wrapList packs items into lines of at most width columns, breaking only between
// them: the separator stays on its line, and no item is split unless it is wider
// than a whole line.
func wrapList(items []string, sep string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	cur := ""
	for _, item := range items {
		if item == "" {
			continue
		}
		candidate := item
		if cur != "" {
			candidate = cur + sep + item
		}
		if lipgloss.Width(candidate) <= width {
			cur = candidate
			continue
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		if lipgloss.Width(item) <= width {
			cur = item
			continue
		}
		parts := hardBreak(item, width)
		lines = append(lines, parts[:len(parts)-1]...)
		cur = parts[len(parts)-1]
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// wrapShell breaks a command into lines a shell would continue: only at spaces,
// ` \` ending every line but the last and counted in the width. A token is never
// split -- half a path is copied wrong -- unless it is wider than the line.
func wrapShell(cmd string, width int) []string {
	if width < 4 {
		width = 4
	}
	if lipgloss.Width(cmd) <= width {
		return []string{cmd}
	}
	words := strings.Fields(cmd)
	var lines []string
	cur := ""
	for _, word := range words {
		candidate := word
		if cur != "" {
			candidate = cur + " " + word
		}
		if lipgloss.Width(candidate) <= width-2 {
			cur = candidate
			continue
		}
		if cur != "" {
			lines = append(lines, cur+" \\")
		}
		// A token wider than the line cannot be kept whole and must not be split.
		cur = word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// hardBreak splits one token that cannot fit any line, at a cell boundary.
func hardBreak(word string, width int) []string {
	var out []string
	var b strings.Builder
	w := 0
	for _, r := range word {
		rw := lipgloss.Width(string(r))
		if w+rw > width && b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
			w = 0
		}
		b.WriteRune(r)
		w += rw
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
