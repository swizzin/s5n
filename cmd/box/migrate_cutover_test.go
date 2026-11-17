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
	"github.com/swizzin/s5n/internal/say"
)

// proofOf answers the stop proof with one fixed answer.
func proofOf(running ...discover.Running) func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error) {
	return func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error) {
		return running, nil
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
	// What it says: the app, what is running, and the command to double-check.
	for _, want := range []string{
		"sonarr: 1 replica, 1 pod left in media/sonarr",
		"kubectl -n media scale deployment/sonarr --replicas=0",
		"box never stops a source",
		"survey: ",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
}

func TestCutoverWritesTheFinalSnapshotWhenEveryAppStopped(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = out
	o.Replace = true
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
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
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
		"box can't tell how this cluster was installed",
		"box migrate apply " + out,
		"box does not remove a cluster it did not install",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the replace path does not say %q:\n%s", want, text)
		}
	}
}

// TestCutoverRefusesASavedReadForTheStopProof: the proof is the reason cutover
// exists, so a read with no pods behind it is refused rather than treated as
// stopped.
func TestCutoverRefusesASavedReadForTheStopProof(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	reporter, _ := recording()
	err := cutover(context.Background(), o, reporter, unstopped)
	if err == nil || !strings.Contains(err.Error(), "--from-dir") {
		t.Fatalf("cutover of a saved read = %v, want it to name --from-dir", err)
	}
}

// TestResetRecipeNamesTheSourcesNode: the teardown printed for the operator is
// the one for the distribution the source's own nodes say installed it, and a
// cluster box cannot place gets a line that says so. The versions are the shapes
// k0s and k3s write, not a cluster this test ran against.
func TestResetRecipeNamesTheSourcesNode(t *testing.T) {
	o := cutoverOptions{discoverOptions: discoverOptions{Out: "/var/lib/s5n/migrate/migrate-20261117T090000Z-final.tar.gz"}}
	tests := []struct {
		name  string
		nodes []discover.Node
		want  string
	}{
		{"k0s", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1+k0s.0"}}, "k0s stop; k0s reset"},
		{"k3s", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1+k3s1"}}, "/usr/local/bin/k3s-uninstall.sh"},
		{"no marker", []discover.Node{{Name: "box", KubeletVersion: "v1.34.1"}}, "box can't tell how this cluster was installed"},
		{"no nodes", nil, "box can't tell how this cluster was installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := resetLines(o, tt.nodes)
			if len(lines) < 2 {
				t.Fatalf("the recipe is %v, want a teardown and where the manifests are", lines)
			}
			if !strings.Contains(lines[0], tt.want) {
				t.Errorf("teardown = %q, want it to name %q", lines[0], tt.want)
			}
			// Whatever the distribution, the last line is the same and is the
			// one that says the source is still there.
			if !strings.Contains(lines[len(lines)-1], "manifest") {
				t.Errorf("the recipe does not say where the source's manifests are: %v", lines)
			}
			// And a recipe for one distribution never carries another's
			// command.
			for _, line := range lines {
				if tt.want == "/usr/local/bin/k3s-uninstall.sh" && strings.Contains(line, "k0s ") {
					t.Errorf("k3s's recipe carries a k0s command: %q", line)
				}
				if tt.want == "k0s stop; k0s reset" && strings.Contains(line, "k3s-uninstall") {
					t.Errorf("k0s's recipe carries a k3s command: %q", line)
				}
			}
		})
	}
}
