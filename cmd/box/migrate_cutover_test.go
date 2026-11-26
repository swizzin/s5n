package main

// migrate_cutover_test.go drives `box migrate cutover`'s two outcomes over a
// saved read and an injected stop proof: a source that still runs is refused
// with a survey written first and the stop command printed, and a source that
// has stopped gets the final snapshot, which is what apply runs. No cluster is
// involved: the proof is the only thing that reads one.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
	reportfile "github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/say"
)

// proofOf answers the two live-cluster checks: the stop proof with one fixed
// answer, and the keep path's pre-flight with none, so a run that keeps the cluster
// goes on to the install.
func proofOf(running ...discover.Running) cutoverSeams {
	return cutoverSeams{
		proof: func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error) {
			return running, nil
		},
		keep: func(context.Context, cutoverOptions, migrationRead) ([]discover.Blocker, error) {
			return nil, nil
		},
	}
}

// recording is a reporter whose transcript a test reads back.
func recording() (*say.Plain, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &say.Plain{W: buf, Err: buf}, buf
}

// cutoverOptionsFor is one cutover of the saved source, writing its snapshots
// where the test can find them.
func cutoverOptionsFor(t *testing.T, dir string) cutoverOptions {
	t.Helper()
	o := cutoverOptions{discoverOptions: readOf(t)}
	o.FromDir = dir
	o.Root = dir
	return o
}

func TestCutoverRefusesWhileAnAppRunsAndWritesTheSurveyFirst(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = out
	o.Replace = true
	reporter, text := recording()

	running := discover.Running{
		App: "sonarr", Kind: "Deployment", Namespace: "media", Name: "sonarr",
		Replicas: 1, Pods: []string{"sonarr-abc"},
	}
	err := cutover(context.Background(), o, reporter, proofOf(running))
	if !errors.Is(err, errCutoverStillRunning) {
		t.Fatalf("cutover = %v, want the still-running refusal", err)
	}
	// Nothing apply could run was written: no final snapshot at all.
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused cutover wrote the final snapshot: %v", err)
	}
	// The read was kept first, so following the command it printed is safe.
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	var surveys []string
	for _, e := range entries {
		m, err := archive.Read(filepath.Join(filepath.Dir(out), e.Name()))
		if err != nil {
			continue
		}
		if m.Mode == archive.ModeSurvey {
			surveys = append(surveys, e.Name())
		}
	}
	if len(surveys) != 1 {
		t.Fatalf("the refusal wrote %d surveys, want one: %v", len(surveys), surveys)
	}
	// What it says: one line of state and the command that stops it (rule 3),
	// with the survey kept but off the screen.
	for _, want := range []string{
		"! 1 app running · stop them, then run this again",
		"  $ kubectl -n media scale deployment --replicas=0 sonarr",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text.String(), "survey: ") {
		t.Errorf("the refusal puts the survey's path on the screen, and plumbing belongs in the log:\n%s", text)
	}
}

func TestCutoverWritesTheFinalSnapshotWhenEveryAppStopped(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = out
	o.Replace = true
	// These two drive the snapshot itself, not the read on record: the source has
	// never been read before, and the operator's flag says so.
	o.NoPriorDiscover = true
	reporter, text := recording()

	err := cutover(context.Background(), o, reporter, proofOf())
	if !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	m, err := archive.Read(out)
	if err != nil {
		t.Fatalf("the final snapshot does not read: %v", err)
	}
	if m.Mode != archive.ModeFinal {
		t.Errorf("mode = %q, want %q", m.Mode, archive.ModeFinal)
	}
	// The final snapshot is the one apply runs, so it carries the configs.
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"), nil, nil)
	if err != nil {
		t.Fatalf("the final snapshot does not open: %v", err)
	}
	if len(opened.Configs) != 2 {
		t.Errorf("the final snapshot carries %d configs, want both apps'", len(opened.Configs))
	}
	// The replace path stops after the snapshot with the two things left for
	// the operator: the reset, to double-check, and the next command. This
	// source is a saved directory with no Node objects, so the recipe is the
	// one that admits box cannot place the cluster rather than a k0s teardown
	// for a cluster nobody proved is k0s (TestResetRecipeNamesTheSourcesNode
	// covers the recipes box does know).
	for _, want := range []string{
		"! this cluster is yours to replace",
		"· remove this cluster the way it was installed, before anything else uses this host",
		"$ box migrate apply \\",
		filepath.Base(out),
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the replace path does not say %q:\n%s", want, text)
		}
	}
}

func TestCutoverCallsOutWhatItCouldNotSettle(t *testing.T) {
	manifests, root := unsettledSource(t)
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = out
	o.Replace = true
	o.NoPriorDiscover = true
	reporter, text := recording()

	if err := cutover(context.Background(), o, reporter, proofOf()); !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"), nil, nil)
	if err != nil {
		t.Fatalf("the final snapshot does not open: %v", err)
	}
	report := string(opened.Report)
	for _, want := range []string{"\nchecks\n", "left as it is", "/mnt/other/shows"} {
		if !strings.Contains(report, want) {
			t.Errorf("the snapshot's report does not say %q:\n%s", want, report)
		}
	}
	// The report is beside the snapshot too, and the two are one document: the
	// entry travels with the file, the file is what a person opens.
	beside, err := os.ReadFile(reportfile.Name(out))
	if err != nil {
		t.Fatalf("no report beside the snapshot: %v", err)
	}
	if string(beside) != report {
		t.Errorf("the report beside the snapshot is not the one inside it:\n%s", beside)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	if got := p.Defaults(); len(got) != 1 || !strings.Contains(got[0], "/mnt/other/shows") {
		t.Errorf("the plan's defaults = %v, want the path nothing covered", got)
	}
	// Unattended, the run says how many decisions it took and points at the
	// page that names each one: a default is never silent, and the screen is
	// counts (rules 4 and 7).
	lines := text.String()
	if !strings.Contains(lines, "! defaults       1 default taken · the report names each one") {
		t.Errorf("the run does not say how many defaults it took:\n%s", lines)
	}
	if !strings.Contains(report, "/mnt/other/shows") {
		t.Errorf("the report does not name the default it took:\n%s", report)
	}
}

func TestCutoverRefusesASavedReadForTheStopProof(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	reporter, _ := recording()
	err := cutover(context.Background(), o, reporter, cutoverSeams{proof: unstopped})
	if err == nil || !strings.Contains(err.Error(), "--from-dir") {
		t.Fatalf("cutover of a saved read = %v, want it to name --from-dir", err)
	}
}

func TestResetRecipeNamesTheSourcesNode(t *testing.T) {
	tests := []struct {
		name  string
		nodes []discover.Node
		want  string
	}{
		{"k0s", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1+k0s.0"}}, "sudo k0s stop; sudo k0s reset"},
		{"k3s", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1+k3s1"}}, "/usr/local/bin/k3s-uninstall.sh"},
		{"no marker", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1"}}, "remove this cluster the way it was installed"},
		{"no nodes", nil, "remove this cluster the way it was installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps := resetSteps(tt.nodes)
			if len(steps) == 0 {
				t.Fatal("the recipe is empty")
			}
			if step := steps[0]; !strings.Contains(step.Text, tt.want) {
				t.Errorf("teardown = %q, want it to name %q", step.Text, tt.want)
			}
			for _, step := range steps {
				if tt.want == "/usr/local/bin/k3s-uninstall.sh" && strings.Contains(step.Text, "k0s ") {
					t.Errorf("k3s's recipe carries a k0s command: %q", step.Text)
				}
				if tt.want == "sudo k0s stop; sudo k0s reset" && strings.Contains(step.Text, "k3s-uninstall") {
					t.Errorf("k0s's recipe carries a k3s command: %q", step.Text)
				}
			}
			// A recipe box cannot place is a note, not a command: a `$` line is
			// only ever something to copy and run.
			if tt.want == "remove this cluster the way it was installed" && steps[0].Command {
				t.Errorf("the cannot-tell recipe is printed as a command: %q", steps[0].Text)
			}
		})
	}
	// The k0s recipe is the one the k0s.yaml belongs to: `k0s reset` leaves the
	// config behind and bootstrap refuses over it.
	k0s := resetSteps([]discover.Node{{Name: "box", KubeletVersion: "v1.34.1+k0s.0"}})
	joined := k0s[0].Text + " " + k0s[1].Text
	if !strings.Contains(joined, "rm -f /etc/k0s/k0s.yaml") {
		t.Errorf("the k0s recipe leaves /etc/k0s/k0s.yaml behind, and the next bootstrap refuses over it: %q", joined)
	}
}
