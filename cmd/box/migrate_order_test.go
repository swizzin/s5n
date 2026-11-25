package main

// migrate_order_test.go proves the order `box migrate apply` takes on a host with
// no box: the whole run is decided and shown first, the one question is asked,
// and only then is anything installed. It is the one thing about this command
// that no single step can show: a run that installed the cluster, the platform
// and the empty release before asking left a host nobody agreed to when the
// answer was no.
//
// The survey is driven twice: once with a fake, to read the order of the calls,
// and once with the real pre-install pass, to prove what it does is confined to a
// directory it removes.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/say"
)

// stepAsker answers the one question and records where in the run it happened.
type stepAsker struct {
	order []string
	yes   bool
	asked []string
}

func (a *stepAsker) Ask(question string) (bool, error) {
	a.order = append(a.order, "ask")
	a.asked = append(a.asked, question)
	return a.yes, nil
}

// TestTheQuestionComesBeforeAnythingIsInstalled: the pre-install pass, then the
// question, then the install -- and the answer is what decides whether the
// install happens at all.
func TestTheQuestionComesBeforeAnythingIsInstalled(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yes      bool
		want     string
		canceled bool
	}{
		{name: "yes", yes: true, want: "survey ask install"},
		{name: "no", yes: false, want: "survey ask", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := &stepAsker{yes: tc.yes}
			steps := recordingSteps(ask)
			agreed, err := steps.beforeInstall(context.Background(), ask, askOptions{}, "s5n-alice", confirmScreen{}, &say.Plain{W: &bytes.Buffer{}})
			assertRunOutcome(t, err, agreed, tc.canceled)
			if got := strings.Join(ask.order, " "); got != tc.want {
				t.Errorf("the run went %q, want %q", got, tc.want)
			}
			if len(ask.asked) != 1 {
				t.Fatalf("the run asked %d questions (%v), want one", len(ask.asked), ask.asked)
			}
			if want := "move these 1 app in?"; ask.asked[0] != want {
				t.Errorf("the question = %q, want %q", ask.asked[0], want)
			}
		})
	}
}

// recordingSteps is a run whose two halves record themselves in the asker's
// order, so the test reads one list.
func recordingSteps(ask *stepAsker) migrateInstallSteps {
	return migrateInstallSteps{
		survey: func(context.Context) (apply.Summary, *platform.Plan, error) {
			ask.order = append(ask.order, "survey")
			return migrationSummaryForOrder(), nil, nil
		},
		install: func(context.Context) error {
			ask.order = append(ask.order, "install")
			return nil
		},
	}
}

// assertRunOutcome is the answer's half: a cancelled run hands nothing back and
// installs nothing (the order list has no "install" in it), and a yes hands back
// the summary it installed against.
func assertRunOutcome(t *testing.T, err error, agreed *apply.Summary, canceled bool) {
	t.Helper()
	if !canceled {
		if err != nil {
			t.Fatalf("beforeInstall: %v", err)
		}
		if agreed == nil {
			t.Error("the run installed without recording what was agreed to")
		}
		return
	}
	if !errors.Is(err, apply.ErrCancelled) {
		t.Fatalf("answering no = %v, want a cancellation", err)
	}
	if agreed != nil {
		t.Error("a cancelled run handed back a summary it never agreed to")
	}
}

// migrationSummaryForOrder is a summary with one app, which is all the order test
// reads.
func migrationSummaryForOrder() apply.Summary {
	return apply.Summary{Instances: []apply.InstanceSummary{{
		App:    "sonarr",
		Staged: apply.Staged{App: "sonarr", Files: 3, Bytes: 1024},
		Enable: true,
	}}}
}

// TestThePreInstallPassWritesOnlyWhereItIsThrownAway: the real pass, over a real
// snapshot, against a config root and a grant path that must survive it
// untouched. The run's state directory under the config root is the adoption's,
// and it is not created until the answer is yes.
func TestThePreInstallPassWritesOnlyWhereItIsThrownAway(t *testing.T) {
	snapshot := preInstallSnapshot(t)
	p, raw, opened := snapshot.plan(t)
	chart := chartWithVariants(t)
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		t.Fatalf("parse the chart's catalog: %v", err)
	}
	grantPath := filepath.Join(t.TempDir(), "grant.yaml")
	configRoot := t.TempDir()
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	o := &migrateApplyFlags{applyFlags: &applyFlags{grantPath: &grantPath, valuesPath: &valuesPath}}

	summary, err := migrateSurvey(context.Background(), o, p, opened, raw, chart, cat, false)
	if err != nil {
		t.Fatalf("migrateSurvey: %v", err)
	}
	byApp := map[string]apply.InstanceSummary{}
	for _, in := range summary.Instances {
		byApp[in.App] = in
	}
	client, ok := byApp["qbittorrent"]
	if !ok {
		t.Fatalf("the summary has no qbittorrent row: %+v", summary.Instances)
	}
	if client.Staged.Files == 0 || len(client.Changes) == 0 {
		t.Errorf("the client's row counts %d files and %d changes: its check did not run", client.Staged.Files, len(client.Changes))
	}
	// Plex's row is the one the pass cannot check, and the note about what its
	// snapshot does not carry is on the same summary: both come from the
	// snapshot, and the run after the install shows the same two.
	if plex := byApp["plex"]; plex.Deferred == "" || plex.Staged.Files == 0 {
		t.Errorf("Plex's row is %+v, want a deferred check and a staged copy", plex)
	}
	if len(summary.Carried) != 1 || summary.Carried[0].App != "plex" || !strings.Contains(summary.Carried[0].Note, "carried Preferences.xml") {
		t.Errorf("the summary's notes about what the snapshot does not hold are %+v, want Plex's", summary.Carried)
	}
	// Answering no from here installs nothing and leaves both paths alone.
	installed := false
	steps := migrateInstallSteps{
		survey: func(context.Context) (apply.Summary, *platform.Plan, error) {
			return summary, nil, nil
		},
		install: func(context.Context) error {
			installed = true
			return nil
		},
	}
	if _, err := steps.beforeInstall(context.Background(), &stepAsker{}, askOptions{}, "", confirmScreen{}, &say.Plain{W: &bytes.Buffer{}}); !errors.Is(err, apply.ErrCancelled) {
		t.Fatalf("answering no = %v, want a cancellation", err)
	}
	if installed {
		t.Error("answering no installed something")
	}
	if _, err := os.Stat(grantPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pre-install pass or the answer wrote a grant at %s", grantPath)
	}
	entries, err := os.ReadDir(configRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the config root holds %d entries after a run that was answered no, want none", len(entries))
	}
}

// snapshotFixture is a snapshot laid out the way bundle.Open leaves one: the
// plan at the root, and <Dir>/config/<instance>/ per planned app. It holds the
// two apps the pre-install pass treats differently: one whose rewriter reads
// files, and Plex, whose config is the trimmed set a snapshot carries and whose
// check runs its own image.
type snapshotFixture struct {
	dir      string
	dataRoot string
	doc      string
	configs  map[string]string
}

func preInstallSnapshot(t *testing.T) *snapshotFixture {
	t.Helper()
	s := &snapshotFixture{dir: t.TempDir(), dataRoot: t.TempDir(), configs: map[string]string{}}
	plexHome := filepath.Join(s.dir, archive.ConfigPrefix, "plex", "Library", "Application Support", "Plex Media Server")
	for _, dir := range []string{
		filepath.Join(s.dataRoot, "Downloads"),
		filepath.Join(s.dataRoot, "media"),
		filepath.Join(s.dir, archive.ConfigPrefix, "qbittorrent", "qBittorrent"),
		plexHome,
		filepath.Join(plexHome, "Plug-in Support", "Databases"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.dir, archive.ConfigPrefix, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n\n[WebUI]\nPort=8080\n")
	// Plex's snapshot carries its state and nothing else: the note the confirm
	// shows comes from exactly this set.
	write("plex/Library/Application Support/Plex Media Server/Preferences.xml", `<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<Preferences MachineIdentifier="synthetic-machine" />`+"\n")
	write("plex/Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db", "")
	s.configs["qbittorrent"] = filepath.Join(s.dir, archive.ConfigPrefix, "qbittorrent")
	s.configs["plex"] = filepath.Join(s.dir, archive.ConfigPrefix, "plex")
	s.doc = `version: 2
data:
  root: ` + s.dataRoot + `
  downloads: Downloads
  media: media
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    namespace: media
    ports: [32400]
    paths:
      - from: /srv/apps/plex
        to: /config
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    namespace: media
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
`
	return s
}

// plan is the snapshot as openSnapshot would return it.
func (s *snapshotFixture) plan(t *testing.T) (*plan.Plan, []byte, *bundle.Opened) {
	t.Helper()
	raw := []byte(s.doc)
	p, err := plan.Parse(raw)
	if err != nil {
		t.Fatalf("parse the plan: %v", err)
	}
	return p, raw, &bundle.Opened{
		Path:    filepath.Join(s.dir, "snapshot.tar.gz"),
		Dir:     s.dir,
		Mode:    archive.ModeFinal,
		Plan:    raw,
		Configs: s.configs,
		// The numbers the pass that verified the snapshot counts: this fixture
		// is a snapshot's extract, so what it holds of each app is its config
		// directory's two files and its one.
		Contents: map[string]bundle.Contents{
			"plex":        {Files: 2, Bytes: 200},
			"qbittorrent": {Files: 1, Bytes: 60},
		},
	}
}
