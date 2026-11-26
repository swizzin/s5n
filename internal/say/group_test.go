package say

import (
	"bytes"
	"strings"
	"testing"
)

// A block with columns is a table, every row's facts starting in the same
// column; the last column wraps under itself, so a wordy row is still a row
// (rule 7).
func TestPlainGroupIsOneTable(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Group(Group{
		Title:   "apps",
		Columns: []string{"carries", "changes"},
		Rows: []TableRow{
			{Name: "autobrr", Facts: []string{"1.5 GiB", "2 addresses · login → s5n"}},
			{Name: "radarr", Facts: []string{"34.0 MiB", `2,930 paths · 1 address · keeps download client "qBT" · keeps its API key`}},
			{Glyph: "+", Facts: []string{"5 more", "sonarr, tautulli"}},
		},
	})
	want := strings.Join([]string{
		"apps       carries   changes",
		"  autobrr  1.5 GiB   2 addresses · login → s5n",
		`  radarr   34.0 MiB  2,930 paths · 1 address · keeps download client "qBT" ·`,
		"                     keeps its API key",
		"  + 5 more  sonarr, tautulli",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("group:\n%s\nwant:\n%s", got, want)
	}
}

// A block with no columns is a list: one sentence per row, wrapped under the
// column its facts start in.
func TestPlainGroupWithoutColumnsIsAList(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Group(Group{Rows: []TableRow{
		{Glyph: MarkDone, Name: "data", Facts: []string{"Downloads → downloads · library → media"}},
		{Glyph: MarkNote, Name: "config", Facts: []string{"nothing to rename"}},
	}})
	want := strings.Join([]string{
		"  ✓ data    Downloads → downloads · library → media",
		"  · config  nothing to rename",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("list:\n%s\nwant:\n%s", got, want)
	}
}

// A note belongs to the block above it: two columns in, its continuation indented
// past the text, so a footer is one note and not a paragraph against the margin.
func TestPlainNoteHangsUnderItsText(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	long := strings.TrimSpace(strings.Repeat("a note that is too long for one line · ", 4))
	p.Note(long)
	p.Note("   ")
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("the note did not wrap:\n%s", buf.String())
	}
	if !strings.HasPrefix(lines[0], "  ") || strings.HasPrefix(lines[0], "   ") {
		t.Fatalf("the first line is not two columns in: %q", lines[0])
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("a continuation does not hang under the text: %q", line)
		}
	}
	for _, line := range lines {
		if columns(line) > Width {
			t.Fatalf("a note line is %d columns, want at most %d: %q", columns(line), Width, line)
		}
	}
}
