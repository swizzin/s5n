package apply

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/say"
)

func TestAColumnWithAnArrowLinesUp(t *testing.T) {
	var buf bytes.Buffer
	out := &say.Plain{W: &buf, Err: &buf}
	out.Group(Group{
		Title:   "apps",
		Columns: []string{"carries", "version", "changes"},
		Rows: []say.TableRow{
			{Name: "autobrr", Facts: []string{"390.9 KiB", "1.91.1", "2 addresses"}},
			{Name: "pbh", Facts: []string{"66.2 MiB", "7.3 → 9.6  ↑", "1 address"}},
		},
	})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	col := func(line, fact string) int {
		i := strings.Index(line, fact)
		if i < 0 {
			t.Fatalf("%q is not in %q", fact, line)
		}
		return utf8.RuneCountInString(line[:i])
	}
	var auto, pbh string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "autobrr"):
			auto = l
		case strings.Contains(l, "pbh"):
			pbh = l
		}
	}
	if a, p := col(auto, "2 addresses"), col(pbh, "1 address"); a != p {
		t.Fatalf("the changes column starts at %d on one row and %d on the other:\n%s", a, p, buf.String())
	}
}

func TestWrapCountsColumnsNotBytes(t *testing.T) {
	text := strings.Repeat("a → b · ", 9) // 72 columns, 108 bytes
	text = strings.TrimSpace(text)
	if got := report.Wrap(text, report.Width); len(got) != 1 {
		t.Fatalf("a %d-column line wrapped to %d lines at %d: %q", utf8.RuneCountInString(text), len(got), report.Width, got)
	}
	if got := say.Wrap(text, say.Width); len(got) != 1 {
		t.Fatalf("the seam wrapped a %d-column line to %d lines at %d: %q", utf8.RuneCountInString(text), len(got), say.Width, got)
	}
}
