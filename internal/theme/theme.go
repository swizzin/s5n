// Package theme is box's colours: seven named light/dark pairs, as plain data.
//
// Nothing here imports lipgloss, huh or Bubble Tea, because the same names are
// read by more than the terminal: the chart's portal page and the dashboard
// carry the palettes too. A theme is therefore hex strings and names, and the
// TUI turns them into styles.
//
// A theme is a PAIR, not one palette. box does not own the terminal's
// background, so which variant is right depends on what the terminal is
// painting behind it; a variant is also selectable on its own, for an operator
// who knows what they want. See Parse.
//
// Every contrast threshold in themes_test.go is met by the data here. Where an
// upstream palette's own value did not reach one, the value was changed and the
// change is noted on the field with the ratio that made it necessary; no
// threshold was lowered.
package theme

import (
	"strconv"
	"strings"
)

// Palette is one variant's colours, as #rrggbb strings.
//
// The eight are the whole vocabulary the TUI needs: BG and FG are the ground
// and the words, Muted is explanation, Dim is furniture that should be seen and
// not read (the masthead's unlit pieces), Accent is the one loud colour, Good
// and Bad are the two verdicts, and OnAccent is what is drawn on top of an
// accent fill.
type Palette struct {
	BG       string
	FG       string
	Muted    string
	Dim      string
	Accent   string
	Good     string
	Bad      string
	OnAccent string
}

// Variant is a palette and the name it is selected by within its pair.
//
// The name is a suffix, not a sentence: catppuccin's two variants are "mocha"
// and "latte", so the pair selects "catppuccin" and a person pins
// "catppuccin-mocha". Names are the upstream palette's own where it has them.
type Variant struct {
	Name string
	Palette
}

// Theme is a named light/dark pair.
type Theme struct {
	// Name is the pair: what someone types to follow the terminal.
	Name  string
	Dark  Variant
	Light Variant
}

// themes is the registry, in the order the error message lists it: the default
// first, then the palettes it was chosen against.
//
// Comments name the source and the tag each palette was read from, because the
// values are the only thing that makes a theme recognisable and a hand-edited
// one is a bug report waiting to happen.
var themes = []Theme{
	{
		// s5n is ours, from the brand notes: the mark's mint on the mark's
		// ink. The light variant is derived rather than upstream — mint
		// #3DD6B5 on white is 1.5:1, so the light accent is the same hue
		// taken down to #0B7F6C (4.62:1). Amber was the old accent and is
		// gone: it sat too close to Plex's orange to be told apart in a
		// terminal.
		Name: "s5n",
		Dark: Variant{Name: "dark", Palette: Palette{
			BG:       "#11161B",
			FG:       "#E4ECEF",
			Muted:    "#71818A", // brand #6E7D86 is 4.28:1 on BG; lightened to 4.51:1
			Dim:      "#1E2A30",
			Accent:   "#3DD6B5",
			Good:     "#4FD08A", // the mark's mint leaned green: 9.29:1 on BG
			Bad:      "#F2706A", // 6.32:1, warm enough to sit beside the mint
			OnAccent: "#11161B",
		}},
		Light: Variant{Name: "light", Palette: Palette{
			BG:       "#F6F8F9",
			FG:       "#1B2328",
			Muted:    "#5E6B73",
			Dim:      "#D5DEE2",
			Accent:   "#0B7F6C", // the mint at light-background contrast: 4.62:1
			Good:     "#2E7D4F",
			Bad:      "#C0362C",
			OnAccent: "#FFFFFF",
		}},
	},
	{
		// swizzin is the dashboard's own look: Bootswatch Darkly and Flatly.
		// Darkly: bg $gray-900 #222, fg $white, muted $gray-500 #ADB5BD, dim
		// $gray-800 #303030, accent $success #00BC8C (Darkly's link colour),
		// bad $danger #E74C3C. Darkly has one green, so good is that green.
		// https://github.com/thomaspark/bootswatch/blob/v5.3.8/dist/darkly/_variables.scss
		Name: "swizzin",
		Dark: Variant{Name: "darkly", Palette: Palette{
			BG:       "#222222",
			FG:       "#FFFFFF",
			Muted:    "#ADB5BD",
			Dim:      "#303030",
			Accent:   "#00BC8C",
			Good:     "#00BC8C",
			Bad:      "#E74C3C",
			OnAccent: "#222222",
		}},
		// Flatly: bg $white, fg $gray-900 #212529, dim $gray-200 #ECF0F1,
		// bad $danger #E74C3C.
		// Muted is $gray-700 #7B8A8B darkened to #6B797A: 3.59:1 on white,
		// and the next value up is $gray-800, which reads as text. Accent and
		// good are $success #18BC9C darkened to #11866F: 2.41:1 on white is
		// unreadable, and darkening keeps Flatly's teal where taking $primary
		// #2C3E50 would have made swizzin's light side navy.
		// https://github.com/thomaspark/bootswatch/blob/v5.3.8/dist/flatly/_variables.scss
		Light: Variant{Name: "flatly", Palette: Palette{
			BG:       "#FFFFFF",
			FG:       "#212529",
			Muted:    "#6B797A",
			Dim:      "#ECF0F1",
			Accent:   "#11866F",
			Good:     "#11866F",
			Bad:      "#E74C3C",
			OnAccent: "#FFFFFF",
		}},
	},
	{
		// Catppuccin, from palette.json: base is the ground, text the words,
		// overlay1 the quiet grey, surface0 the furniture, and the accents are
		// mauve/green/red.
		// https://github.com/catppuccin/palette/blob/main/palette.json
		Name: "catppuccin",
		Dark: Variant{Name: "mocha", Palette: Palette{
			BG:       "#1E1E2E",
			FG:       "#CDD6F4",
			Muted:    "#82889F", // overlay1 #7F849C is 4.44:1; lightened to 4.66:1
			Dim:      "#313244",
			Accent:   "#CBA6F7", // mauve, as dark as it is: 8.07:1 on base
			Good:     "#A6E3A1",
			Bad:      "#F38BA8",
			OnAccent: "#1E1E2E",
		}},
		Light: Variant{Name: "latte", Palette: Palette{
			BG:       "#EFF1F5",
			FG:       "#4C4F69",
			Muted:    "#6A6D82", // overlay1 #8C8FA1 is 2.83:1; darkened to 4.50:1, just past subtext0 #6C6F85
			Dim:      "#CCD0DA",
			Accent:   "#8839EF",
			Good:     "#3F9E2B", // green #40A02B is 2.96:1; darkened to 3.03:1
			Bad:      "#D20F39",
			OnAccent: "#FFFFFF",
		}},
	},
	{
		// Solarized, from the vim palette's own table.
		// https://github.com/altercation/solarized/blob/master/vim-colors-solarized/colors/solarized.vim
		//
		// Solarized is deliberately low contrast, and neither side's own body
		// text reaches 7:1: base1 #93A1A1 on base03 is 5.61:1, base01 #586E75
		// on base3 is 4.99:1. Each foreground is the nearest of its side's own
		// values that does, nudged along its own hue instead of jumping to
		// base2/base02, which would have made solarized into a high-contrast
		// theme wearing solarized's colours.
		Name: "solarized",
		Dark: Variant{Name: "dark", Palette: Palette{
			BG:       "#002B36",
			FG:       "#A8B4B4", // base1 #93A1A1 lightened: 5.61:1 -> 7.04:1
			Muted:    "#839496", // base0: 4.75:1; base01 is 2.79:1
			Dim:      "#073642",
			Accent:   "#B58900",
			Good:     "#859900",
			Bad:      "#DC322F", // 3.25:1: solarized's red, dimmer than the rest
			OnAccent: "#002B36",
		}},
		Light: Variant{Name: "light", Palette: Palette{
			BG:       "#FDF6E3",
			FG:       "#45565C", // base01 #586E75 darkened: 4.99:1 -> 7.11:1
			Muted:    "#586E75", // base02 is 12.05:1 and reads as text; base00 is 4.13:1
			Dim:      "#EEE8D5",
			Accent:   "#2076B3", // blue #268BD2 darkened: 3.41:1 -> 4.52:1
			Good:     "#849800", // green #859900 darkened: 2.97:1 -> 3.01:1
			Bad:      "#DC322F",
			OnAccent: "#FDF6E3",
		}},
	},
	{
		// Gruvbox, from the vim colours' own table.
		// https://github.com/morhetz/gruvbox/blob/master/colors/gruvbox.vim
		Name: "gruvbox",
		Dark: Variant{Name: "dark", Palette: Palette{
			BG:       "#282828",
			FG:       "#EBDBB2",
			Muted:    "#A89984", // gray #928374 is 4.02:1; the next gruvbox gray up is 5.30:1
			Dim:      "#3C3836",
			Accent:   "#FE8019",
			Good:     "#B8BB26",
			Bad:      "#FB4934",
			OnAccent: "#282828",
		}},
		Light: Variant{Name: "light", Palette: Palette{
			BG:       "#FBF1C7",
			FG:       "#3C3836",
			Muted:    "#665C54", // gray #928374 is 3.24:1; dark3 is 5.74:1
			Dim:      "#EBDBB2",
			Accent:   "#AF3A03", // bright orange #FE8019 and faded #D65D0E are 3.41:1 on light0
			Good:     "#79740E",
			Bad:      "#9D0006",
			OnAccent: "#FBF1C7",
		}},
	},
	{
		// Nord, Polar Night and Snow Storm.
		// https://github.com/nordtheme/nord/blob/develop/src/nord.scss
		Name: "nord",
		Dark: Variant{Name: "polar-night", Palette: Palette{
			BG:       "#2E3440", // nord0
			FG:       "#ECEFF4", // nord6
			Muted:    "#919CB1", // a grey-blue lifted off nord3 #4C566A (2.4:1); nord9 is the accent's family
			Dim:      "#3B4252", // nord1
			Accent:   "#88C0D0", // nord8
			Good:     "#A3BE8C", // nord14
			Bad:      "#BF616A", // nord11
			OnAccent: "#2E3440",
		}},
		Light: Variant{Name: "snow-storm", Palette: Palette{
			BG:       "#ECEFF4", // nord6
			FG:       "#2E3440", // nord0
			Muted:    "#4C566A", // nord3
			Dim:      "#D8DEE9", // nord4
			Accent:   "#4E6F97", // nord10 #5E81AC darkened: 3.50:1 -> 4.50:1
			Good:     "#719454", // nord14 #A3BE8C is 1.77:1 on snow storm; darkened to 3.00:1
			Bad:      "#BF616A", // nord11
			OnAccent: "#ECEFF4",
		}},
	},
	{
		// Dracula and Alucard, from the theme's published OSS palette table.
		// https://github.com/dracula/dracula-theme/blob/master/README.md
		Name: "dracula",
		Dark: Variant{Name: "dracula", Palette: Palette{
			BG:       "#282A36",
			FG:       "#F8F8F2",
			Muted:    "#8390B7", // comment #6272A4 is 3.03:1; lightened to 4.50:1
			Dim:      "#44475A",
			Accent:   "#FF79C6",
			Good:     "#50FA7B",
			Bad:      "#FF5555",
			OnAccent: "#282A36",
		}},
		Light: Variant{Name: "alucard", Palette: Palette{
			BG:       "#FFFBEB",
			FG:       "#1F1F1F",
			Muted:    "#6C664B", // comment
			Dim:      "#CFCFDE", // selection
			Accent:   "#A3144D", // pink
			Good:     "#14710A",
			Bad:      "#CB3A2A",
			OnAccent: "#FFFBEB",
		}},
	},
}

// Themes is the registry, in list order. It is a copy: a caller that sorts or
// trims it is not editing the palettes for everyone else.
func Themes() []Theme { return append([]Theme(nil), themes...) }

// Selection is what a theme name asked for: one pair, and either the variant to
// pin or nothing, meaning follow the terminal's background.
type Selection struct {
	Theme Theme
	// Variant is the pinned variant, or nil to follow the background.
	Variant *Variant
}

// Pick is the variant to draw with, given whether the terminal is dark. A
// pinned selection ignores that, which is the point of pinning.
func (s Selection) Pick(dark bool) Variant {
	if s.Variant != nil {
		return *s.Variant
	}
	if dark {
		return s.Theme.Dark
	}
	return s.Theme.Light
}

// Default is the pair box ships with. It follows the terminal.
func Default() Theme { return themes[0] }

// Parse resolves a theme name.
//
//   - "" is s5n, the default pair.
//   - A pair name ("catppuccin") follows the terminal's background.
//   - A pair and one of its variant names ("catppuccin-mocha", "s5n-light")
//     pins that variant.
//
// Names are matched without case or surrounding space, because they arrive from
// a values file a person edited. Anything else is an error that lists every
// name that would have worked: a typo in a theme is a startup error, not a
// silently different colour scheme.
func Parse(name string) (Selection, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, t := range themes {
		if want == "" || want == t.Name {
			return Selection{Theme: t}, nil
		}
		for _, v := range []Variant{t.Dark, t.Light} {
			if want == t.Name+"-"+v.Name {
				pinned := v
				return Selection{Theme: t, Variant: &pinned}, nil
			}
		}
	}
	return Selection{Theme: Default()}, &UnknownError{Name: want}
}

// Resolve is Parse and Pick in one step: the variant to draw with on a terminal
// that is dark or not.
func Resolve(name string, dark bool) (Variant, error) {
	s, err := Parse(name)
	if err != nil {
		return Default().Dark, err
	}
	return s.Pick(dark), nil
}

// Names is every name Parse accepts, pairs and variants, in registry order.
func Names() []string {
	names := make([]string, 0, len(themes)*3)
	for _, t := range themes {
		names = append(names, t.Name, t.Name+"-"+t.Dark.Name, t.Name+"-"+t.Light.Name)
	}
	return names
}

// UnknownError is the error for a name that is not in the registry. Its message
// is the whole sentence box shows: the name and every name that would have
// worked, so a typo is fixed without opening a manual.
type UnknownError struct {
	Name string
}

func (e *UnknownError) Error() string {
	return "unknown theme " + strconv.Quote(e.Name) + ": pick one of " + strings.Join(Names(), ", ")
}
