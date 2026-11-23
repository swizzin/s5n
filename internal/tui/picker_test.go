package tui

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/swizzin/s5n/internal/setup"
)

// pickerApps is the picker's list in the test: one short, distinct name per
// app, so counting a name in the drawn view counts one row.
var pickerApps = []setup.Option{
	{Value: "qbittorrent", Label: "qbittorrent"},
	{Value: "sonarr", Label: "sonarr"},
	{Value: "radarr", Label: "radarr"},
	{Value: "prowlarr", Label: "prowlarr"},
	{Value: "plex", Label: "plex"},
	{Value: "seerr", Label: "seerr"},
	{Value: "tautulli", Label: "tautulli"},
	{Value: "bazarr", Label: "bazarr"},
}

// TestPickerKeepsEveryAppOnScreen drives the app picker the way a person moves
// down it, and reads what the terminal would show.
//
// It is the regression test for the redraw a person saw on her terminal: the
// hint and the first apps repeated above the list, and stale rows left behind.
// The cause is the field's height. huh's Height is the whole field (its own doc
// says "including padding"), and its option viewport is that minus the title
// and the description, so "one row per app" was one row short of the list: the
// viewport scrolled as the cursor passed the bottom, and bubbletea's inline
// renderer, which counts the lines it drew in the frame before, left the rows
// the scroll created on screen. At 100x40 the picker saw no reason to scroll,
// and every app and the hint are drawn exactly once.
func TestPickerKeepsEveryAppOnScreen(t *testing.T) {
	u := New(Options{In: strings.NewReader(""), Out: &bytes.Buffer{}, Width: 100, NoColor: true})
	var value []string
	field := u.pickerField(setup.MultiSelect{Key: "apps", Options: pickerApps}, optionKeys(pickerApps), &value)
	// What the form does before it hands the field to the terminal: box's look,
	// box's keys, and the width the terminal reported.
	field.WithTheme(huh.ThemeFunc(func(bool) *huh.Styles { return u.styles }))
	field.WithKeyMap(keyMap())
	field.WithWidth(100)
	field.Focus()

	// One frame per move, exactly as the terminal would draw them: the state
	// that scrolls is the state that redraws badly.
	views := []string{field.View()}
	for range pickerApps {
		model, _ := field.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		field = model.(*huh.MultiSelect[string])
		views = append(views, field.View())
	}

	for frame, view := range views {
		text := stripANSI(view)
		if got := strings.Count(text, multiSelectHelp); got != 1 {
			t.Fatalf("frame %d draws the hint %d times, want once:\n%s", frame, got, text)
		}
		for _, app := range pickerApps {
			if got := strings.Count(text, app.Label); got != 1 {
				t.Fatalf("frame %d draws %s %d times, want once (the list scrolled):\n%s", frame, app.Label, got, text)
			}
		}
	}

	// And the last option is the one under the cursor, with the whole list --
	// the hint and one row per app -- in the frame that has to hold it.
	lines := strings.Split(strings.TrimRight(stripANSI(views[len(views)-1]), " \n"), "\n")
	if len(lines) != len(pickerApps)+1 {
		t.Fatalf("the picker is %d lines for a hint and %d apps:\n%s", len(lines), len(pickerApps), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[len(lines)-1], "▸") || !strings.Contains(lines[len(lines)-1], pickerApps[len(pickerApps)-1].Label) {
		t.Fatalf("the cursor is not on the last app:\n%s", strings.Join(lines, "\n"))
	}
}
