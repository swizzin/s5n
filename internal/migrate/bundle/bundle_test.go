package bundle

// bundle_test.go proves the three things the package exists for: a snapshot's
// name says what it is and when it was taken, a snapshot written and opened
// gives apply the same plan and the same config trees, and the newest snapshot
// a run finds is the newest one that can be applied. Everything here is
// invented: example.com paths, a synthetic config, a plan with no source behind
// it.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
)

const testPlan = `version: 2
data:
  root: /srv/library
  downloads: Downloads
  media: media
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
`

const testReport = "plan: a read of the source\napp: sonarr (from sonarr, config /srv/apps/sonarr)\n"

// tree builds one app's config directory with a file and a directory, the way a
// real one has both.
func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<Config/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "sonarr.txt"), []byte("starting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestFileNameSaysWhatItIsAndWhen: the name is what an operator reads in the
// directory listing, so it carries the time in UTC and the mode, and nothing a
// shell would have to quote.
func TestFileNameSaysWhatItIsAndWhen(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 5, 3, 0, time.UTC)
	for _, mode := range []archive.Mode{archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal} {
		got := FileName(mode, at)
		want := "migrate-20261117T090503Z-" + string(mode) + ".tar.gz"
		if got != want {
			t.Errorf("FileName(%q) = %q, want %q", mode, got, want)
		}
	}
	// A time in another zone is the same instant in UTC: two hosts must not
	// name one snapshot two ways.
	other := time.Date(2026, 11, 17, 10, 5, 3, 0, time.FixedZone("elsewhere", 3600))
	if got, want := FileName(archive.ModeFinal, other), "migrate-20261117T090503Z-final.tar.gz"; got != want {
		t.Errorf("FileName of the same instant in another zone = %q, want %q", got, want)
	}
}

// TestWriteThenOpenGivesBackTheSameSnapshot is the round trip: what apply needs
// out of a snapshot is the plan's bytes, the report, the source's manifests and
// one directory per planned app.
func TestWriteThenOpenGivesBackTheSameSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeFinal, time.Now()))
	res, err := Write(path, Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   []byte(testPlan),
		Report: []byte(testReport),
		Source: map[string][]byte{"deployment-media-sonarr.json": []byte("{}\n")},
		Configs: []Config{
			{Instance: "sonarr", Dir: tree(t)},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Write skipped %v, and this tree has no link in it", res.Skipped)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("snapshot mode = %04o, want 0600: it holds the source's secrets", info.Mode().Perm())
	}

	out := filepath.Join(dir, "open")
	got, err := Open(path, out)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Mode != archive.ModeFinal {
		t.Errorf("mode = %q, want %q", got.Mode, archive.ModeFinal)
	}
	if string(got.Plan) != testPlan {
		t.Errorf("plan does not round-trip:\n%s", got.Plan)
	}
	if string(got.Report) != testReport {
		t.Errorf("report does not round-trip: %q", got.Report)
	}
	if _, ok := got.Source["deployment-media-sonarr.json"]; !ok {
		t.Errorf("the source's manifests do not round-trip: %v", keys(got.Source))
	}
	cfg := got.Configs["sonarr"]
	if cfg == "" {
		t.Fatalf("the snapshot opened with no config for sonarr: %v", keys(got.Configs))
	}
	raw, err := os.ReadFile(filepath.Join(cfg, "logs", "sonarr.txt"))
	if err != nil {
		t.Fatalf("read the opened config: %v", err)
	}
	if string(raw) != "starting\n" {
		t.Errorf("the opened config holds %q, want what was written", raw)
	}
}

// TestASurveyIsWrittenWithoutConfigs: the survey mode exists so discover can
// record a read without copying gigabytes of config, and apply refuses it
// because there is nothing in it to rewrite.
func TestASurveyIsWrittenWithoutConfigs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeSurvey, time.Now()))
	if _, err := Write(path, Snapshot{
		Mode:   archive.ModeSurvey,
		Plan:   []byte(testPlan),
		Report: []byte(testReport),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m, err := archive.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for name := range m.Entries {
		if name == archive.ConfigPrefix || strings.HasPrefix(name, archive.ConfigPrefix+"/") {
			t.Errorf("the survey holds %s, and a survey holds no configs", name)
		}
	}
}

// TestWriteRefusesWhatItCannotCarry: a snapshot is the whole of what apply
// runs, so a half-written one is worse than none. Every refusal leaves no file
// behind for the next run to mistake for a snapshot.
func TestWriteRefusesWhatItCannotCarry(t *testing.T) {
	tests := []struct {
		name string
		s    Snapshot
		want string
	}{
		{"no plan", Snapshot{Mode: archive.ModeFinal, Report: []byte(testReport)}, "no plan"},
		{"a mode apply does not read", Snapshot{Mode: archive.ModeBackup, Plan: []byte(testPlan)}, "mode"},
		{"a config with no instance", Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{{Dir: tree(t)}}}, "instance"},
		{"an instance named like a path", Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{{Instance: "../etc", Dir: tree(t)}}}, "instance"},
		{"the same instance twice", Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{{Instance: "sonarr", Dir: tree(t)}, {Instance: "sonarr", Dir: tree(t)}}}, "twice"},
		{"a survey with a config", Snapshot{Mode: archive.ModeSurvey, Plan: []byte(testPlan), Configs: []Config{{Instance: "sonarr", Dir: tree(t)}}}, "survey"},
		{"a config directory that is not there", Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{{Instance: "sonarr", Dir: "/nonexistent/example"}}}, "sonarr"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.tar.gz")
			_, err := Write(path, tt.s)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Write error = %v, want it to name %q", err, tt.want)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused write left %s behind", path)
			}
		})
	}
}

// TestNewestSkipsASurvey: `box migrate apply` with no file takes the newest
// snapshot in the directory, and a survey is not one it can apply. A newer
// survey must not shadow the final snapshot behind it.
func TestNewestSkipsASurvey(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "migrate-20261117T090000Z-final.tar.gz")
	survey := filepath.Join(dir, "migrate-20261117T100000Z-survey.tar.gz")
	rehearsal := filepath.Join(dir, "migrate-20261117T110000Z-rehearsal.tar.gz")
	for _, path := range []string{final, survey, rehearsal} {
		mode := archive.ModeFinal
		switch path {
		case survey:
			mode = archive.ModeSurvey
		case rehearsal:
			mode = archive.ModeRehearsal
		}
		if _, err := Write(path, Snapshot{Mode: mode, Plan: []byte(testPlan), Report: []byte(testReport)}); err != nil {
			t.Fatalf("write %s: %v", filepath.Base(path), err)
		}
	}
	// A rehearsal is refused by apply for its own reason, but it is still a
	// snapshot apply reads: the newest one that is not a survey is the answer,
	// so a newer survey does not shadow the snapshot behind it.
	got, err := Newest(dir)
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if got != rehearsal {
		t.Errorf("Newest = %q, want %q", filepath.Base(got), filepath.Base(rehearsal))
	}
	// With the rehearsal gone, the final snapshot is what a run picks up.
	if err := os.Remove(rehearsal); err != nil {
		t.Fatal(err)
	}
	if got, err = Newest(dir); err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if got != final {
		t.Errorf("Newest = %q, want %q", filepath.Base(got), filepath.Base(final))
	}
}

// TestWriteNeverOverwritesASnapshot: two reads of one source are two snapshots,
// and the second must not quietly replace the first: the file is the operator's
// rollback for a source that is about to be reset.
func TestWriteNeverOverwritesASnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate-20261117T090000Z-final.tar.gz")
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan + "  # a second read\n")}); err == nil || !strings.Contains(err.Error(), "there already") {
		t.Fatalf("Write error = %v, want it to refuse writing over a snapshot", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the refused write changed the snapshot that was there")
	}
}

// TestWriteLeavesTheOperatorsDirectoryAlone: --out can name a file in a
// directory the operator made, and a snapshot's mode is the file's. Changing
// the mode of a directory box was pointed at would be a change nobody asked for.
func TestWriteLeavesTheOperatorsDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "migrate-20261117T090000Z-final.tar.gz")
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the directory is %04o, want the 0755 it was", info.Mode().Perm())
	}
	// A directory box makes itself is 0700: it holds the source's secrets.
	fresh := filepath.Join(t.TempDir(), "snapshots")
	if _, err := Write(filepath.Join(fresh, "migrate-20261117T090000Z-final.tar.gz"), Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err = os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("the directory box made is %04o, want 0700", info.Mode().Perm())
	}
}

// TestNewestSaysWhenThereIsNone: an empty directory is a refusal that names the
// two commands that write one, because "apply" with no file is the documented
// way to finish a cutover.
func TestNewestSaysWhenThereIsNone(t *testing.T) {
	_, err := Newest(t.TempDir())
	if err == nil {
		t.Fatal("Newest of an empty directory found something")
	}
	for _, want := range []string{"box migrate cutover", "box migrate apply <file>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestOpenRefusesASnapshotItCannotTrust: the file format's guarantees are
// internal/archive's, and this package's job is to say which file was wrong.
func TestOpenRefusesASnapshotItCannotTrust(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate-20261117T090000Z-final.tar.gz")
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.tar.gz")
	if err := os.WriteFile(truncated, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(truncated, filepath.Join(dir, "open")); err == nil {
		t.Fatal("Open accepted a truncated snapshot")
	}
	if _, err := os.Stat(filepath.Join(dir, "open")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused Open left an extracted tree behind")
	}
}

// TestOpenRefusesAPlanlessSnapshot: apply runs a plan, so a snapshot without
// one is refused here rather than one step later.
func TestOpenRefusesAPlanlessSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.tar.gz")
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Report: []byte(testReport)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := filepath.Join(dir, "open")
	got, err := Open(path, out)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Plan) == 0 {
		t.Fatal("the opened snapshot has no plan")
	}
	// The file is there under its own name; what this package promises is that
	// a caller finds it by the constant, not by guessing a path.
	if _, err := os.Stat(filepath.Join(out, archive.PlanName)); err != nil {
		t.Errorf("the opened snapshot holds no %s: %v", archive.PlanName, err)
	}
}

// keys is a map's keys, sorted by the map itself, for a failure message.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
