package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/setup"
)

// ShowPathCheck draws the path report: a tree of facts per path, the values
// that matter (is it there, which disk, how full, who owns it) and a red line
// when the path would grow the disk the host itself runs from. The plain text
// of the same report is setup.PathCheck.Text(), which the line prompt prints.
func (u *UI) ShowPathCheck(check setup.PathCheck) error {
	u.begin()
	title := "path check"
	fmt.Fprintln(u.out, u.chrome.paint("  "+title, accentStyle.Bold(true).Render("  "+title)))
	for i, r := range check.Reports {
		if i > 0 {
			fmt.Fprintln(u.out)
		}
		u.pathBlock(r)
	}
	if check.Footer != "" {
		fmt.Fprintln(u.out, u.chrome.paint("  "+check.Footer, mutedStyle.Render("  "+check.Footer)))
	}
	u.end()
	return nil
}

// pathBlock is one path's rows. Continuation lines of the report's summary (the
// ownership note) are printed under the tree, because they are a warning about
// the path rather than a column of it.
func (u *UI) pathBlock(r setup.PathReport) {
	fmt.Fprintln(u.out, u.chrome.paint("  "+r.Kind, boldInkStyle.Render("  "+r.Kind)))

	rows := []row{
		{label: "path", value: r.Path},
		{label: "exists", value: existsText(r), verdict: true},
		{label: "device", value: deviceName(r)},
		{label: "mount point", value: orUnknown(r.Mount.Point)},
		{label: "filesystem", value: orUnknown(r.Mount.FSType)},
		{label: "free space", value: humanBytes(r.Free)},
		{label: "owner", value: orDash(r.Owner)},
	}
	holds, notes := splitSummary(r.Summary)
	if r.Exists {
		rows = append(rows, row{label: "", value: holds})
	}
	for i, rw := range rows {
		glyph := "├ "
		if i == len(rows)-1 && len(notes) == 0 {
			glyph = "└ "
		}
		u.row(glyph, rw)
	}
	for i, note := range notes {
		glyph := "├ "
		if i == len(notes)-1 {
			glyph = "└ "
		}
		u.row(glyph, row{label: "", value: note, warning: true})
	}
	for _, warn := range warnings(r) {
		fmt.Fprintln(u.out, u.chrome.paint("  ⚠ "+warn, badStyle.Render("  ⚠ "+warn)))
	}
}

// row is one line of the tree: a muted label, and a value that is a verdict
// (coloured), a warning (coloured) or plain ink.
type row struct {
	label   string
	value   string
	verdict bool
	warning bool
}

// row prints one row, with the tree glyph and the label muted and the value in
// ink, unless the value is itself a verdict.
func (u *UI) row(glyph string, r row) {
	edge := u.chrome.paint(glyph, mutedStyle.Render(glyph))
	if r.label == "" {
		fmt.Fprintf(u.out, "  %s%s\n", edge, u.colour(r, mutedStyle))
		return
	}
	label := fmt.Sprintf("%-12s", r.label)
	fmt.Fprintf(u.out, "  %s%s  %s\n", edge, u.chrome.paint(label, mutedStyle.Render(label)), u.colour(r, inkStyle))
}

// colour paints a row's value according to what it means.
func (u *UI) colour(r row, normal lipgloss.Style) string {
	switch {
	case r.verdict && r.value == "yes":
		return u.chrome.paint(r.value, goodStyle.Render(r.value))
	case r.verdict:
		return u.chrome.paint(r.value, badStyle.Render(r.value))
	case r.warning:
		return u.chrome.paint(r.value, badStyle.Render(r.value))
	}
	return u.chrome.paint(r.value, normal.Render(r.value))
}

func existsText(r setup.PathReport) string {
	if r.Exists {
		return "yes"
	}
	return "no"
}

// deviceName is what the disk is called. The source is the device node when the
// mount table has one; a container or a network mount may only have numbers.
func deviceName(r setup.PathReport) string {
	if r.Mount.Source != "" {
		return r.Mount.Source
	}
	if r.Mount.Point != "" {
		return fmt.Sprintf("%d:%d", r.Mount.Major, r.Mount.Minor)
	}
	return unknown
}

// unknown is what a row says when the host could not report the fact.
const unknown = "unknown"

func orUnknown(v string) string {
	if v == "" {
		return unknown
	}
	return v
}

func orDash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

// splitSummary separates the "what is already there" line from the ownership
// note setup appends to it.
func splitSummary(summary string) (string, []string) {
	lines := strings.Split(summary, "\n")
	first := strings.TrimSpace(lines[0])
	var notes []string
	for _, l := range lines[1:] {
		if s := strings.TrimSpace(l); s != "" {
			notes = append(notes, s)
		}
	}
	return first, notes
}

// warnings is the red line under the block: the two ways a path can sink the
// host, both of which are about the disk rather than the directory.
func warnings(r setup.PathReport) []string {
	if r.Exists {
		return nil
	}
	if r.OnRoot {
		return []string{"mount point is / — writing here grows the system disk"}
	}
	if r.SameDevice {
		return []string{"shares the root device (" + deviceName(r) + ") — filling it takes the host down"}
	}
	return nil
}

// humanBytes renders a size the way the plain report does: 24.0 TiB, 512 GiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
