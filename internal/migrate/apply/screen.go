package apply

// screen.go is the confirm's layout: the blocks as a person reads them, one
// heading and a row per thing, in columns where the data has columns, wrapped at
// 80 with the facts hanging under the facts column (docs/output.md, rules 1, 7
// and 13).
//
// It is here rather than in cmd/box because the block it draws is the summary's
// own: a test renders the screen from the same function the command does, so the
// golden and the transcript cannot drift apart.

import (
	"strings"

	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/say"
)

// printGroup is one block of the confirm: a heading, the columns its rows read
// in, and the rows. A block with columns is the table a person compares down; a
// block without is rows whose facts are one sentence each.
func PrintGroup(out say.Reporter, g Group) {
	if len(g.Rows) == 0 {
		return
	}
	nameWidth := 0
	for _, r := range g.Rows {
		if w := len(r.Name); w > nameWidth {
			nameWidth = w
		}
	}
	if len(g.Columns) == 0 {
		if g.Title != "" {
			out.Line(g.Title)
		}
		for _, r := range g.Rows {
			glyph := r.Glyph
			if glyph == "" {
				glyph = say.MarkNote
			}
			writeRow(out, "  "+glyph+" ", r.Name, nameWidth, strings.Join(r.Facts, " · "))
		}
		return
	}
	// A table: the first column is the heading itself, so the column headings
	// line up over the facts.
	cols := make([]int, len(g.Columns))
	for _, r := range g.Rows {
		if r.Glyph != "" {
			continue
		}
		for i := range g.Columns {
			if i+1 >= len(r.Facts) {
				continue
			}
			if w := len(r.Facts[i]); w > cols[i] {
				cols[i] = w
			}
		}
	}
	head := padCol(g.Title, 2+nameWidth)
	for i, c := range g.Columns {
		head += "  " + padCol(c, cols[i])
	}
	out.Line(strings.TrimRight(head, " "))
	for _, r := range g.Rows {
		if r.Glyph != "" {
			// The fold: "+ 5 more" and the names that did not fit.
			out.Line("  " + r.Glyph + " " + strings.Join(r.Facts, "  "))
			continue
		}
		// Every column but the last is padded, so they line up; the last holds
		// what changes, and is what wraps.
		head := "  " + padCol(r.Name, nameWidth)
		for i := 0; i+1 < len(r.Facts); i++ {
			col := 0
			if i < len(cols) {
				col = cols[i]
			}
			head += "  " + padCol(r.Facts[i], col)
		}
		last := ""
		if len(r.Facts) > 0 {
			last = r.Facts[len(r.Facts)-1]
		}
		if strings.TrimSpace(last) == "" {
			out.Line(strings.TrimRight(head, " "))
			continue
		}
		head += "  "
		for i, line := range report.Wrap(last, report.Width-len(head)-2) {
			if i == 0 {
				out.Line(head + line)
				continue
			}
			out.Line(strings.Repeat(" ", len(head)) + line)
		}
	}
}

// writeRow is one plain block row: the mark in the gutter, the name in the
// column, and the facts wrapped under the facts column.
func writeRow(out say.Reporter, gutter, name string, width int, facts string) {
	head := gutter
	if name != "" {
		head += padCol(name, width) + "  "
	}
	if facts == "" {
		out.Line(strings.TrimRight(head, " "))
		return
	}
	for i, line := range report.Wrap(facts, report.Width-len(head)) {
		if i == 0 {
			out.Line(head + line)
			continue
		}
		out.Line(strings.Repeat(" ", len(head)) + line)
	}
}

// pad right-pads s to width.
func padCol(s string, width int) string {
	if n := width - len(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// PrintNote is one line of prose under the blocks: wrapped at the column like
// every other row, with the continuation hanging under the text (rule 7).
func PrintNote(out say.Reporter, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for i, line := range report.Wrap(text, report.Width-2) {
		if i == 0 {
			out.Line("  " + line)
			continue
		}
		out.Line("    " + line)
	}
}
