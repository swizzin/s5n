package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/theme"
)

// themeName is the values' top-level `theme:` or the chart's default. Read
// before drawing: an unknown name would colour the whole run, and theme.Parse's
// error lists the names that work.
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
		// No file or no key: the spelled-out default pair.
		return theme.Default().Name, nil
	}
	return name, nil
}

// startupTheme resolves before a reporter exists, so nothing is shown wrong.
func startupTheme(valuesPath string) string {
	name, err := themeName(valuesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
	return name
}
