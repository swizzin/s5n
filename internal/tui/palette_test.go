package tui

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/setup"
)

// fakeBackground answers the OSC 11 query for one test and counts how often it
// was asked. box only asks when both ends are files it could put in raw mode,
// so the tests that care about the answer pass os.Pipe ends; the rest pass
// buffers and get the dark default.
func fakeBackground(t *testing.T, dark bool) *int {
	t.Helper()
	calls := 0
	old := hasDarkBackground
	hasDarkBackground = func(_, _ *os.File) bool {
		calls++
		return dark
	}
	t.Cleanup(func() { hasDarkBackground = old })
	return &calls
}

// terminalFiles is a pair of *os.File for a UI that needs terminal-shaped
// streams. Nothing is ever written to them: the tests that use this look at the
// palette, not the output.
func terminalFiles(t *testing.T) (in, out *os.File) {
	t.Helper()
	in, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); out.Close() })
	return in, out
}

// TestNewFollowsTheTerminalBackground: a pair name has no colours of its own.
// A terminal that says it is light gets the light side, without the operator
// naming a variant.
func TestNewFollowsTheTerminalBackground(t *testing.T) {
	for _, tc := range []struct {
		dark bool
		want string
	}{{true, "dark"}, {false, "light"}} {
		calls := fakeBackground(t, tc.dark)
		in, out := terminalFiles(t)
		u := New(Options{In: in, Out: out, Theme: "s5n"})
		if u.Err() != nil {
			t.Fatal(u.Err())
		}
		if got := u.Palette().Name; got != tc.want {
			t.Fatalf("dark=%v: picked %q, want %q", tc.dark, got, tc.want)
		}
		if *calls != 1 {
			t.Fatalf("dark=%v: the terminal was asked %d times, want once per UI", tc.dark, *calls)
		}
	}
}

// TestNewPinsAVariant: a variant name is the answer for a terminal that will
// not say what it is drawing on. It ignores the query entirely, so a pinned
// run pays no two-second wait for an answer it is not going to use.
func TestNewPinsAVariant(t *testing.T) {
	calls := fakeBackground(t, true)
	in, out := terminalFiles(t)
	u := New(Options{In: in, Out: out, Theme: "s5n-light"})
	if got := u.Palette().Name; got != "light" {
		t.Fatalf("picked %q on a dark terminal, want the pinned light", got)
	}
	if *calls != 0 {
		t.Fatalf("a pinned variant asked the terminal %d times", *calls)
	}
	if got := u.Palette().BG; got != "#F6F8F9" {
		t.Fatalf("pinned light background = %s", got)
	}
}

// TestUnknownThemeIsAnErrorWithTheList: the name comes from a values file, so a
// typo has to surface as a sentence naming what would have worked -- not as a
// run in colours nobody chose.
func TestUnknownThemeIsAnErrorWithTheList(t *testing.T) {
	u := New(Options{Out: &bytes.Buffer{}, Theme: "amber"})
	err := u.Err()
	if err == nil {
		t.Fatal("an unknown theme name was accepted")
	}
	for _, want := range []string{"amber", "s5n-light", "catppuccin-mocha"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not mention %q: %v", want, err)
		}
	}
	// The first question asked reports it too: cmd/box builds the UI in a
	// function that has no error to return, and the operator still has to hear
	// about the name they typed.
	if _, got := u.Input(setup.Input{Key: "gateway.host", Prompt: "domain"}); got == nil {
		t.Fatal("the first question did not report the bad theme name")
	}
}

// TestNoColorIgnoresTheTheme: NO_COLOR is the terminal saying it cannot be
// trusted with colour, so it wins over the theme, and the background is not
// even asked about -- there is nothing to pick between.
func TestNoColorIgnoresTheTheme(t *testing.T) {
	calls := fakeBackground(t, false)
	in, out := terminalFiles(t)
	u := New(Options{In: in, Out: out, NoColor: true, Theme: "dracula-alucard"})
	if *calls != 0 {
		t.Fatalf("NO_COLOR asked the terminal for its background %d times", *calls)
	}

	var buf bytes.Buffer
	u = New(Options{Out: &buf, NoColor: true, Theme: "dracula"})
	u.accessible = true
	u.Step(1, 2, "what domain will this box answer on?")
	if _, err := u.Input(setup.Input{Key: "gateway.host", Prompt: "domain", Default: "box.example.com"}); err != nil {
		t.Fatal(err)
	}
	shown := buf.String()
	if strings.Contains(shown, "\x1b[38;2;") || strings.Contains(shown, "\x1b[48;2;") {
		t.Fatalf("a themed run under NO_COLOR still emitted colour:\n%q", shown)
	}
	if !strings.Contains(stripANSI(shown), "b o x") {
		t.Fatalf("the plain masthead lost its wordmark:\n%s", stripANSI(shown))
	}
}

// TestTheMastheadIsDrawnInTheSelectedTheme: the palette reaches what a person
// reads, not just the form. The masthead's wordmark is the accent of the
// variant the UI resolved, and the frame is the muted colour.
func TestTheMastheadIsDrawnInTheSelectedTheme(t *testing.T) {
	u, out := line("x\n")
	u.Step(1, 2, "first")
	if _, err := u.Input(setup.Input{Key: "one", Prompt: "one"}); err != nil {
		t.Fatal(err)
	}
	shown := out.String()
	for _, want := range []string{escapeFor(accentStyle), escapeFor(mutedStyle), escapeFor(inkStyle)} {
		if !strings.Contains(shown, want) {
			t.Fatalf("the masthead is missing the %q escape:\n%q", want, shown)
		}
	}
}

// TestTheReporterDrawsTheSameVariant: one run, one palette. The reporter builds
// the UI it asks through, so a section frame and a finished step cannot be two
// different looks.
func TestTheReporterDrawsTheSameVariant(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, Yes: true, Theme: "gruvbox"})
	defer r.Close()
	if got := r.Palette().Name; got != "dark" {
		t.Fatalf("the reporter resolved %q, want gruvbox's dark side", got)
	}
	r.Phase("apply")
	if !strings.Contains(buf.String(), colourOf(accentStyle)) {
		t.Fatalf("the phase frame is not in the theme's accent:\n%q", buf.String())
	}
}

// escapeFor is the escape a style puts in front of its text.
func escapeFor(s interface{ Render(...string) string }) string {
	painted := s.Render("x")
	return painted[:strings.Index(painted, "x")]
}

// colourOf is the 38;2;r;g;b part of a style's escape, for the places the same
// colour is drawn bold or with another attribute on top.
func colourOf(s interface{ Render(...string) string }) string {
	e := escapeFor(s)
	return strings.TrimSuffix(strings.TrimPrefix(e, "\x1b["), "m")
}
