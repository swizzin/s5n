package setup

// mig_ux_test.go proves the installer's half of a guided migration: `box
// migrate apply` on a host with no box runs init as one step of its own, and
// init is given the answers the plan holds and the four a plan cannot know.
// Whatever this run already has is a decision, so the only question init asks
// is the one nobody answered.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrating is a guided install as `box migrate apply` runs one: a terminal to
// ask on (so nothing is unattended), the plan's data root, the plan's apps off,
// and whichever of the four answers the test gives.
func (h *harness) migrating(answers map[string]string) Options {
	opts := h.options()
	opts.Adopting = true
	opts.DataPath = "/srv/s5n/data"
	opts.Apps = []string{"none"}
	opts.Login = answers["login"]
	opts.Domain = answers["domain"]
	opts.ConfigPath = answers["config"]
	opts.PasswordFile = answers["password"]
	return opts
}

// passwordFile is the --password-file one of the four answers names.
func (h *harness) passwordFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(h.dir, "login-password")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAGuidedMigrationAsksNothingItWasGiven: with all four answers, init asks
// no question at all. The plan's data root and app list are not asked because
// the plan decided them, and neither is the notice address, which belongs to
// bootstrap's host rather than to this install.
func TestAGuidedMigrationAsksNothingItWasGiven(t *testing.T) {
	h := newHarness(t)
	opts := h.migrating(map[string]string{
		"domain":   "box.example.com",
		"login":    "alice",
		"password": h.passwordFile(t),
		"config":   "/srv/s5n/config",
	})
	// Root's config is there with no address, the state bootstrap leaves: the
	// ordinary `box init` asks for one here, and this run must not.
	opts.NoticePath = noticeFile(t)

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("a guided migration with every answer was refused: %v\n%s", err, h.out)
	}
	if questions := strings.Join(h.prompt.events, ", "); questions != "" {
		t.Fatalf("a guided migration asked %s, want no questions: its answers were all given", questions)
	}
	if !res.Applied {
		t.Fatal("the guided migration did not hand over to apply")
	}
	grant := h.read(h.grantPath)
	for _, want := range []string{"host: box.example.com", "login: alice", "path: /srv/s5n/config"} {
		if !strings.Contains(grant, want) {
			t.Errorf("the grant is missing %q:\n%s", want, grant)
		}
	}
	assertNoApps(t, grant)
}

// TestAGuidedMigrationAsksExactlyWhatItLacks: one answer left off is one
// question, with init's own prompt for it, and nothing else. The app picker is
// never shown and the data root is never asked -- the plan decided both.
func TestAGuidedMigrationAsksExactlyWhatItLacks(t *testing.T) {
	h := newHarness(t)
	opts := h.migrating(map[string]string{
		"domain":   "box.example.com",
		"login":    "alice",
		"password": h.passwordFile(t),
	})
	// The one answer this run does not have, typed on the terminal.
	h.prompt.inputs = []string{"/srv/s5n/config"}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("a guided migration with one answer missing was refused: %v\n%s", err, h.out)
	}
	if got, want := strings.Join(h.prompt.events, ","), "input:storage.config.path"; got != want {
		t.Fatalf("questions were %s, want just the config path", got)
	}
	if !strings.Contains(h.read(h.grantPath), "path: /srv/s5n/config") {
		t.Errorf("the answered config path did not reach the grant:\n%s", h.read(h.grantPath))
	}
}

// TestAGuidedMigrationAsksForThePlanItHasNoAnswerFor: the plan's data root is
// not a question, and a run without one is refused rather than asked -- there
// is no answer a person could give that the plan does not already own.
func TestAGuidedMigrationRefusesWithoutThePlansDataRoot(t *testing.T) {
	h := newHarness(t)
	opts := h.migrating(map[string]string{
		"domain":   "box.example.com",
		"login":    "alice",
		"password": h.passwordFile(t),
		"config":   "/srv/s5n/config",
	})
	opts.DataPath = ""

	_, err := h.run(opts)
	if err == nil {
		t.Fatalf("a migration with no data root was accepted:\n%s", h.out)
	}
	if !strings.Contains(err.Error(), "--data-path") {
		t.Errorf("the refusal does not name --data-path: %v", err)
	}
	for _, event := range h.prompt.events {
		if strings.HasPrefix(event, "input:storage.data.path") || strings.HasPrefix(event, "multi:apps") {
			t.Fatalf("the run asked %s, and the plan decides both", event)
		}
	}
}
