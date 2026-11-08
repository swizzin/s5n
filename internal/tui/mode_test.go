package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name string
		term Terminal
		want Plan
	}{
		{"a normal terminal gets huh", Terminal{Stdin: true, Stdout: true, TERM: "xterm-256color"}, Plan{Mode: ModeHuh}},
		{"NO_COLOR keeps huh and drops colour", Terminal{Stdin: true, Stdout: true, TERM: "xterm", NoColor: true}, Plan{Mode: ModeHuh, NoColor: true}},
		{"TERM=dumb is the line prompt", Terminal{Stdin: true, Stdout: true, TERM: "dumb"}, Plan{Mode: ModePlain}},
		{"no terminal on stdin is the line prompt", Terminal{Stdout: true, TERM: "xterm"}, Plan{Mode: ModePlain}},
		{"no terminal on stdout is the line prompt", Terminal{Stdin: true, TERM: "xterm"}, Plan{Mode: ModePlain}},
		{"--plain wins over a good terminal", Terminal{Stdin: true, Stdout: true, TERM: "xterm", Plain: true}, Plan{Mode: ModePlain}},
		{"--plain and NO_COLOR is still the line prompt", Terminal{Stdin: true, Stdout: true, TERM: "xterm", Plain: true, NoColor: true}, Plan{Mode: ModePlain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.term); got != tc.want {
				t.Fatalf("Decide(%+v) = %+v, want %+v", tc.term, got, tc.want)
			}
		})
	}
}

func TestThemePaintsAndDropsColour(t *testing.T) {
	styled := theme(false)
	if got := styled.Focused.Title.GetForeground(); got == nil {
		t.Fatal("the themed title has no colour")
	}
	if out := styled.Focused.Title.Render("domain"); !strings.Contains(out, "\x1b[") {
		t.Fatalf("the themed title renders no escapes: %q", out)
	}
	if got := styled.Focused.FocusedButton.GetBackground(); got == nil {
		t.Fatal("the focused button has no background: the selected choice must be visible")
	}

	plain := theme(true)
	if out := plain.Focused.Title.Render("domain"); out != "domain" {
		t.Fatalf("the plain theme still styles text: %q", out)
	}
	if out := plain.Focused.FocusedButton.Render("yes"); strings.Contains(out, "\x1b[38;") || strings.Contains(out, "\x1b[48;") {
		t.Fatalf("the plain theme still emits colour: %q", out)
	}
}

func TestMastheadIsAFixedWidthBox(t *testing.T) {
	c := &chrome{step: 3, total: 4, question: "which apps should run here?"}
	c.answers = []answered{{step: 1, value: "box.example.com"}, {step: 2, value: "/srv/s5n/data"}}
	for _, noColor := range []bool{false, true} {
		c.noColor = noColor
		lines := strings.Split(c.masthead(), "\n")
		if len(lines) != 5 {
			t.Fatalf("noColor=%v: %d lines, want frame+3 rows+frame:\n%s", noColor, len(lines), c.masthead())
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got != mastheadWidth {
				t.Fatalf("noColor=%v: line %d is %d wide, want %d: %q", noColor, i, got, mastheadWidth, line)
			}
		}
		text := stripANSI(c.masthead())
		for _, want := range []string{"b o x", "s5n · seedbox setup", "3/4", "which apps should run here?", "box.example.com"} {
			if !strings.Contains(text, want) {
				t.Fatalf("noColor=%v: the masthead is missing %q:\n%s", noColor, want, text)
			}
		}
	}
}

func TestMastheadTrimsALongBreadcrumb(t *testing.T) {
	c := &chrome{step: 3, total: 4, question: "which apps should run here?"}
	c.answers = []answered{
		{step: 1, value: "box.example.com"},
		{step: 2, value: "/srv/a/very/long/seedbox/data/path/that/keeps/going"},
		{step: 2, value: "/srv/a/very/long/seedbox/config/path/that/keeps/going"},
	}
	shown := stripANSI(c.masthead())
	for i, line := range strings.Split(shown, "\n") {
		if got := lipgloss.Width(line); got != mastheadWidth {
			t.Fatalf("line %d is %d wide, want %d: %q", i, got, mastheadWidth, line)
		}
	}
	if !strings.Contains(shown, "… · ") {
		t.Fatalf("nothing says the breadcrumb was trimmed:\n%s", shown)
	}
}

// TestMastheadFitsTheWidestBreadcrumbThatFits is the case the first capture
// caught: a trail exactly as wide as the line, with the indent on top.
func TestMastheadFitsTheWidestBreadcrumbThatFits(t *testing.T) {
	c := &chrome{step: 3, total: 4, question: "which apps should run here?"}
	c.answers = []answered{
		{step: 1, value: "box.example.com"},
		{step: 2, value: "/tmp/s5n-demo/data"},
		{step: 2, value: "/tmp/s5n-demo/config"},
	}
	for i, line := range strings.Split(stripANSI(c.masthead()), "\n") {
		if got := lipgloss.Width(line); got != mastheadWidth {
			t.Fatalf("line %d is %d wide, want %d: %q", i, got, mastheadWidth, line)
		}
	}
	if !strings.Contains(stripANSI(c.masthead()), "/tmp/s5n-demo/config") {
		t.Fatalf("the newest answer was dropped even though it fits:\n%s", stripANSI(c.masthead()))
	}
}

// stripANSI removes SGR sequences so a test can look at what a person reads.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 'A' || s[j] > 'Z') && (s[j] < 'a' || s[j] > 'z') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
