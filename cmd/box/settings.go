package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/theme"
)

// themeName is the palette this run draws with: the values file's top-level
// `theme:` key, or the chart's default (which is s5n) when the file has none or
// does not exist yet.
//
// It is read before anything is drawn, because a theme is not a setting the UI
// can report a problem with: an unknown name would otherwise be a whole run in
// colours nobody chose. theme.Parse is the authority on the names, and its
// error is the sentence box prints, listing every name that would have worked.
func themeName(valuesPath string) (string, error) {
	values, err := seedbox.LoadValues(valuesPath)
	if err != nil {
		return "", err
	}
	name, _ := values.Map()["theme"].(string)
	if _, err := theme.Parse(name); err != nil {
		return "", err
	}
	if name = strings.TrimSpace(name); name == "" {
		// No values file yet (init has not written one) or no key in it: the
		// default pair, spelled out, so the caller never carries a magic empty
		// string around.
		return theme.Default().Name, nil
	}
	return name, nil
}

// startupTheme is themeName for a command that has not drawn anything yet: a
// name box cannot resolve stops the run here, before a reporter or a prompt
// exists, so nothing is ever shown in the wrong palette.
func startupTheme(valuesPath string) string {
	name, err := themeName(valuesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
	return name
}
