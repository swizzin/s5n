// Package releasetest is how a test reads the platform pins the chart ships.
//
// The pins are not compiled into box any more: every version box installs lives
// in charts/s5n/platform.yaml, inside the chart release, so a test that asserts
// against "the version box installs" has to read that file. One helper, one
// parser (release.ParseVersions, the one box uses), so the tests of the
// bootstrap plan, the platform layer, the login and hack/versions all agree
// with the chart the e2e packages.
package releasetest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

// Path is charts/s5n/platform.yaml in this checkout.
func Path(tb testing.TB) string {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("releasetest: cannot locate this package's source file")
	}
	// .../internal/release/releasetest/pins.go -> the repository root.
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "charts", "s5n", "platform.yaml")
}

// File is the file's bytes, for the tests that assert on its shape (the
// Renovate comment on each line) rather than on the parsed values.
func File(tb testing.TB) []byte {
	tb.Helper()
	data, err := os.ReadFile(Path(tb))
	if err != nil {
		tb.Fatalf("read the chart's platform pins: %v", err)
	}
	return data
}

// Pins is the parsed file: the versions the chart in this checkout installs.
func Pins(tb testing.TB) release.Versions {
	tb.Helper()
	pins, err := release.ParseVersions(File(tb))
	if err != nil {
		tb.Fatalf("parse charts/s5n/platform.yaml: %v", err)
	}
	return pins
}

// MustPins is Pins for a package-level fixture, where there is no test to fail
// yet. A checkout whose chart pins do not parse is a broken checkout, so
// panicking is the honest answer.
func MustPins() release.Versions {
	pins, err := release.ParseVersions(mustFile())
	if err != nil {
		panic("charts/s5n/platform.yaml: " + err.Error())
	}
	return pins
}

func mustFile() []byte {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("releasetest: cannot locate this package's source file")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "charts", "s5n", "platform.yaml"))
	if err != nil {
		panic("releasetest: " + err.Error())
	}
	return data
}
