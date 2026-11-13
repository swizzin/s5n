package tui

import (
	"image/color"
	"io"
	"os"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/theme"
)

// The palette in force, as styles.
//
// box draws one UI per process, and the reporter it draws through is that same
// UI, so the styles are package state: the whole program is one look, and
// threading eight colours through every call site would only make that harder
// to see. Every constructor -- New, and NewReporter through it -- calls use
// before anything is written, so nothing draws in an unset palette.
var (
	// It starts on s5n's dark side so that nothing can draw in an unset
	// palette, and use replaces it with what the run selected.
	accent   = lipgloss.Color(theme.Default().Dark.Accent)
	ink      = lipgloss.Color(theme.Default().Dark.FG)
	muted    = lipgloss.Color(theme.Default().Dark.Muted)
	good     = lipgloss.Color(theme.Default().Dark.Good)
	bad      = lipgloss.Color(theme.Default().Dark.Bad)
	onAccent = lipgloss.Color(theme.Default().Dark.OnAccent)

	accentStyle   = lipgloss.NewStyle().Foreground(accent)
	inkStyle      = lipgloss.NewStyle().Foreground(ink)
	mutedStyle    = lipgloss.NewStyle().Foreground(muted)
	badStyle      = lipgloss.NewStyle().Foreground(bad)
	goodStyle     = lipgloss.NewStyle().Foreground(good)
	wordmarkStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	boldInkStyle  = lipgloss.NewStyle().Foreground(ink).Bold(true)
)

// use paints the package's styles from one variant. Nothing else in this
// package knows a colour: the masthead's wordmark, a ✓ and a warning all name
// a role here, and the theme decides what that role looks like.
func use(p theme.Variant) {
	accent = lipgloss.Color(p.Accent)
	ink = lipgloss.Color(p.FG)
	muted = lipgloss.Color(p.Muted)
	good = lipgloss.Color(p.Good)
	bad = lipgloss.Color(p.Bad)
	onAccent = lipgloss.Color(p.OnAccent)

	accentStyle = lipgloss.NewStyle().Foreground(accent)
	inkStyle = lipgloss.NewStyle().Foreground(ink)
	mutedStyle = lipgloss.NewStyle().Foreground(muted)
	badStyle = lipgloss.NewStyle().Foreground(bad)
	goodStyle = lipgloss.NewStyle().Foreground(good)
	wordmarkStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	boldInkStyle = lipgloss.NewStyle().Foreground(ink).Bold(true)
}

// backgroundQuery is the terminal-background question, with box's own signature
// so a test can answer it without naming a terminal type. The implementation is
// internal/tui's own fenced OSC 11 query (background.go); the seam is here so a
// test with no terminal does not have to answer a terminal.
type backgroundQuery func(in, out *os.File) bool

// hasDarkBackground is that query, replaced in tests.
var hasDarkBackground backgroundQuery = queryBackground

// darkBackground is the ground box is drawing on.
//
// box asks the terminal for its background colour, fenced by a DA1 request so a
// terminal that does not answer OSC 11 says so instead of leaving box waiting:
// a DA1 reply before the colour means dark at once, and the whole query is
// capped at one second for a terminal that answers neither. Every spelling of
// "I don't know" -- no terminal, no answer in time, a failure to put the
// terminal in raw mode, a reply it cannot parse -- is dark. That is the right
// way to be wrong here: light text on a light ground is unreadable, a dark
// palette on a light terminal is only not what was asked for.
//
// Nothing is asked when the colour is about to be dropped anyway (NO_COLOR), or
// when the caller's streams are not terminals box can put in raw mode (a pipe, a
// buffer in a test).
func darkBackground(in io.Reader, out io.Writer, noColor bool) bool {
	if noColor {
		return true
	}
	fin, ok := in.(*os.File)
	if !ok {
		return true
	}
	fout, ok := out.(*os.File)
	if !ok {
		return true
	}
	return hasDarkBackground(fin, fout)
}

// huhTheme is box's look for huh: the same palette, applied to every part of a
// form box draws.
//
// plain is what NO_COLOR asks for: the same form, every colour dropped, so a
// terminal set that cannot be told apart is drawn as the same shape. Focus is
// still visible without colour: the current row keeps its selector glyph and
// the focused button is reversed.
func huhTheme(plain bool) *huh.Styles {
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
		// The theme's own ink, not a colour chosen here: on a light accent
		// (nord's, catppuccin latte's) white text is a smear.
		focused = focused.Foreground(onAccent).Background(accent)
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
