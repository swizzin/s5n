package main

// migrate_version_test.go proves the one refusal the version column can make: a
// release older than the version that wrote the source's config stops the run
// before anything is shown or moved, names every app with both versions and the
// command that runs it anyway, and --yolo is that command.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/say"
)

// downgradeFixture is a run over three apps: one the release moves forward, one
// it moves back, and one whose source version could not be read.
func downgradeFixture() apply.Summary {
	s := confirmFixture()
	for i := range s.Instances {
		switch s.Instances[i].App {
		case "sonarr":
			s.Instances[i].Version = apply.AppVersion{App: "sonarr", Source: "9.6", Box: "5.0", Known: true, Cmp: -1}
		case "radarr":
			s.Instances[i].Version = apply.AppVersion{App: "radarr", Source: "5.20.1", Box: "7.0.3", Known: true, Cmp: 1}
		case "qui":
			s.Instances[i].Version = apply.AppVersion{App: "qui"}
		}
	}
	return s
}

// TestADowngradeRefusesBeforeAnythingMoves: the run stops on the fact, with both
// versions on the row, one command that runs it anyway, and nothing else said.
func TestADowngradeRefusesBeforeAnythingMoves(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	screen := confirmScreen{Snapshot: "/var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz"}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{}, nil, "", screen)
	ok, err := confirm(downgradeFixture())
	if ok {
		t.Fatal("a downgrade was agreed to")
	}
	if !errors.Is(err, errDowngrade) {
		t.Fatalf("the refusal is %v, want the downgrade's", err)
	}
	if !say.ReportedBy(err) {
		t.Error("the refusal is not marked as already on screen, so it would be printed twice")
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
	if got := out.String(); !strings.HasSuffix(got, want) {
		t.Fatalf("the refusal:\n%s\nwant it to end:\n%s", got, want)
	}
	if strings.Contains(out.String(), "apps ") {
		t.Error("a refused run showed the plan as if it were to be applied")
	}
}

// TestYoloRunsADowngradeWithARowForIt: --yolo is the operator's word, and the
// screen then carries the downgrade as a row that needs them rather than a
// refusal.
func TestYoloRunsADowngradeWithARowForIt(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{AllowDowngrades: true}, nil, "", confirmScreen{})
	ok, err := confirm(downgradeFixture())
	if err != nil {
		t.Fatalf("confirm: %v\n%s", err, out.String())
	}
	if !ok || len(ask.questions) != 1 {
		t.Fatalf("a --yolo run asked %d questions and answered %v", len(ask.questions), ok)
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

// TestAnUpgradeIsNotARefusal: the ordinary case, which every real source is.
func TestAnUpgradeIsNotARefusal(t *testing.T) {
	s := downgradeFixture()
	for i := range s.Instances {
		if s.Instances[i].App == "sonarr" {
			s.Instances[i].Version = apply.AppVersion{App: "sonarr", Source: "4.0.14", Box: "5.0.0", Known: true, Cmp: 1}
		}
	}
	var out bytes.Buffer
	if _, err := migrateConfirm(&say.Plain{W: &out}, &countingAsker{}, "s5n-alice", askOptions{}, nil, "", confirmScreen{})(s); err != nil {
		t.Fatalf("an upgrade was refused: %v", err)
	}
}
