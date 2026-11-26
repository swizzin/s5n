package main

// migrate_version_test.go proves the one refusal the version column can make: a
// release older than the version that wrote the source's config stops the run
// before anything is shown or moved, names every app with both versions and the
// command that runs it anyway, and --yolo is that command.
//
// The column is read from the plan and the chart alone (planFacts), so the tests
// drive the installer half with the pass that decides the platform and the
// install itself rigged to fail if they are reached: a refusal that got as far
// as either would have staged a seedbox's configs, probed the cluster for a
// platform, or installed one, to say no to a fact about two image references.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/say"
)

// downgradeVersions is the version column of a run over three apps: one the
// release moves forward, one it moves back, and one whose source version could
// not be read.
func downgradeVersions() []apply.AppVersion {
	return []apply.AppVersion{
		{App: "qui"},
		{App: "radarr", Source: "5.20.1", Box: "7.0.3", Known: true, Cmp: 1},
		{App: "sonarr", Source: "9.6", Box: "5.0", Known: true, Cmp: -1},
	}
}

// downgradeFixture is the summary the confirm is shown on a host that already has
// a box: the same three apps' rows, each with the column the refusal read.
func downgradeFixture() apply.Summary { return versionFixture(-1) }

// versionFixture is that summary with sonarr's direction set: -1 a downgrade, 1
// an upgrade. radarr always moves forward and qui's source version could not be
// read, so one summary carries all three cases.
func versionFixture(sonarrCmp int) apply.Summary {
	s := confirmFixture()
	for i := range s.Instances {
		switch s.Instances[i].App {
		case "sonarr":
			s.Instances[i].Version = apply.AppVersion{App: "sonarr", Source: "9.6", Box: "5.0", Known: true, Cmp: sonarrCmp}
			if sonarrCmp > 0 {
				s.Instances[i].Version = apply.AppVersion{App: "sonarr", Source: "4.0.14", Box: "5.0", Known: true, Cmp: sonarrCmp}
			}
		case "radarr":
			s.Instances[i].Version = apply.AppVersion{App: "radarr", Source: "5.20.1", Box: "7.0.3", Known: true, Cmp: 1}
		case "qui":
			s.Instances[i].Version = apply.AppVersion{App: "qui"}
		}
	}
	return s
}

// untouchedSteps is the installer half of a run that must not get past the
// refusal: the pre-install pass (where the platform plan is decided) and the
// install (the platform layer, the box and the release) both fail the test by
// being called, and reached says which one was.
func untouchedSteps(t *testing.T, versions []apply.AppVersion, reached *string) migrateInstallSteps {
	t.Helper()
	return migrateInstallSteps{
		versions: versions,
		survey: func(context.Context) (apply.Summary, *platform.Plan, error) {
			*reached = "the pre-install pass, which decides the platform to install"
			return apply.Summary{}, nil, nil
		},
		install: func(context.Context) error {
			*reached = "the install, which is the platform layer and the box"
			return nil
		},
		lead: "nothing is installed yet: this run installs the platform layer, writes the box, and then moves the snapshot into it",
	}
}

func TestADowngradeRefusesBeforeAnythingMoves(t *testing.T) {
	var out bytes.Buffer
	reached := ""
	ask := &countingAsker{}
	screen := confirmScreen{Snapshot: "/var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz"}
	steps := untouchedSteps(t, downgradeVersions(), &reached)
	agreed, err := steps.beforeInstall(context.Background(), ask, askOptions{}, "s5n-alice", screen, &say.Plain{W: &out})
	if agreed != nil {
		t.Error("a refused run handed back a summary it never agreed to")
	}
	if !errors.Is(err, errDowngrade) {
		t.Fatalf("the refusal is %v, want the downgrade's", err)
	}
	if !say.ReportedBy(err) {
		t.Error("the refusal is not marked as already on screen, so it would be printed twice")
	}
	if reached != "" {
		t.Errorf("a downgrade reached %s", reached)
	}
	if len(ask.questions) != 0 {
		t.Error("a downgrade was still put to the operator as a question")
	}
	want := strings.Join([]string{
		"✗ sonarr         9.6 → 5.0",
		"                 the config is newer than the app",
		"  $ box migrate apply \\",
		"      /var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz --yolo",
		"· nothing has moved",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Fatalf("the refusal:\n%s\nwant exactly:\n%s", got, want)
	}
}

func TestADowngradeIsFoundFromThePlanAndTheChart(t *testing.T) {
	p := &plan.Plan{Apps: map[string]plan.App{
		"sonarr": {Image: "ghcr.io/example/sonarr:version-4.0.14.2939"},
		"radarr": {Image: "ghcr.io/example/radarr:version-2.0.0.1234"},
	}}
	images := func(app string) string {
		switch app {
		case "sonarr":
			return "ghcr.io/example/sonarr:5.0.0.3041@sha256:" + strings.Repeat("a", 64)
		case "radarr":
			return "ghcr.io/example/radarr:1.0.0"
		}
		return ""
	}
	versions := apply.Versions(p, []string{"radarr", "sonarr"}, images)
	if len(versions) != 2 {
		t.Fatalf("the column has %d rows, want one per planned app: %+v", len(versions), versions)
	}
	if got := versions[0]; got.App != "radarr" || !got.Downgrade() || got.Source != "2.0" || got.Box != "1.0" {
		t.Errorf("radarr's row is %+v, want 2.0 → 1.0 and a downgrade", got)
	}
	if got := versions[1]; got.App != "sonarr" || got.Downgrade() || got.Source != "4.0.14" || got.Box != "5.0" {
		t.Errorf("sonarr's row is %+v, want 4.0.14 → 5.0 and no downgrade", got)
	}
	// A chart with no image for the app is a `?`, not a refusal: box does not
	// stop a migration over a version it could not read.
	if got := apply.Versions(p, []string{"sonarr"}, func(string) string { return "" })[0]; got.Known || got.Cell() != "?" {
		t.Errorf("a row with no release image is %+v, want a question mark", got)
	}
	if got := apply.Downgrades(versions); len(got) != 1 || got[0].App != "radarr" {
		t.Errorf("the downgrades are %+v, want radarr's one row", got)
	}
}

func TestYoloRunsADowngradeWithARowForIt(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	steps := untouchedSteps(t, downgradeVersions(), new(string))
	steps.survey = func(context.Context) (apply.Summary, *platform.Plan, error) {
		return downgradeFixture(), nil, nil
	}
	if _, err := steps.beforeInstall(context.Background(), ask, askOptions{AllowDowngrades: true}, "s5n-alice", confirmScreen{}, &say.Plain{W: &out}); err != nil {
		t.Fatalf("confirm: %v\n%s", err, out.String())
	}
	if len(ask.questions) != 1 {
		t.Fatalf("a --yolo run asked %d questions, want one", len(ask.questions))
	}
	if !strings.Contains(out.String(), "! sonarr          9.6 → 5.0 · the config is newer than the app") {
		t.Errorf("the screen does not carry the downgrade as a row that needs a person:\n%s", out.String())
	}
	// An upgrade and an unreadable source version are not rows: one is the
	// normal direction and the other says nothing.
	if strings.Contains(out.String(), "! qui") || strings.Contains(out.String(), "! radarr  5.20.1") {
		t.Errorf("a row that needs nobody is in the needs-you block:\n%s", out.String())
	}
}

func TestAnUpgradeIsNotARefusal(t *testing.T) {
	versions := downgradeVersions()
	for i := range versions {
		if versions[i].App == "sonarr" {
			versions[i] = apply.AppVersion{App: "sonarr", Source: "4.0.14", Box: "5.0.0", Known: true, Cmp: 1}
		}
	}
	var out bytes.Buffer
	asked := 0
	steps := untouchedSteps(t, versions, new(string))
	steps.survey = func(context.Context) (apply.Summary, *platform.Plan, error) {
		asked++
		return versionFixture(1), nil, nil
	}
	if _, err := steps.beforeInstall(context.Background(), &countingAsker{}, askOptions{}, "s5n-alice", confirmScreen{}, &say.Plain{W: &out}); err != nil {
		t.Fatalf("an upgrade was refused: %v", err)
	}
	if asked != 1 {
		t.Errorf("the pre-install pass ran %d times, want once", asked)
	}
}

func TestAHostWithABoxRefusesAtTheConfirm(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{}, nil, "", confirmScreen{})
	ok, err := confirm(downgradeFixture())
	if ok {
		t.Fatal("a downgrade was agreed to")
	}
	if !errors.Is(err, errDowngrade) {
		t.Fatalf("the refusal is %v, want the downgrade's", err)
	}
	if len(ask.questions) != 0 {
		t.Error("a downgrade was put to the operator as a question")
	}
	if strings.Contains(out.String(), "apps ") {
		t.Error("a refused run showed the plan as if it were to be applied")
	}
}
