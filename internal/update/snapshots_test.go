package update

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/snapshot"
)

// configRoot is a temp config root holding one app's two instances, with the
// files the catalog says an upgrade rewrites.
func configRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"sonarr", "sonarr-4k"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"sonarr/sonarr.db":        "hd-database",
		"sonarr/config.xml":       "<config>hd</config>",
		"sonarr-4k/sonarr.db":     "4k-database",
		"sonarr-4k/config.xml":    "<config>4k</config>",
		"sonarr/sonarr.db-shm":    "shm",
		"sonarr-4k/sonarr.db-shm": "shm",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// twoInstances is the values of an app with its 4K instance enabled.
func twoInstances() map[string]any {
	return map[string]any{"apps": map[string]any{"sonarr": map[string]any{
		"enabled": true,
		"instances": []any{
			map[string]any{"suffix": "", "enabled": true},
			map[string]any{"suffix": "4k", "enabled": true},
		},
	}}}
}

func sonarrGlobs(context.Context, string) (map[string][]string, error) {
	return map[string][]string{"sonarr": {"sonarr.db", "sonarr.db-wal", "sonarr.db-shm", "config.xml"}}, nil
}

// TestConfigSnapshotsCopiesEveryInstance: one app's snapshot is every enabled
// instance's files, in one step directory, with the images the step moved
// between recorded in each manifest. Restore puts the content back and removes a
// file the failed run added that the globs match.
func TestConfigSnapshotsCopiesEveryInstance(t *testing.T) {
	root := configRoot(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &ConfigSnapshots{ConfigRoot: root, Values: twoInstances(), Globs: sonarrGlobs, Now: func() time.Time { return at }}
	step := Step{From: "0.4.0", To: "0.5.0"}
	images := map[string]ImageChange{"sonarr": {
		Ran:     snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestA},
		MovedTo: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.1.0", Digest: digestB},
	}}

	if err := s.Check(context.Background(), step, []string{"sonarr"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	taken, err := s.Take(context.Background(), step, []string{"sonarr"}, images)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	wantStep := "0.4.0-0.5.0-" + at.Format("20060102T150405Z")
	if taken.Step != wantStep {
		t.Fatalf("step = %q, want %q", taken.Step, wantStep)
	}
	if !taken.At.Equal(at) {
		t.Fatalf("taken at %v, want %v", taken.At, at)
	}
	wantDir := filepath.Join(root, ".box", "snapshots", wantStep, "sonarr")
	if taken.Dirs["sonarr"] != wantDir {
		t.Fatalf("sonarr's copy = %q, want %q", taken.Dirs["sonarr"], wantDir)
	}
	_, man, err := snapshot.Newest(snapshot.Root(root), "sonarr")
	if err != nil {
		t.Fatalf("newest: %v", err)
	}
	if man.Image != images["sonarr"].Ran || man.MovedTo != images["sonarr"].MovedTo {
		t.Fatalf("manifest images = %+v -> %+v, want %+v -> %+v",
			man.Image, man.MovedTo, images["sonarr"].Ran, images["sonarr"].MovedTo)
	}

	// The failed run migrated one instance and left a -wal behind.
	for name, content := range map[string]string{
		"sonarr/sonarr.db":     "migrated",
		"sonarr/sonarr.db-wal": "wal",
		"sonarr-4k/sonarr.db":  "migrated-4k",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Restore(context.Background(), step, []string{"sonarr"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for name, want := range map[string]string{
		"sonarr/sonarr.db":    "hd-database",
		"sonarr-4k/sonarr.db": "4k-database",
	} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q after the restore, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sonarr/sonarr.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("the restore kept a file the globs match and the snapshot lacks: %v", err)
	}
}

// TestConfigSnapshotsRefusesAMissingInstance: an app is covered only when every
// instance of it can be copied. A missing config directory is refused before
// anything stops, because the restore could not put that instance back.
func TestConfigSnapshotsRefusesAMissingInstance(t *testing.T) {
	root := configRoot(t)
	if err := os.RemoveAll(filepath.Join(root, "sonarr-4k")); err != nil {
		t.Fatal(err)
	}
	s := &ConfigSnapshots{ConfigRoot: root, Values: twoInstances(), Globs: sonarrGlobs}
	err := s.Check(context.Background(), Step{From: "0.4.0", To: "0.5.0"}, []string{"sonarr"})
	if err == nil {
		t.Fatal("check passed although one instance's config directory is gone")
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "sonarr-4k")) {
		t.Fatalf("error = %v, want it to name the missing directory", err)
	}
}

// TestConfigSnapshotsKeepsTwoPerInstance: pruning is per config directory, so a
// box that has updated three times keeps the two newest copies of each instance.
func TestConfigSnapshotsKeepsTwoPerInstance(t *testing.T) {
	root := configRoot(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := &ConfigSnapshots{ConfigRoot: root, Values: twoInstances(), Globs: sonarrGlobs, Now: func() time.Time { return at }}
	images := map[string]ImageChange{"sonarr": {
		Ran:     snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestA},
		MovedTo: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestB},
	}}
	for i := 0; i < 3; i++ {
		at = at.Add(time.Hour)
		step := Step{From: "0.4." + strconv.Itoa(i), To: "0.5." + strconv.Itoa(i)}
		if _, err := s.Take(context.Background(), step, []string{"sonarr"}, images); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	step := Step{From: "0.4.2", To: "0.5.2"}
	if err := s.Prune(context.Background(), step, []string{"sonarr"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	steps, err := os.ReadDir(snapshot.Root(root))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range steps {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 2 {
		t.Fatalf("step directories after pruning = %v, want the two newest", dirs)
	}
}

// TestConfigSnapshotsRestoresOnlyTheStepItTook: a restore of a step this run
// never copied refuses rather than reading the newest snapshot, which holds
// state the app has already written past.
func TestConfigSnapshotsRestoresOnlyTheStepItTook(t *testing.T) {
	root := configRoot(t)
	s := &ConfigSnapshots{ConfigRoot: root, Values: twoInstances(), Globs: sonarrGlobs}
	err := s.Restore(context.Background(), Step{From: "0.4.0", To: "0.5.0"}, []string{"sonarr"})
	if err == nil || !strings.Contains(err.Error(), "0.4.0 -> 0.5.0") {
		t.Fatalf("error = %v, want a refusal naming the step", err)
	}
}
