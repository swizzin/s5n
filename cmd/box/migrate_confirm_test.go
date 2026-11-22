package main

// migrate_confirm_test.go proves the run asks one question. A migrate apply on
// a host with no box installs a cluster, a platform and an empty seedbox, and
// then adopts into it: the platform plan and the migration's own summary are
// one decision, so they are one form, shown once, and --yes answers it without
// asking at all.

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

// countingAsker answers yes and remembers every question it was asked.
type countingAsker struct {
	questions []string
}

func (c *countingAsker) Ask(question string) (bool, error) {
	c.questions = append(c.questions, question)
	return true, nil
}

// installedPlatform is a plan with everything on it, as a fresh host gets.
func installedPlatform(t *testing.T) *platform.Plan {
	t.Helper()
	pins := release.Versions{
		Authelia:     "docker.io/example/authelia:1.0.0@sha256:" + strings.Repeat("d", 64),
		K0s:          "v9.8.0+k0s.1",
		EnvoyGateway: "1.9.3",
		MetalLB:      "0.16.1",
		CertManager:  "v1.21.3",
	}
	plan, err := platform.BuildPlan(pins, []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "install", Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentLoadBalancer, Action: "install", Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentGateway, Action: "install", Mode: bootstrap.ModeAuto},
		{Component: bootstrap.ComponentCertManager, Action: "install", Mode: bootstrap.ModeAuto},
	}, "eg", "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return &plan
}

// migratingSummary is what the confirm shows after the platform: one app with
// one rewrite, carried, and one data rename.
func migratingSummary() apply.Summary {
	return apply.Summary{
		Instances: []apply.InstanceSummary{{
			App:     "sonarr",
			Staged:  apply.Staged{App: "sonarr", Files: 12, Bytes: 5 << 20},
			Changes: []rewrite.Change{{File: "config.xml", Where: "UrlBase", What: "url base", Count: 1}},
			Enable:  true,
		}},
		Renames: []string{"Downloads to downloads"},
	}
}

// TestOneConfirmForTheWholeRun: a guided run is asked once, and the platform
// plan is the first thing in that question -- not a question of its own and not
// printed outside it. The first line says nothing has been installed yet, which
// is what makes the answer the whole run's.
func TestOneConfirmForTheWholeRun(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", false, false, installedPlatform(t))

	ok, err := confirm(migratingSummary())
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !ok {
		t.Fatal("the one question was answered no")
	}
	if len(ask.questions) != 1 {
		t.Fatalf("the run asked %d questions (%v), want one", len(ask.questions), ask.questions)
	}
	if want := "move these into s5n-alice?"; ask.questions[0] != want {
		t.Errorf("question = %q, want %q", ask.questions[0], want)
	}
	printed := out.String()
	// The screen says the run has not written anything yet, before it shows
	// what the yes installs.
	lead := strings.Index(printed, "nothing is installed yet")
	if lead < 0 {
		t.Fatalf("the confirm does not say the run has installed nothing yet:\n%s", printed)
	}
	planAt := strings.Index(printed, "platform:")
	migrationAt := strings.Index(printed, "1 app\n")
	if planAt < 0 {
		t.Fatalf("the one confirm does not show the platform plan:\n%s", printed)
	}
	if migrationAt < 0 {
		t.Fatalf("the one confirm does not show the migration:\n%s", printed)
	}
	if lead > planAt || planAt > migrationAt {
		t.Fatalf("the platform plan is not shown first, after the lead line:\n%s", printed)
	}
	// The platform plan is the plan's own items, not a summary of them.
	if !strings.Contains(printed, "Envoy Gateway 1.9.3 (the front door)") {
		t.Errorf("the platform block is not the platform plan:\n%s", printed)
	}
	// The old two-question transcript is gone: nothing prints a platform plan
	// heading outside the confirm, and no summary line is cut off by it.
	if strings.Contains(printed, "platform plan:") {
		t.Errorf("the platform plan is printed with its own heading, which is the block the confirm replaced:\n%s", printed)
	}
	if !strings.Contains(printed, "sonarr") || !strings.Contains(printed, "Downloads → downloads") {
		t.Errorf("the migration summary did not reach the one confirm:\n%s", printed)
	}
}

// TestTheOneConfirmWithoutAPlatform: a run that adopted into an existing box
// installed no platform, so the question shows only the migration -- and does not
// claim it is about to install anything.
func TestTheOneConfirmWithoutAPlatform(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "seedbox", false, false, nil)

	if _, err := confirm(migratingSummary()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(ask.questions) != 1 {
		t.Fatalf("the run asked %d questions (%v), want one", len(ask.questions), ask.questions)
	}
	if strings.Contains(out.String(), "platform:") {
		t.Errorf("a run that installed no platform shows a platform block:\n%s", out.String())
	}
	if strings.Contains(out.String(), "nothing is installed yet") {
		t.Errorf("a run that installed nothing says it is about to install something:\n%s", out.String())
	}
}

// TestTheAgreedSummaryIsNotAskedAgain: on a host that had no box the question was
// asked before anything was installed, so the run that follows holds itself to
// that answer. It asks nothing, it refuses a summary that is not the one agreed
// to, and it fills in the rows the question deferred.
func TestTheAgreedSummaryIsNotAskedAgain(t *testing.T) {
	agreed := apply.Summary{
		Instances: []apply.InstanceSummary{
			{App: "plex", Staged: apply.Staged{App: "plex", Files: 7, Bytes: 2 << 20}, Deferred: "its databases are checked once Plex's image runs", Enable: true},
			{App: "sonarr", Staged: apply.Staged{App: "sonarr", Files: 12, Bytes: 5 << 20}, Enable: true},
		},
	}
	// What the run after the install shows: Plex's check has run, and it found
	// paths; nothing else moved.
	now := agreed
	now.Instances = append([]apply.InstanceSummary(nil), agreed.Instances...)
	for i := range now.Instances {
		if now.Instances[i].App != "plex" {
			continue
		}
		now.Instances[i].Deferred = ""
		now.Instances[i].Changes = []rewrite.Change{{File: "library.db", Where: "section_locations.root_path", What: "path", Count: 8264}}
	}

	var out bytes.Buffer
	ok, err := migrateAgreed(&say.Plain{W: &out}, agreed)(now)
	if err != nil {
		t.Fatalf("the run after the install was refused: %v", err)
	}
	if !ok {
		t.Fatal("the run after the install answered no")
	}
	printed := out.String()
	if !strings.Contains(printed, "checked after the install") || !strings.Contains(printed, "8,264 paths") {
		t.Errorf("the row the question deferred is not filled in after the check:\n%s", printed)
	}
	// No question: this confirmer is not given one, and a run that asked again
	// would have to be. What it must not do is print a second summary.
	if strings.Contains(printed, "move these into") {
		t.Errorf("the run asked the question it had already been answered:\n%s", printed)
	}

	// One rewrite more anywhere else, and the run stops rather than doing what
	// nobody was shown.
	differed := now
	differed.Instances = append([]apply.InstanceSummary(nil), now.Instances...)
	for i := range differed.Instances {
		if differed.Instances[i].App != "sonarr" {
			continue
		}
		differed.Instances[i].Changes = []rewrite.Change{{File: "config.xml", Where: "UrlBase", What: "url base", Count: 1}}
	}
	if ok, err := migrateAgreed(&say.Plain{W: &bytes.Buffer{}}, agreed)(differed); err == nil || ok {
		t.Fatalf("a run that would do more than it showed was allowed: ok=%v err=%v", ok, err)
	} else if !strings.Contains(err.Error(), "nothing of the snapshot has been moved") {
		t.Errorf("the refusal does not say what state the host is in: %v", err)
	}
}

// TestAHostWithABoxAsksInsideTheRun: nothing was agreed to before an install that
// did not happen, so the run asks its one question itself -- after every check,
// including the one Plex's image makes (internal/migrate/apply proves the order;
// this is the choice between the two).
func TestAHostWithABoxAsksInsideTheRun(t *testing.T) {
	grant, values := "/etc/s5n/seedboxes/root.yaml", "/root/.config/box/values.yaml"
	o := &migrateApplyFlags{applyFlags: &applyFlags{grantPath: &grant, valuesPath: &values, yes: new(bool)}, details: new(bool)}
	// No terminal and no --yes: the asking form refuses rather than answering.
	ask := migrateAnswer(&say.Plain{W: &bytes.Buffer{}}, o, "seedbox", false, nil)
	if _, err := ask(migratingSummary()); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a host with a box did not ask: %v", err)
	}
	// A run that agreed before installing does not.
	agreed := migratingSummary()
	ask = migrateAnswer(&say.Plain{W: &bytes.Buffer{}}, o, "seedbox", false, &agreed)
	if ok, err := ask(agreed); err != nil || !ok {
		t.Fatalf("a run that already agreed asked again: %v %v", ok, err)
	}
}

// TestTheMigratingInitCannotAskItsOwnRelease: init's hand-over asks its release
// question on a box a person installed, and never when a migration runs init as
// one step of its own: the run's single confirm is asked after this, and a form
// here would be a second one.
func TestTheMigratingInitCannotAskItsOwnRelease(t *testing.T) {
	if _, ok := initApplyConfirmer(true, &say.Plain{W: &bytes.Buffer{}}, false).(answered); !ok {
		t.Fatal("a migrating init kept its own release question, so a guided migrate asks twice")
	}
	if _, ok := initApplyConfirmer(false, &say.Plain{W: &bytes.Buffer{}}, true).(answered); ok {
		t.Fatal("a plain `box init --yes` was handed the migration's already-answered confirmer")
	}
}

// TestEveryPathThatBuildsTheApplyFlagsHasTheDetailsSwitch: a cutover that
// keeps its cluster continues into `migrate apply`, and it builds its own copy
// of that command's flags. A switch missing there is a nil dereference in the
// confirm on the one path a keep-cluster cutover takes.
func TestEveryPathThatBuildsTheApplyFlagsHasTheDetailsSwitch(t *testing.T) {
	fs := flag.NewFlagSet("cutover apply", flag.ContinueOnError)
	kubeconfig := ""
	o := addCutoverApplyFlags(fs, &kubeconfig)
	if o.details == nil {
		t.Fatal("the cutover's apply flags have no --details switch, so its confirm dereferences nil")
	}
	if *o.details {
		t.Error("--details is on by default")
	}
}
