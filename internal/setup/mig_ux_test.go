package setup

// The installer's half of a guided migration: `box migrate apply` on a host with no
// box runs init with the answers the plan holds and the four it cannot know, so the
// only question init asks is the one nobody answered.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrating is a guided install: a terminal to ask on, the plan's data root, the
// plan's apps off, and whichever of the four answers the test gives.
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

// With all four answers init asks nothing: the plan decided the data root and apps,
// and the notice address belongs to bootstrap's host.
func TestAGuidedMigrationAsksNothingItWasGiven(t *testing.T) {
	h := newHarness(t)
	opts := h.migrating(map[string]string{
		"domain":   "box.example.com",
		"login":    "alice",
		"password": h.passwordFile(t),
		"config":   "/srv/s5n/config",
	})
	// Root's config with no address, as bootstrap leaves it: plain init would ask
	// here, and this run must not.
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

// One answer left off is one question, with init's own prompt: the app picker and
// the data root are never asked, the plan having decided both.
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

// A run without the plan's data root is refused rather than asked: no answer a
// person could give is not already the plan's.
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

// A missing domain is init's own question, with its help line, and the answer goes
// into the grant: the plan decides no domain.
func TestAGuidedMigrationAsksForItsMissingDomain(t *testing.T) {
	h := newHarness(t)
	opts := h.migrating(map[string]string{
		"login":    "alice",
		"password": h.passwordFile(t),
		"config":   "/srv/s5n/config",
	})
	h.prompt.inputs = []string{"box.example.com"}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("a guided migration with no domain was refused instead of asked: %v\n%s", err, h.out)
	}
	if got, want := strings.Join(h.prompt.events, ","), "input:gateway.host"; got != want {
		t.Fatalf("questions were %s, want just the domain", got)
	}
	if !strings.Contains(h.read(h.grantPath), "host: box.example.com") {
		t.Errorf("the answered domain did not reach the grant:\n%s", h.read(h.grantPath))
	}
}
