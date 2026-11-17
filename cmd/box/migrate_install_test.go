package main

// migrate_install_test.go proves the answers a host with no box is installed
// from: the plan holds the apps, the data root and the owner, and the four a
// plan cannot know are refused by name, together, when they were not given.
// Nothing here touches a host: installAnswers is the whole decision.

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// planWithData is the plan a cutover writes: one app, a data root, and the
// owner the source's files belonged to.
func planWithData() *plan.Plan {
	return &plan.Plan{
		Version: 2,
		Source:  &plan.Source{Host: "media-host", Taken: "2026-11-17T09:00:00Z", Owner: &plan.Owner{UID: 1000, GID: 1000}, Kinds: []string{"kubernetes"}},
		Data:    &plan.Data{Root: "/srv/library", Downloads: "Downloads", Media: "media"},
		Apps: map[string]plan.App{
			"sonarr": {Kind: "kubernetes", Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media"},
		},
	}
}

// installFlagSet is the four answers a plan cannot hold, all empty: each test
// fills what it is about. The two files are the ones a run already has.
func installFlagSet(t *testing.T) (*migrateApplyFlags, *installFlags) {
	t.Helper()
	ns := "seedbox"
	grant, values := "/etc/s5n/seedboxes/root.yaml", "/root/.config/box/values.yaml"
	o := &migrateApplyFlags{applyFlags: &applyFlags{grantPath: &grant, valuesPath: &values}}
	return o, &installFlags{domain: new(string), login: new(string), password: new(string), configPath: new(string), namespace: &ns}
}

func TestInstallAnswersComeFromThePlan(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	got, err := installAnswers(o, inst, planWithData())
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	joined := strings.Join(got, " ")
	// The apps go in off, and the migration switches on what it moved: an app
	// started before its config is moved would run on an empty one.
	for _, want := range []string{
		"--apps none",
		"--namespace seedbox",
		"--data-path /srv/library",
		"--config-path /srv/config",
		"--domain box.example.com",
		"--login someone",
		"--password-file /run/password",
		"--puid 1000",
		"--pgid 1000",
		"--grant /etc/s5n/seedboxes/root.yaml",
		"--values /root/.config/box/values.yaml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the answers do not carry %q: %v", want, got)
		}
	}
}

func TestInstallAnswersRefuseEveryMissingOneByName(t *testing.T) {
	o, inst := installFlagSet(t)
	_, err := installAnswers(o, inst, planWithData())
	if err == nil {
		t.Fatal("an install with no domain, login, password or config path was accepted")
	}
	// All of them at once: a refusal one per run is four runs to find out.
	for _, want := range []string{"--domain", "--login", "--password-file", "--config-path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "no box") {
		t.Errorf("the refusal does not say why it is asking: %v", err)
	}
}

func TestInstallAnswersNeedADataRoot(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	p := planWithData()
	p.Data = nil
	_, err := installAnswers(o, inst, p)
	if err == nil || !strings.Contains(err.Error(), "data root") {
		t.Fatalf("a plan with no data root was accepted with %v", err)
	}
}

// TestInstallAnswersLeaveTheOwnerToHostFoundations: a plan with no recorded
// owner names no uid, so init settles it the way host foundations does rather
// than being told 0.
func TestInstallAnswersLeaveTheOwnerToHostFoundations(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	p := planWithData()
	p.Source = nil
	got, err := installAnswers(o, inst, p)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	for _, arg := range got {
		if arg == "--puid" || arg == "--pgid" {
			t.Errorf("the answers name an owner the plan did not: %v", got)
		}
	}
}
