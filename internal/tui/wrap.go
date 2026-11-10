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
// A line breaks only at a space. A token is never split to make a line fit: a
// wrapped `platform.cert-` / `manager.mode=external` or `/var/data/s5n/config/`
// / `radarr-4k` reads as two different settings, which is exactly how someone
// copies the wrong one. Only a single token wider than the whole line is
// hard-broken, and then by display width.
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

// wrapList packs a list of items into lines of at most width columns, breaking
// only between items. It is how a list of names stays a list: the separator
// stays on the line it belongs to, and no item is split unless it is wider
// than a whole line on its own.
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
