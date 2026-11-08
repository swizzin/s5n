package tui

import (
	"image/color"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// The palette is the approved design's: a warm amber accent, warm off-white
// ink, a muted grey for anything that is explanation, red for the path warning
// and green for a path that is already there.
var (
	accent = lipgloss.Color("#F2A65A")
	ink    = lipgloss.Color("#E6E1D8")
	muted  = lipgloss.Color("#8C8578")
	bad    = lipgloss.Color("#E0685C")
	good   = lipgloss.Color("#94BE72")
)

// theme is box's look for huh.
//
// plain is what NO_COLOR asks for: the same form, every colour dropped, so a
// terminal set that cannot be told apart is drawn as the same shape. Focus is
// still visible without colour: the current row keeps its selector glyph and
// the focused button is reversed.
func theme(plain bool) *huh.Styles {
	t := huh.ThemeBase(true)

	paint := func(s lipgloss.Style, c color.Color) lipgloss.Style {
		if plain {
			return s
		}
		return s.Foreground(c)
	}
	weight := func(s lipgloss.Style) lipgloss.Style {
		if plain {
			return s
		}
		return s.Bold(true)
	}

	t.Form.Base = lipgloss.NewStyle()
	t.Group.Base = lipgloss.NewStyle()
	t.FieldSeparator = lipgloss.NewStyle().SetString("\n")

	// One field per form, indented under the masthead rather than boxed: the
	// masthead is the box.
	t.Focused.Base = lipgloss.NewStyle().PaddingLeft(2)
	t.Focused.Title = weight(paint(lipgloss.NewStyle(), ink))
	t.Focused.Description = paint(lipgloss.NewStyle(), muted)
	t.Focused.ErrorIndicator = lipgloss.NewStyle().SetString("")
	// The error is drawn by the group's footer, outside the field's own base
	// style, so it needs the field's indent of its own: flush left, it read as
	// a message about the step rather than about this answer.
	t.Focused.ErrorMessage = paint(lipgloss.NewStyle().PaddingLeft(2), bad)
	t.Focused.SelectSelector = paint(lipgloss.NewStyle().SetString("▸ "), accent)
	t.Focused.MultiSelectSelector = paint(lipgloss.NewStyle().SetString("▸ "), accent)
	t.Focused.SelectedPrefix = paint(lipgloss.NewStyle().SetString("▣ "), accent)
	t.Focused.SelectedOption = paint(lipgloss.NewStyle(), ink)
	t.Focused.UnselectedPrefix = paint(lipgloss.NewStyle().SetString("▢ "), muted)
	t.Focused.UnselectedOption = paint(lipgloss.NewStyle(), muted)
	t.Focused.TextInput.Prompt = paint(lipgloss.NewStyle().SetString("› "), accent)
	t.Focused.TextInput.Cursor = paint(lipgloss.NewStyle(), accent)
	t.Focused.TextInput.Placeholder = paint(lipgloss.NewStyle(), muted)
	t.Focused.NoteTitle = weight(paint(lipgloss.NewStyle(), accent))

	focused := lipgloss.NewStyle().Padding(0, 2).MarginRight(1)
	if plain {
		focused = focused.Reverse(true)
	} else {
		focused = focused.Foreground(lipgloss.Color("#1B1B1B")).Background(accent)
	}
	t.Focused.FocusedButton = focused
	t.Focused.BlurredButton = paint(lipgloss.NewStyle().Padding(0, 2).MarginRight(1), ink)

	t.Blurred = t.Focused
	t.Blurred.Base = lipgloss.NewStyle().PaddingLeft(2)

	t.Help.ShortKey = paint(lipgloss.NewStyle(), accent)
	t.Help.ShortDesc = paint(lipgloss.NewStyle(), muted)
	t.Help.ShortSeparator = paint(lipgloss.NewStyle().SetString(" · "), muted)
	t.Help.Ellipsis = paint(lipgloss.NewStyle(), muted)
	return t
}
