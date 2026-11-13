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
//
// Every line wraps to the terminal's width with its continuation kept under
// the line's own indent. init claims to support 80 columns, and an 80-column
// terminal is exactly where the footer and the root-device warning overflow;
// letting the terminal wrap them put the continuation at column 0, which read
// as a new block rather than the end of this one.
func (u *UI) ShowPathCheck(check setup.PathCheck) error {
	if u.themeErr != nil {
		return u.themeErr
	}
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
		u.block("    ", strings.TrimLeft(check.Footer, " "), mutedStyle)
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
		u.treeRow(glyph, rw)
	}
	for i, note := range notes {
		glyph := "├ "
		if i == len(notes)-1 {
			glyph = "└ "
		}
		u.treeRow(glyph, row{label: "", value: note, warning: true})
	}
	for _, warn := range warnings(r) {
		u.warning(warn)
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

// treeRow prints one row, with the tree glyph and the label muted and the value
// styled by what it means. A value too wide for the row wraps under its own
// column, so a long path or note does not run to the margin or break the tree.
func (u *UI) treeRow(glyph string, r row) {
	style := u.valueStyle(r)
	edge := u.chrome.paint(glyph, mutedStyle.Render(glyph))
	if r.label == "" {
		// The glyph and its two leading columns are the hanging indent.
		const indent = "    "
		for _, line := range wrapText(r.value, u.width-lipgloss.Width(indent)) {
			fmt.Fprintf(u.out, "%s%s\n", indent, u.chrome.paint(line, style.Render(line)))
		}
		return
	}
	label := fmt.Sprintf("%-12s", r.label)
	painted := u.chrome.paint(label, mutedStyle.Render(label))
	const gap = "  "
	prefix := "  " + glyph + label + gap
	cont := strings.Repeat(" ", lipgloss.Width(prefix))
	for i, line := range wrapText(r.value, u.width-lipgloss.Width(prefix)) {
		if i == 0 {
			fmt.Fprintf(u.out, "  %s%s%s%s\n", edge, painted, gap, u.chrome.paint(line, style.Render(line)))
			continue
		}
		fmt.Fprintf(u.out, "%s%s\n", cont, u.chrome.paint(line, style.Render(line)))
	}
}

// warning is the red line under a path's block: the two ways a path can sink
// the host, both of which are about the disk rather than the directory. Its
// continuation lines stay under the glyph, not against the margin.
func (u *UI) warning(text string) {
	const indent = "  ⚠ "
	cont := strings.Repeat(" ", lipgloss.Width(indent))
	for i, line := range wrapText(text, u.width-lipgloss.Width(indent)) {
		if i == 0 {
			fmt.Fprintf(u.out, "%s%s\n", u.chrome.paint(indent, badStyle.Render(indent)), u.chrome.paint(line, badStyle.Render(line)))
			continue
		}
		fmt.Fprintf(u.out, "%s%s\n", cont, u.chrome.paint(line, badStyle.Render(line)))
	}
}

// block prints a paragraph at one indent, wrapped to the terminal. The
// continuation lines keep the indent, which is the whole point of wrapping
// here rather than letting the terminal do it.
func (u *UI) block(indent, text string, style lipgloss.Style) {
	for _, line := range wrapText(text, u.width-lipgloss.Width(indent)) {
		fmt.Fprintf(u.out, "%s%s\n", indent, u.chrome.paint(line, style.Render(line)))
	}
}

// valueStyle colours a row's value according to what it means.
func (u *UI) valueStyle(r row) lipgloss.Style {
	switch {
	case r.verdict && r.value == "yes":
		return goodStyle
	case r.verdict, r.warning:
		return badStyle
	case r.label == "":
		// The "what is already there" line is explanation, not a fact column.
		return mutedStyle
	}
	return inkStyle
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
	if src := r.Mount.Source; src != "" {
		// mountinfo names a btrfs subvolume as /dev/sda1[/@srv]; the disk is
		// what matters here, not the subvolume it was cut from.
		if i := strings.Index(src, "["); i > 0 {
			src = src[:i]
		}
		return src
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
