package theme

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestEveryVariantIsReadable is the gate on the data above: every colour a
// person reads has to reach a fixed contrast against what it is read on. The
// thresholds are WCAG's, kept where WCAG puts them, and computed here rather
// than trusted from a comment:
//
//   - body text on the background is 7:1, AAA, because a terminal has no
//     antialiasing worth the name and box is read while something is failing;
//   - muted explanation and the accent are 4.5:1, AA;
//   - text drawn on an accent-filled button is 4.5:1 against that fill, or the
//     busiest control in the form is the least legible thing on screen;
//   - the ✓ and ✗ marks are 3:1, the threshold for a graphic rather than prose.
func TestEveryVariantIsReadable(t *testing.T) {
	checks := []struct {
		name string
		get  func(Palette) string
		bg   func(Palette) string
		min  float64
	}{
		{"fg on bg", func(p Palette) string { return p.FG }, func(p Palette) string { return p.BG }, 7},
		{"muted on bg", func(p Palette) string { return p.Muted }, func(p Palette) string { return p.BG }, 4.5},
		{"accent on bg", func(p Palette) string { return p.Accent }, func(p Palette) string { return p.BG }, 4.5},
		{"onAccent on accent", func(p Palette) string { return p.OnAccent }, func(p Palette) string { return p.Accent }, 4.5},
		{"good on bg", func(p Palette) string { return p.Good }, func(p Palette) string { return p.BG }, 3},
		{"bad on bg", func(p Palette) string { return p.Bad }, func(p Palette) string { return p.BG }, 3},
	}
	for _, th := range Themes() {
		for _, v := range []Variant{th.Dark, th.Light} {
			name := th.Name + "-" + v.Name
			for _, c := range checks {
				got := contrast(t, c.get(v.Palette), c.bg(v.Palette))
				if got+1e-9 < c.min {
					t.Errorf("%s: %s is %.2f:1, want at least %g:1", name, c.name, got, c.min)
				}
			}
		}
	}
}

// TestPalettesAreCompleteHex catches a typo in a colour before it reaches a
// terminal: every field has to be a six-digit #rrggbb string, which is the only
// form lipgloss and an OSC 11 sequence both accept.
func TestPalettesAreCompleteHex(t *testing.T) {
	for _, th := range Themes() {
		for _, v := range []Variant{th.Dark, th.Light} {
			fields := map[string]string{
				"bg": v.BG, "fg": v.FG, "muted": v.Muted, "dim": v.Dim,
				"accent": v.Accent, "good": v.Good, "bad": v.Bad, "onAccent": v.OnAccent,
			}
			for field, value := range fields {
				if len(value) != 7 || value[0] != '#' {
					t.Errorf("%s-%s: %s = %q, want #rrggbb", th.Name, v.Name, field, value)
					continue
				}
				if _, _, _, err := rgb(value); err != nil {
					t.Errorf("%s-%s: %s = %q: %v", th.Name, v.Name, field, value, err)
				}
			}
		}
	}
}

// TestTheRegistryIsWellFormed: a name that appears twice makes Parse's answer
// depend on list order, and an empty variant name produces "nord-" as a valid
// name in the error list.
func TestTheRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, th := range Themes() {
		if th.Name == "" {
			t.Fatal("a theme has no name")
		}
		for _, v := range []Variant{th.Dark, th.Light} {
			if v.Name == "" {
				t.Fatalf("%s: a variant has no name", th.Name)
			}
			full := th.Name + "-" + v.Name
			if seen[full] {
				t.Fatalf("%s is in the registry twice", full)
			}
			seen[full] = true
		}
	}
	if len(Themes()) != 7 {
		t.Fatalf("%d themes, want the seven the design names", len(Themes()))
	}
	if got := Themes()[0].Name; got != "s5n" {
		t.Fatalf("the first theme is %q, want s5n: it is the default and the list's first line", got)
	}
}

// TestParseFollowsTheBackground: a pair name has no colours of its own. The
// terminal decides, which is the whole reason themes come in pairs.
func TestParseFollowsTheBackground(t *testing.T) {
	sel, err := Parse("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Variant != nil {
		t.Fatalf("a pair name pinned %q", sel.Variant.Name)
	}
	if got := sel.Pick(true).BG; got != "#1E1E2E" {
		t.Fatalf("dark pick = %s, want mocha's base", got)
	}
	if got := sel.Pick(false).BG; got != "#EFF1F5" {
		t.Fatalf("light pick = %s, want latte's base", got)
	}
}

// TestParsePinsAVariant: a variant name draws the same colours on any terminal,
// which is what an operator who does not trust OSC 11 wants.
func TestParsePinsAVariant(t *testing.T) {
	sel, err := Parse("catppuccin-mocha")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Variant == nil || sel.Variant.Name != "mocha" {
		t.Fatalf("selection = %+v, want mocha pinned", sel)
	}
	for _, dark := range []bool{true, false} {
		if got := sel.Pick(dark).BG; got != "#1E1E2E" {
			t.Fatalf("dark=%v: pinned mocha picked %s", dark, got)
		}
	}

	// The pair's own short names pin too: s5n-light is the light side on a
	// dark terminal, which is how someone with a light terminal that answers
	// no query gets the palette they asked for.
	sel, err = Parse("s5n-light")
	if err != nil {
		t.Fatal(err)
	}
	if got := sel.Pick(true).BG; got != "#F6F8F9" {
		t.Fatalf("pinned s5n-light on a dark terminal picked %s", got)
	}
}

// TestParseDefaultsToS5n: an unset theme is the shipped one, not an error.
// box's values may leave the key out entirely.
func TestParseDefaultsToS5n(t *testing.T) {
	for _, name := range []string{"", "  ", "S5N"} {
		sel, err := Parse(name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if sel.Theme.Name != "s5n" || sel.Variant != nil {
			t.Fatalf("%q resolved to %+v, want the s5n pair", name, sel)
		}
		if got := sel.Pick(true).Accent; got != "#3DD6B5" {
			t.Fatalf("%q: the default accent is %s, want the mark's mint", name, got)
		}
	}
}

// TestParseRejectsAnUnknownNameWithTheList: the error is the whole answer. A
// theme name is typed into a values file, so the message has to say what would
// have worked rather than that something is wrong.
func TestParseRejectsAnUnknownNameWithTheList(t *testing.T) {
	_, err := Parse("amber")
	if err == nil {
		t.Fatal("an unknown theme was accepted")
	}
	var unknown *UnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T, want *UnknownError", err)
	}
	if unknown.Name != "amber" {
		t.Fatalf("error names %q", unknown.Name)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"amber"`) {
		t.Fatalf("the error does not quote the name: %s", msg)
	}
	for _, name := range Names() {
		if !strings.Contains(msg, name) {
			t.Fatalf("the error does not list %q: %s", name, msg)
		}
	}
	// Every variant of every theme, not just the pairs: those are the names
	// someone would actually pin.
	for _, want := range []string{"s5n", "s5n-dark", "catppuccin-latte", "nord-snow-storm", "dracula-alucard"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the error does not list %q: %s", want, msg)
		}
	}
}

// TestResolveIsParseAndPick keeps the two-step and the one-step honest: the tui
// calls Resolve, the tests above call Parse.
func TestResolveIsParseAndPick(t *testing.T) {
	v, err := Resolve("nord", false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "snow-storm" {
		t.Fatalf("resolved to %q on a light terminal, want snow-storm", v.Name)
	}
	if _, err := Resolve("nope", true); err == nil {
		t.Fatal("Resolve accepted an unknown name")
	}
}

// contrast is the WCAG 2.x contrast ratio between two #rrggbb colours.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, err := luminance(a)
	if err != nil {
		t.Fatalf("%q: %v", a, err)
	}
	lb, err := luminance(b)
	if err != nil {
		t.Fatalf("%q: %v", b, err)
	}
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// luminance is WCAG's relative luminance: the channels linearised, then the
// weights for a standard observer.
func luminance(hex string) (float64, error) {
	r, g, b, err := rgb(hex)
	if err != nil {
		return 0, err
	}
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b), nil
}

// rgb splits #rrggbb into three channels in 0..1.
func rgb(hex string) (r, g, b float64, err error) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, errors.New("want #rrggbb")
	}
	var v [3]int
	for i := range v {
		n, perr := hex2(hex[1+i*2 : 3+i*2])
		if perr != nil {
			return 0, 0, 0, perr
		}
		v[i] = n
	}
	return float64(v[0]) / 255, float64(v[1]) / 255, float64(v[2]) / 255, nil
}

// hex2 reads one two-digit channel.
func hex2(s string) (int, error) {
	n := 0
	for _, c := range s {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, errors.New("not a hex digit: " + string(c))
		}
		n = n*16 + d
	}
	return n, nil
}
