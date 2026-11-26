package tui

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/swizzin/s5n/internal/setup"
)

// The two sequences painting touches, the bytes Bubble Tea's renderer writes, so a
// test fails if the TUI emits a different one.
func painted() string  { return ansi.SetBackgroundColor("#11161b") }
func restored() string { return ansi.ResetBackgroundColor }

// A run that did not ask for painting writes no byte of it, on a normal run or a
// failure: the change outlives the process.
func TestBackgroundIsOffByDefault(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 90, Yes: true})
	r.Phase("apply")
	r.Success("nothing painted")
	r.Close()
	if strings.Contains(buf.String(), "\x1b]11") {
		t.Fatalf("a default run touched the terminal background:\n%q", buf.String())
	}
}

// One set before the first line and one reset on Close: the run is one ground, and
// the terminal is handed back as found.
func TestBackgroundIsPaintedOnceForTheRun(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 90, Yes: true, Background: true})
	r.Phase("apply")
	r.Success("painted")
	out := buf.String()
	if !strings.HasPrefix(out, painted()) {
		t.Fatalf("the run does not start painted:\n%q", out[:min(len(out), 60)])
	}
	if got := strings.Count(out, painted()); got != 1 {
		t.Fatalf("the background was set %d times before any form ran, want once", got)
	}
	if strings.Contains(out, restored()) {
		t.Fatalf("the background was reset before the run ended:\n%q", out)
	}

	r.Close()
	if !strings.HasSuffix(buf.String(), restored()) {
		t.Fatalf("the reset is not the last thing written:\n%q", buf.String())
	}
	r.Close()
	if got := strings.Count(buf.String(), restored()); got != 1 {
		t.Fatalf("closing twice wrote %d resets, want one", got)
	}
}

// Each form is its own Bubble Tea program, which resets the background on exit, so
// the paint is re-asserted in the gap before the reporter's next line.
func TestBackgroundSurvivesTheGapBetweenForms(t *testing.T) {
	var buf bytes.Buffer
	u := New(Options{Out: &buf, In: strings.NewReader("x\n"), Background: true})
	u.accessible = true
	if _, err := u.Input(setup.Input{Key: "one", Prompt: "one"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if got := strings.Count(out, painted()); got < 2 {
		t.Fatalf("the background was set %d times around one form, want the form and the gap after it:\n%q", got, out)
	}
	// The record is printed after the form's program has gone, so the last paint
	// comes before it.
	record := strings.Index(out, escapeFor(accentStyle)+"✓")
	if paint := strings.LastIndex(out, painted()); record < 0 || paint > record {
		t.Fatalf("the record was drawn after the repaint, not on it:\n%q", out)
	}
	u.Close()
	if !strings.HasSuffix(buf.String(), restored()) {
		t.Fatalf("Close did not restore the background:\n%q", buf.String())
	}
}

// cmd/box defers Close, so an error on the way out does not leave the terminal
// painted; the theme name's error returns before a single question.
func TestBackgroundResetsWhenARunReturnsAnError(t *testing.T) {
	var buf bytes.Buffer
	u := New(Options{Out: &buf, Background: true, Theme: "amber"})
	err := func() error {
		defer u.Close()
		return u.Note("hello")
	}()
	if err == nil {
		t.Fatal("a bad theme name did not fail the run")
	}
	if !strings.HasSuffix(buf.String(), restored()) {
		t.Fatalf("the background survived an error return:\n%q", buf.String())
	}
}

// Ctrl-C and SIGTERM leave through os.Exit, where no deferred Close runs, so the
// signal handler restores the background itself.
func TestBackgroundResetsOnACancelledContext(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 90, Background: true})

	var code int
	old := exit
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = old })

	r.interrupted()
	if code != 130 {
		t.Fatalf("exit code %d, want 130", code)
	}
	if !strings.HasSuffix(buf.String(), restored()) {
		t.Fatalf("the interrupt path left the terminal painted:\n%q", buf.String())
	}
	// The deferred Close after it writes nothing more.
	r.Close()
	if got := strings.Count(buf.String(), restored()); got != 1 {
		t.Fatalf("%d resets, want one", got)
	}
}

// NO_COLOR asks for no colour at all, and the background is a colour.
func TestBackgroundIsNotPaintedUnderNoColor(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 90, Yes: true, NoColor: true, Background: true})
	r.Phase("apply")
	r.Close()
	if strings.Contains(buf.String(), "\x1b]11") {
		t.Fatalf("NO_COLOR still painted the background:\n%q", buf.String())
	}
}

// The hook tells a form's own renderer the colour, so every frame, including a
// redraw after a resize, is on the run's ground.
func TestBackgroundHookPaintsTheForm(t *testing.T) {
	var buf bytes.Buffer
	u := New(Options{Out: &buf, Background: true, Theme: "s5n"})
	got := u.painter.hook(tea.View{})
	if got.BackgroundColor == nil {
		t.Fatal("the view hook left the form's background unset")
	}
	if body := renderColour(got.BackgroundColor); body != "17, 22, 27" {
		t.Fatalf("the hook paints rgb(%s), want s5n's #11161B", body)
	}

	off := New(Options{Out: &buf})
	if v := off.painter.hook(tea.View{}); v.BackgroundColor != nil {
		t.Fatal("a run without painting set a background anyway")
	}
}

// TestCloseIsSafeWithoutPainting: every command defers Close, painted or not.
func TestCloseIsSafeWithoutPainting(t *testing.T) {
	var buf bytes.Buffer
	u := New(Options{Out: &buf})
	u.Close()
	u.Close()
	if buf.String() != "" {
		t.Fatalf("an unpainted UI wrote %q", buf.String())
	}
}

// renderColour is a colour's channels as a terminal would receive them.
func renderColour(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("%d, %d, %d", r>>8, g>>8, b>>8)
}
