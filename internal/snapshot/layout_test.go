package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirNamesTheStepAndTheApp(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	got, err := Dir(root, "0.4.0", "0.5.0", at, "sonarr")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	want := filepath.Join(root, ".box", "snapshots", "0.4.0-0.5.0-20260304T050607Z", "sonarr")
	if got != want {
		t.Fatalf("dir = %s, want %s", got, want)
	}
	// Two instances of one app are two config directories and so two
	// snapshots, which is why the last element is the instance name.
	other, err := Dir(root, "0.4.0", "0.5.0", at, "sonarr-4k")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if other == got || filepath.Dir(other) != filepath.Dir(got) {
		t.Fatalf("second instance dir = %s, want a sibling of %s", other, got)
	}
	// The name is UTC, whatever zone the run is in: the directory name is what
	// Prune orders by.
	local := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("x", 3600))
	shifted, err := Dir(root, "0.4.0", "0.5.0", local, "sonarr")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if shifted != filepath.Join(root, ".box", "snapshots", "0.4.0-0.5.0-20260304T040607Z", "sonarr") {
		t.Fatalf("dir = %s, want the time in UTC", shifted)
	}
}

// TestDirRefusesANameThatIsAPath: the versions come from chart metadata, and a
// name that walks up would put the snapshot, and the restore that reads it,
// outside the box's own tree.
func TestDirRefusesANameThatIsAPath(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		name        string
		from, to    string
		app         string
		wantInError string
	}{
		{name: "empty version", from: "", to: "0.5.0", app: "sonarr", wantInError: "is empty"},
		{name: "up a level", from: "..", to: "0.5.0", app: "sonarr", wantInError: "is not a name"},
		{name: "separator in version", from: "0.4.0/../..", to: "0.5.0", app: "sonarr", wantInError: "path separator"},
		{name: "separator in app", from: "0.4.0", to: "0.5.0", app: "sonarr/../../etc", wantInError: "path separator"},
		{name: "empty app", from: "0.4.0", to: "0.5.0", app: "", wantInError: "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Dir(t.TempDir(), tc.from, tc.to, at, tc.app)
			if err == nil {
				t.Fatal("the path was built")
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantInError)
			}
		})
	}
}

func TestCheckGlob(t *testing.T) {
	cases := []struct {
		glob    string
		wantErr string
	}{
		{glob: "app.db"},
		{glob: "app.db-wal"},
		{glob: "db/db.sqlite3"},
		{glob: "Plug-in Support/Databases/*.db"},
		{glob: "*.sqlite"},
		{glob: "", wantErr: "is empty"},
		{glob: "/etc/passwd", wantErr: "is absolute"},
		{glob: "../sonarr/sonarr.db", wantErr: `has a ".." segment`},
		{glob: "db/../app.db", wantErr: `has a ".." segment`},
		{glob: "./app.db", wantErr: "not a clean relative path"},
		{glob: "db//app.db", wantErr: "not a clean relative path"},
		{glob: "app[.db", wantErr: "path.Match"},
	}
	for _, tc := range cases {
		t.Run(tc.glob, func(t *testing.T) {
			err := CheckGlob(tc.glob)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("glob %q refused: %v", tc.glob, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("glob %q accepted", tc.glob)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestPruneKeepsTheNewest puts three steps on disk for one app, and a newer step
// that only holds another app's snapshot: the app keeps its two newest and the
// other app's step stays exactly as it was.
func TestPruneKeepsTheNewest(t *testing.T) {
	configRoot := t.TempDir()
	root := Root(configRoot)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	steps := make([]string, 3)
	for i := range steps {
		dest, err := Dir(configRoot, "0.4.0", fmt.Sprintf("0.5.%d", i), at.Add(time.Duration(i)*time.Hour), "sonarr")
		if err != nil {
			t.Fatalf("dir: %v", err)
		}
		steps[i] = dest
		write(t, filepath.Join(dest, "sonarr.db"), fmt.Sprintf("database %d", i), 0o600)
	}
	// A different app in the same newest step, and a directory that is not a
	// step of box's at all.
	otherApp := filepath.Join(filepath.Dir(steps[2]), "radarr")
	write(t, filepath.Join(otherApp, "radarr.db"), "radarr's", 0o600)
	stranger := filepath.Join(root, "not-a-step")
	write(t, filepath.Join(stranger, "keep.txt"), "not ours", 0o600)

	if err := Prune(root, "sonarr", 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The oldest is gone, and so is the step directory that held only it.
	if _, err := os.Lstat(steps[0]); !os.IsNotExist(err) {
		t.Fatalf("the oldest snapshot survived: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(steps[0])); !os.IsNotExist(err) {
		t.Fatalf("the emptied step directory survived: %v", err)
	}
	for _, step := range steps[1:] {
		if _, err := os.Lstat(step); err != nil {
			t.Fatalf("a kept snapshot is gone: %v", err)
		}
	}
	// Another app's snapshot in the same step is not this app's to prune.
	if _, err := os.Lstat(filepath.Join(filepath.Dir(steps[2]), "radarr", "radarr.db")); err != nil {
		t.Fatalf("another app's snapshot was pruned: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stranger, "keep.txt")); err != nil {
		t.Fatalf("a directory that is not a step was pruned: %v", err)
	}

	// And keeping one deletes the newer of the two, which is what makes the
	// order by time rather than by name.
	if err := Prune(root, "sonarr", 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Lstat(steps[1]); !os.IsNotExist(err) {
		t.Fatalf("the older of the two survived: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(steps[2], "sonarr.db")); err != nil {
		t.Fatalf("the newest was pruned: %v", err)
	}
}

func TestPruneRefusesToDeleteEverything(t *testing.T) {
	root := Root(t.TempDir())
	err := Prune(root, "sonarr", 0)
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("error = %v, want a refusal of keep 0", err)
	}
}

// TestPruneWithNothingThere: a box that has never updated has no snapshots
// root, and asking to prune it is not a failure.
func TestPruneWithNothingThere(t *testing.T) {
	if err := Prune(Root(t.TempDir()), "sonarr", 2); err != nil {
		t.Fatalf("prune of nothing: %v", err)
	}
}

// TestPruneLeavesAYoungerAppAlone: an app enabled after a step was taken has no
// snapshot in it, so the step is not this app's to remove.
func TestPruneLeavesAnOlderStepWithoutTheApp(t *testing.T) {
	configRoot := t.TempDir()
	root := Root(configRoot)
	at := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	old, err := Dir(configRoot, "0.4.0", "0.5.0", at, "sonarr")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	write(t, filepath.Join(old, "sonarr.db"), "database", 0o600)
	fresh, err := Dir(configRoot, "0.5.0", "0.6.0", at.Add(time.Hour), "qui")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	write(t, filepath.Join(fresh, "qui.db"), "database", 0o600)

	if err := Prune(root, "qui", 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(old, "sonarr.db")); err != nil {
		t.Fatalf("a step without this app's snapshot was removed: %v", err)
	}
}

func TestStepTimeReadsOnlyOurNames(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"0.4.0-0.5.0-20260304T050607Z", true},
		{"0.4.0-rc1-0.5.0-beta1-20260304T050607Z", true},
		{"20260304T050607Z", false},
		{"0.4.0-0.5.0", false},
		{"0.4.0-0.5.0-notatime", false},
	}
	for _, tc := range cases {
		if _, ok := stepTime(tc.name); ok != tc.ok {
			t.Fatalf("stepTime(%q) ok = %v, want %v", tc.name, ok, tc.ok)
		}
	}
}
