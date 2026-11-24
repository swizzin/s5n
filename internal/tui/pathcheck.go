package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/setup"
)

// ShowPathCheck draws the path report: the same columns table the plain
// transcript prints (setup.PathCheck.Text), with the two facts that carry
// meaning coloured -- whether the path is there, and the line that says writing
// to it can sink the host. Rule 13 is why it is a table and not a seven-row
// tree: the device, the filesystem and the room left are values a person
// compares down a column, not labels on a list.
func (u *UI) ShowPathCheck(check setup.PathCheck) error {
	if u.themeErr != nil {
		return u.themeErr
	}
	u.begin()
	title := "  " + setup.PathTitle
	fmt.Fprintln(u.out, u.chrome.paint(title, accentStyle.Bold(true).Render(title)))
	rows := check.Rows()
	kindWidth, pathWidth := 0, 0
	for _, row := range rows {
		kindWidth = max(kindWidth, lipgloss.Width(row.Kind))
		pathWidth = max(pathWidth, lipgloss.Width(row.Path))
	}
	for i, row := range rows {
		u.pathRow(row, warnings(check.Reports[i]), kindWidth, pathWidth)
	}
	if check.Footer != "" {
		u.block("    ", strings.TrimLeft(check.Footer, " "), mutedStyle)
	}
	u.end()
	return nil
}

// pathRow is one path's line: the columns Text prints, wrapped where the
// terminal runs out, then the notes under the kind column so they read as
// belonging to this path.
func (u *UI) pathRow(row setup.PathRow, warns []string, kindWidth, pathWidth int) {
	cols := []string{
		fmt.Sprintf("%-*s", kindWidth, row.Kind),
		fmt.Sprintf("%-*s", pathWidth, row.Path),
		fmt.Sprintf("%-6s", row.State),
		fmt.Sprintf("%-6s", row.Device),
		fmt.Sprintf("%-4s", row.FSType),
		row.Free,
	}
	head := "  " + cols[0] + "  " + cols[1] + "  "
	rest := strings.Join(cols[2:], "  ")
	cont := strings.Repeat(" ", lipgloss.Width(head))
	for i, line := range wrapText(rest, u.width-lipgloss.Width(head)) {
		if i == 0 {
			fmt.Fprintf(u.out, "%s%s\n", head, u.chrome.paint(line, inkStyle.Render(line)))
			continue
		}
		fmt.Fprintf(u.out, "%s%s\n", cont, u.chrome.paint(line, inkStyle.Render(line)))
	}
	noteIndent := "  " + strings.Repeat(" ", kindWidth) + "  "
	for _, clamp := range warns {
		for i, line := range wrapText("⚠ "+clamp, u.width-lipgloss.Width(noteIndent)-2) {
			if i == 0 {
				fmt.Fprintf(u.out, "%s%s\n", noteIndent, u.chrome.paint(line, badStyle.Render(line)))
				continue
			}
			fmt.Fprintf(u.out, "%s  %s\n", noteIndent, u.chrome.paint(line, badStyle.Render(line)))
		}
	}
	for _, note := range row.Notes {
		// A note the transcript marks with `!` is the same warning this draws
		// with the ⚠ above it: one fact, one line, in each renderer's own words.
		if strings.HasPrefix(note, "!") {
			continue
		}
		for _, line := range wrapText(note, u.width-lipgloss.Width(noteIndent)) {
			fmt.Fprintf(u.out, "%s%s\n", noteIndent, u.chrome.paint(line, mutedStyle.Render(line)))
		}
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

// warnings are the red lines under a path's row: the two ways a path can sink
// the host, both of which are about the disk rather than the directory. Only a
// path that is not there is warned about -- an existing one has whatever the
// operator gave it -- and the same-device case is the terminal's alone: the
// line prompt's own report is older than this and stays as it was.
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
