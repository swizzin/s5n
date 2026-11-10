package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// A dotted, hyphenated key and a slashed path are each one token. Wrapping may
// move a token to the next line, but it may never split it: `platform.cert-` /
// `manager.mode=external` would be two settings, not a line break.
func TestWrapTextBreaksOnlyBetweenTokens(t *testing.T) {
	text := "cert-manager: box does not install it yet; set platform.cert-manager.mode=external and check /var/data/s5n/config/radarr-4k"
	width := 48
	lines := wrapText(text, width)
	if len(lines) < 2 {
		t.Fatalf("text was not wrapped at %d: %q", width, lines)
	}
	words := strings.Fields(text)
	inWords := func(token string) bool {
		for _, w := range words {
			if w == token {
				return true
			}
		}
		return false
	}
	for _, line := range lines {
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("line is %d columns wide, want at most %d: %q", w, width, line)
		}
		for _, token := range strings.Fields(line) {
			if !inWords(token) {
				t.Fatalf("line splits a token: %q in %q", token, line)
			}
		}
	}
	if got, want := strings.Join(lines, " "), text; got != want {
		t.Fatalf("wrapped text does not rejoin:\n got: %q\nwant: %q", got, want)
	}
}

// Only a token that cannot fit a line at all is hard-broken, and then nothing
// may overflow the terminal.
func TestWrapTextHardBreaksOnlyAnOverlongToken(t *testing.T) {
	const long = "supercalifragilisticexpialidocious"
	for _, width := range []int{4, 8, 12} {
		lines := wrapText(long, width)
		if len(lines) < 2 {
			t.Fatalf("width %d did not break an overlong token: %q", width, lines)
		}
		for _, line := range lines {
			if w := lipgloss.Width(line); w > width {
				t.Fatalf("width %d: line is %d columns: %q", width, w, line)
			}
		}
		if got := strings.Join(lines, ""); got != long {
			t.Fatalf("hard break lost characters: %q", got)
		}
	}
}

// A list wraps between its items, never inside one, and keeps the separator on
// the line it belongs to.
func TestWrapListKeepsNamesWhole(t *testing.T) {
	names := []string{"data", "config", "config/qbittorrent", "config/sonarr", "config/radarr-4k", "config/prowlarr"}
	lines := wrapList(names, " · ", 34)
	if len(lines) < 2 {
		t.Fatalf("list was not wrapped: %q", lines)
	}
	for _, line := range lines {
		if w := lipgloss.Width(line); w > 34 {
			t.Fatalf("line is %d columns wide: %q", w, line)
		}
		for _, part := range strings.Split(line, " · ") {
			found := false
			for _, name := range names {
				if part == name {
					found = true
				}
			}
			if !found {
				t.Fatalf("list split a name: %q in %q", part, line)
			}
		}
	}
	if got := strings.Join(lines, " · "); got != strings.Join(names, " · ") {
		t.Fatalf("list does not join back:\n got: %q\nwant: %q", got, strings.Join(names, " · "))
	}
}
