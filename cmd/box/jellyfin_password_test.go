package main

// The one app credential box prints: the person's Jellyfin password, and only
// on the run that generated it. It is shown on screen and kept out of the run
// log, like the login's own.

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/runlog"
	"github.com/swizzin/s5n/internal/say"
)

func TestTheJellyfinPasswordIsShownOnceAndNeverLogged(t *testing.T) {
	dir := t.TempDir()
	lg, err := runlog.OpenAt(dir, "init")
	if err != nil {
		t.Fatal(err)
	}
	old := runLog
	runLog = lg
	defer func() { runLog = old }()

	var screen bytes.Buffer
	out := &say.Plain{W: &screen, Log: lg}
	showJellyfinPassword(out, release.Result{JellyfinPassword: "generated-person-password", JellyfinLogin: "alice"})
	// A later run has nothing: the key is the Secret's and the person has it.
	showJellyfinPassword(out, release.Result{})
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(screen.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("the screen carries %d lines, want the password once:\n%s", len(lines), screen.String())
	}
	if !strings.Contains(lines[0], "generated-person-password") || !strings.Contains(lines[0], "as alice") {
		t.Errorf("the line does not name the password and its account: %q", lines[0])
	}
	body, err := os.ReadFile(lg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "generated-person-password") {
		t.Errorf("the run log carries the password:\n%s", body)
	}
	if !strings.Contains(string(body), runlog.Redacted) {
		t.Errorf("the run log did not record that a password was shown:\n%s", body)
	}
}
