package main

import (
	"errors"
	"strings"
	"testing"
)

// The one error an operator can act on: the API server refused the port box
// picked because its range is not the default, and the raw message names no
// setting at all.
func TestPortRangeHintNamesTheFlag(t *testing.T) {
	raw := errors.New(`release s5n failed: Service "qbittorrent-peers" is invalid: ` +
		`spec.ports[0].nodePort: Invalid value: 30042: provided port is not in the valid range. ` +
		`The range of valid ports is 31000-32000`)
	got := portRangeHint(raw)
	if got == nil {
		t.Fatal("portRangeHint swallowed the error")
	}
	for _, want := range []string{
		"provided port is not in the valid range",
		"--service-node-port-range",
		"/etc/k0s/k0s.yaml",
	} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", got, want)
		}
	}
	// The API server's own words stay: the range it reports is how the operator
	// finds the setting.
	if !strings.Contains(got.Error(), "The range of valid ports is 31000-32000") {
		t.Fatalf("error = %q, want the API server's own range kept", got)
	}
}

// Everything else comes back exactly as it was: this is a translation for one
// message, not a wrapper that rewrites apply's failures.
func TestPortRangeHintLeavesOtherErrorsAlone(t *testing.T) {
	raw := errors.New("release s5n failed: context deadline exceeded")
	if got := portRangeHint(raw); got != raw {
		t.Fatalf("portRangeHint rewrote %q as %q", raw, got)
	}
	if got := portRangeHint(nil); got != nil {
		t.Fatalf("portRangeHint(nil) = %v, want nil", got)
	}
}

// Applying the hint twice is not two hints: the sentence box adds already names
// the flag, so a caller that wraps an already-wrapped error changes nothing.
func TestPortRangeHintIsNotDoubled(t *testing.T) {
	raw := errors.New("provided port is not in the valid range. The range of valid ports is 30000-32767")
	once := portRangeHint(raw)
	twice := portRangeHint(once)
	if twice.Error() != once.Error() {
		t.Fatalf("the hint was added twice:\n%s\n---\n%s", once, twice)
	}
}
