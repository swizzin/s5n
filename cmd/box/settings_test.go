package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/theme"
	"github.com/swizzin/s5n/internal/tui"
)

// writeValues is one values file with a theme key, or none.
func writeValues(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/values.yaml"
	if content == "" {
		return path
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// box reads the theme out of the values file it already edits, so a person
// changes how box looks in the same file they tune the seedbox in.
func TestThemeComesFromTheValuesFile(t *testing.T) {
	name, err := themeName(writeValues(t, "theme: gruvbox\n"))
	if err != nil {
		t.Fatalf("theme: %v", err)
	}
	if name != "gruvbox" {
		t.Fatalf("theme = %q, want the file's gruvbox", name)
	}
}

// A box that has never written a values file draws in the chart's default,
// which is the empty name theme.Parse resolves to s5n.
func TestNoValuesFileIsTheDefaultTheme(t *testing.T) {
	name, err := themeName(writeValues(t, ""))
	if err != nil {
		t.Fatalf("theme: %v", err)
	}
	if name != theme.Default().Name {
		t.Fatalf("theme = %q, want the default %q", name, theme.Default().Name)
	}
}

// A name box does not know is a startup error that lists every name that would
// have worked: a typo in a theme must not become a whole run in colours nobody
// chose.
func TestAnUnknownThemeStopsWithTheList(t *testing.T) {
	_, err := themeName(writeValues(t, "theme: amber\n"))
	if err == nil {
		t.Fatal("box accepted a theme that does not exist")
	}
	var unknown *theme.UnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want a theme.UnknownError", err)
	}
	for _, name := range theme.Names() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal does not list %q:\n%s", name, err)
		}
	}
}

// The palette the file names is the palette the reporter draws with: the values
// key reaches the UI, not only the argument list.
func TestTheValuesThemeReachesTheReporter(t *testing.T) {
	path := writeValues(t, "theme: gruvbox\n")
	out := newOutput(tui.Terminal{Stdin: true, Stdout: true, TERM: "xterm-256color"}, true, startupTheme(path))
	r, ok := out.(*tui.Reporter)
	if !ok {
		t.Fatalf("a terminal-capable run got %T, want the live reporter", out)
	}
	defer r.Close()
	sel, err := theme.Parse("gruvbox")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Palette().Accent, sel.Theme.Dark.Accent; got != want {
		t.Fatalf("the reporter draws in %q, want gruvbox's accent %q", got, want)
	}
}

// The schema's enum and internal/theme's registry are one list: a name box
// accepts that the chart does not is a values file box refuses, and the other
// way round is a name nobody can use.
func TestTheChartThemeEnumIsTheRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../charts/s5n/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Theme struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"theme"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("values.schema.json: %v", err)
	}
	if schema.Properties.Theme.Type != "string" {
		t.Fatalf("theme is %q, want a string", schema.Properties.Theme.Type)
	}
	got, want := schema.Properties.Theme.Enum, theme.Names()
	if len(got) != len(want) {
		t.Fatalf("the schema lists %d names, the registry %d:\n schema  %v\n registry %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("theme %d of the schema is %q, the registry says %q (the lists must match exactly)", i, got[i], want[i])
		}
	}
}

// The chart's own default is a name box accepts, and it is what a box with no
// values file draws with.
func TestTheChartDefaultThemeIsOneBoxKnows(t *testing.T) {
	raw, err := os.ReadFile("../../charts/s5n/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, candidate := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(candidate, "theme:") {
			line = strings.TrimSpace(strings.TrimPrefix(candidate, "theme:"))
			break
		}
	}
	if line == "" {
		t.Fatal("charts/s5n/values.yaml has no top-level theme: key")
	}
	if _, err := theme.Parse(line); err != nil {
		t.Fatalf("the chart's default theme: %v", err)
	}
	if line != theme.Default().Name {
		t.Fatalf("the chart defaults to %q, box's default pair is %q", line, theme.Default().Name)
	}
}
