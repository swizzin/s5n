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
	styled := huhTheme(false)
	if got := styled.Focused.Title.GetForeground(); got == nil {
		t.Fatal("the themed title has no colour")
	}
	if out := styled.Focused.Title.Render("domain"); !strings.Contains(out, "\x1b[") {
		t.Fatalf("the themed title renders no escapes: %q", out)
	}
	if got := styled.Focused.FocusedButton.GetBackground(); got == nil {
		t.Fatal("the focused button has no background: the selected choice must be visible")
	}

	plain := huhTheme(true)
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
	// The deep paths are shortened rather than allowed to push the domain out;
	// the elision is itself the visible sign that the line was trimmed.
	if !strings.Contains(shown, "…") {
		t.Fatalf("nothing says the breadcrumb was trimmed:\n%s", shown)
	}
}

// TestMastheadElidesPathsBeforeDroppingItems is the case the 80-column capture
// caught: the two paths filled the line, and the breadcrumb answered by
// eliding the domain to "…" while showing both paths in full. A path's middle
// is what can go.
func TestMastheadElidesPathsBeforeDroppingItems(t *testing.T) {
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
	trail := c.trail(textWidth() - 2)
	for _, want := range []string{"box.example.com", "/tmp/…/data", "/tmp/…/config"} {
		if !strings.Contains(trail, want) {
			t.Fatalf("the trail is missing %q: %s", want, trail)
		}
	}
	if strings.Contains(trail, "· … ·") || strings.HasPrefix(trail, "…") {
		t.Fatalf("an answer was dropped when eliding the paths was enough: %s", trail)
	}
	if lipgloss.Width("→ "+trail) > textWidth()-2 {
		t.Fatalf("the trail is %d wide, want at most %d: %s", lipgloss.Width("→ "+trail), textWidth()-2, trail)
	}
}

// TestBreadcrumbKeepsTheDomainWhenItMustDrop: with a domain too long to sit
// beside both paths even elided, the domain stays and the oldest path is the
// one that goes.
func TestBreadcrumbKeepsTheDomainWhenItMustDrop(t *testing.T) {
	domain := "box.a-rather-long-name.example.com"
	c := &chrome{step: 3, total: 4}
	c.answers = []answered{
		{step: 1, value: domain},
		{step: 2, value: "/tmp/s5n-demo/data"},
		{step: 2, value: "/tmp/s5n-demo/config"},
	}
	trail := c.trail(textWidth() - 2)
	if !strings.HasPrefix(trail, domain) {
		t.Fatalf("the domain was not kept: %s", trail)
	}
	if !strings.Contains(trail, "/tmp/…/config") {
		t.Fatalf("the newest answer was not kept: %s", trail)
	}
	if !strings.Contains(trail, "…") {
		t.Fatalf("the dropped answer is not marked: %s", trail)
	}
	if lipgloss.Width("→ "+trail) > textWidth()-2 {
		t.Fatalf("the trail is %d wide, want at most %d: %s", lipgloss.Width("→ "+trail), textWidth()-2, trail)
	}
}

func TestElidePathKeepsTheEnds(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/s5n/data":                  "/srv/…/data",
		"/tmp/s5n-demo/config":           "/tmp/…/config",
		"/srv/data":                      "/srv/data",
		"/data":                          "/data",
		"box.example.com":                "box.example.com",
		"/srv/a/very/long/path/keeps/on": "/srv/…/on",
	} {
		if got := elidePath(in); got != want {
			t.Fatalf("elidePath(%q) = %q, want %q", in, got, want)
		}
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
