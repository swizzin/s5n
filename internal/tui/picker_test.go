package tui

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/swizzin/s5n/internal/setup"
)

// pickerApps is the test's list: one short, distinct name per app, so counting a
// name counts one row.
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

// Drives the app picker the way a person moves down it and reads what the
// terminal would show.
//
// It pins the redraw bug: the hint and the first apps repeated above the list and
// stale rows left behind. huh's Height is the whole field ("including padding"),
// and its viewport is that minus title and description, so one row per app was one
// short: the viewport scrolled past the bottom and bubbletea's inline renderer left
// the rows the scroll created on screen.
func TestPickerKeepsEveryAppOnScreen(t *testing.T) {
	u := New(Options{In: strings.NewReader(""), Out: &bytes.Buffer{}, Width: 100, NoColor: true})
	var value []string
	field := u.pickerField(setup.MultiSelect{Key: "apps", Options: pickerApps}, optionKeys(pickerApps), &value)
	// What the form does before handing the field to the terminal.
	field.WithTheme(huh.ThemeFunc(func(bool) *huh.Styles { return u.styles }))
	field.WithKeyMap(keyMap())
	field.WithWidth(100)
	field.Focus()

	// One frame per move: the state that scrolls is the state that redraws badly.
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

	// The last option is under the cursor, with the whole list in the frame.
	lines := strings.Split(strings.TrimRight(stripANSI(views[len(views)-1]), " \n"), "\n")
	if len(lines) != len(pickerApps)+1 {
		t.Fatalf("the picker is %d lines for a hint and %d apps:\n%s", len(lines), len(pickerApps), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[len(lines)-1], "▸") || !strings.Contains(lines[len(lines)-1], pickerApps[len(pickerApps)-1].Label) {
		t.Fatalf("the cursor is not on the last app:\n%s", strings.Join(lines, "\n"))
	}
}
