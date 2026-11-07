package seedbox

import (
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	before := map[string]any{
		"unchanged": "same",
		"scalar":    "old",
		"number":    int(1000),
		"gone":      "bye",
		"apps": map[string]any{
			"sonarr": map[string]any{"enabled": true, "port": 8989},
			"plex":   map[string]any{"enabled": true},
		},
		"list": []any{"a", "b"},
	}
	after := map[string]any{
		"unchanged": "same",
		"scalar":    "new",
		"number":    float64(1000),
		"added":     "hi",
		"apps": map[string]any{
			"sonarr": map[string]any{"enabled": false, "port": 8989},
		},
		"list": []any{"a", "c", "d"},
	}

	got := Diff(before, after)
	want := []string{
		"+ added: \"hi\"",
		"- apps.plex.enabled: true",
		"~ apps.sonarr.enabled: true -> false",
		"- gone: \"bye\"",
		"~ list[1]: \"b\" -> \"c\"",
		"+ list[2]: \"d\"",
		"~ scalar: \"old\" -> \"new\"",
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %d, want %d:\n%s", len(got), len(want), FormatChanges(got))
	}
	lines := strings.Split(FormatChanges(got), "\n")
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], want[i])
		}
	}
}

func TestDiffOfIdenticalDocumentsIsEmpty(t *testing.T) {
	a := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}, "n": 1}
	if got := Diff(a, cloneMap(a)); len(got) != 0 {
		t.Errorf("changes = %v, want none", FormatChanges(got))
	}
}

// TestDiffAroundAnEdit is the shape box apply uses: effective values before,
// one edit, effective values after, and one line of difference.
func TestDiffAroundAnEdit(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	chart := realChart(t)
	before, err := s.Effective(chart)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if err := s.SetValue("wiring.timeoutSeconds", 900); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	after, err := s.Effective(chart)
	if err != nil {
		t.Fatalf("Effective after edit: %v", err)
	}

	changes := Diff(before, after)
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want one:\n%s", len(changes), FormatChanges(changes))
	}
	c := changes[0]
	if c.Path != "wiring.timeoutSeconds" || c.Kind != Changed || c.New != 900 {
		t.Errorf("change = %+v, want wiring.timeoutSeconds -> 900", c)
	}
	if got := FormatChanges(changes); got != "~ wiring.timeoutSeconds: 600 -> 900" {
		t.Errorf("formatted = %q", got)
	}
}
