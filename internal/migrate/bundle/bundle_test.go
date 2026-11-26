package bundle

// bundle_test.go: a snapshot's name, its round trip through apply's reader, and
// which snapshot a run picks. Everything is invented.

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// tree builds a config directory with a file and a directory.
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

func TestFileNameSaysWhatItIsAndWhen(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 5, 3, 0, time.UTC)
	for _, mode := range []archive.Mode{archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal} {
		got := FileName(mode, at)
		want := "migrate-20261117T090503Z-" + string(mode) + ".tar.gz"
		if got != want {
			t.Errorf("FileName(%q) = %q, want %q", mode, got, want)
		}
	}
	// Another zone is the same instant in UTC.
	other := time.Date(2026, 11, 17, 10, 5, 3, 0, time.FixedZone("elsewhere", 3600))
	if got, want := FileName(archive.ModeFinal, other), "migrate-20261117T090503Z-final.tar.gz"; got != want {
		t.Errorf("FileName of the same instant in another zone = %q, want %q", got, want)
	}
}

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
	got, err := Open(path, out, nil, nil)
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
	// A rehearsal is refused later; a survey never shadows a snapshot.
	got, err := Newest(dir)
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if got != rehearsal {
		t.Errorf("Newest = %q, want %q", filepath.Base(got), filepath.Base(rehearsal))
	}
	// With the rehearsal gone, the final snapshot is the answer.
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
	// A directory box makes is 0700: it holds the source's secrets.
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
	if _, err := Open(truncated, filepath.Join(dir, "open"), nil, nil); err == nil {
		t.Fatal("Open accepted a truncated snapshot")
	}
	if _, err := os.Stat(filepath.Join(dir, "open")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused Open left an extracted tree behind")
	}
}

func TestOpenRefusesAPlanlessSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.tar.gz")
	if _, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Report: []byte(testReport)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := filepath.Join(dir, "open")
	got, err := Open(path, out, nil, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Plan) == 0 {
		t.Fatal("the opened snapshot has no plan")
	}
	// A caller finds it by the constant, not by a guessed path.
	if _, err := os.Stat(filepath.Join(out, archive.PlanName)); err != nil {
		t.Errorf("the opened snapshot holds no %s: %v", archive.PlanName, err)
	}
}

// keys is a map's keys, sorted, for a failure message.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// plexTree is a real Plex config: the two files a migration needs, a -wal, and
// the Metadata tree Plex rebuilds.
func plexTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	server := filepath.Join(dir, "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(filepath.Join(server, "Plug-in Support", "Databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "Preferences.xml"), []byte("<Preferences/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbs := filepath.Join(server, "Plug-in Support", "Databases")
	if err := os.WriteFile(filepath.Join(dbs, "com.plexapp.plugins.library.db"), []byte("sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbs, "com.plexapp.plugins.library.db-wal"), []byte("wal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Metadata", "Posters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Metadata", "Posters", "poster.jpg"), []byte("jpeg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Cache", "PhotoTranscoder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cache", "PhotoTranscoder", "thumb.jpg"), []byte("jpeg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// plexCarry is the preferences and databases, never the metadata tree.
func plexCarry() []File {
	const where = "Library/Application Support/Plex Media Server/"
	return []File{
		{Path: where + "Preferences.xml", Required: true},
		{Path: where + "Plug-in Support/Databases/com.plexapp.plugins.library.db", Required: true},
		{Path: where + "Plug-in Support/Databases/com.plexapp.plugins.library.db-wal"},
		{Path: where + "Plug-in Support/Databases/com.plexapp.plugins.library.db-shm"},
	}
}

func TestASnapshotCarriesOnlyTheFilesAConfigNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeFinal, time.Now()))
	res, err := Write(path, Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   []byte(testPlan),
		Report: []byte(testReport),
		Configs: []Config{
			{Instance: "plex", Dir: plexTree(t), Files: plexCarry()},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Write skipped %v, and this tree has nothing to skip", res.Skipped)
	}
	m, err := archive.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	const server = "config/plex/Library/Application Support/Plex Media Server"
	want := []string{
		"config/plex",
		"config/plex/Library",
		"config/plex/Library/Application Support",
		server,
		server + "/Plug-in Support",
		server + "/Plug-in Support/Databases",
		server + "/Plug-in Support/Databases/com.plexapp.plugins.library.db",
		server + "/Plug-in Support/Databases/com.plexapp.plugins.library.db-wal",
		server + "/Preferences.xml",
		"plan.yaml",
		"report.txt",
	}
	var got []string
	for name := range m.Entries {
		got = append(got, name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the snapshot holds\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// The state comes back; what Plex rebuilds does not.
	opened, err := Open(path, filepath.Join(dir, "open"), nil, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opened.Configs["plex"], "Library", "Application Support", "Plex Media Server", "Preferences.xml")); err != nil {
		t.Errorf("the opened snapshot has no Preferences.xml: %v", err)
	}
	for _, left := range []string{"Metadata", "Cache"} {
		if _, err := os.Stat(filepath.Join(opened.Configs["plex"], left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the opened snapshot holds %s, and Plex rebuilds it: %v", left, err)
		}
	}
}

func TestASnapshotLeavesOutWhatAFullCarryRebuilds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName(archive.ModeFinal, time.Now()))
	tree := plexTree(t)
	server := filepath.Join(tree, "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(filepath.Join(server, "Cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Write(path, Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   []byte(testPlan),
		Report: []byte(testReport),
		Configs: []Config{
			{Instance: "plex", Dir: tree, Skip: []string{
				"Library/Application Support/Plex Media Server/Cache",
			}},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Write skipped %v, and leaving a directory out is not a skip", res.Skipped)
	}
	m, err := archive.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	const prefix = "config/plex/"
	want := []string{
		prefix + "Cache/PhotoTranscoder/thumb.jpg",
		prefix + "Metadata/Posters/poster.jpg",
	}
	for _, entry := range want {
		if _, ok := m.Entries[entry]; !ok {
			t.Errorf("a full carry left out %s, which is what the whole transfer is for", entry)
		}
	}
	if _, ok := m.Entries[prefix+"Library/Application Support/Plex Media Server/Cache"]; ok {
		t.Error("a full carry kept the Cache tree it was told to leave out")
	}
	if _, ok := m.Entries[prefix+"Library/Application Support/Plex Media Server/Preferences.xml"]; !ok {
		t.Error("a full carry left out Preferences.xml")
	}
}

func TestASnapshotRefusesALeftOutPathItCannotPlace(t *testing.T) {
	tree := tree(t)
	for _, c := range []Config{
		{Instance: "app", Dir: tree, Skip: []string{"../elsewhere"}},
		{Instance: "app", Dir: tree, Skip: []string{"/absolute"}},
		{Instance: "app", Dir: tree, Skip: []string{"files", "files"}},
		{Instance: "app", Dir: tree, Files: []File{{Path: "files/a.txt"}}, Skip: []string{"files"}},
	} {
		path := filepath.Join(t.TempDir(), "a.tar.gz")
		_, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{c}})
		if err == nil {
			t.Errorf("Write accepted %+v", c)
			continue
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused write left %s behind", path)
		}
	}
}

func TestASnapshotRefusesARequiredFileItCannotCarry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.tar.gz")
	c := plexCarry()
	tree := plexTree(t)
	if err := os.Remove(filepath.Join(tree, "Library", "Application Support", "Plex Media Server", "Preferences.xml")); err != nil {
		t.Fatal(err)
	}
	_, err := Write(path, Snapshot{
		Mode:    archive.ModeFinal,
		Plan:    []byte(testPlan),
		Configs: []Config{{Instance: "plex", Dir: tree, Files: c}},
	})
	if err == nil || !strings.Contains(err.Error(), "Preferences.xml") {
		t.Fatalf("Write error = %v, want it to name the missing Preferences.xml", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused write left %s behind", path)
	}
}

func TestWriteRefusesCarriedFilesThatAreNotInsideTheConfig(t *testing.T) {
	tree := tree(t)
	tests := []struct {
		name  string
		files []File
		want  string
	}{
		{"a path out of the tree", []File{{Path: "../etc/passwd"}}, "relative path"},
		{"an absolute path", []File{{Path: "/etc/passwd"}}, "relative path"},
		{"a name that is not clean", []File{{Path: "./config.xml"}}, "relative path"},
		{"the same file twice", []File{{Path: "config.xml"}, {Path: "config.xml"}}, "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.tar.gz")
			_, err := Write(path, Snapshot{Mode: archive.ModeFinal, Plan: []byte(testPlan), Configs: []Config{{Instance: "sonarr", Dir: tree, Files: tt.files}}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Write error = %v, want it to name %q", err, tt.want)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused write left %s behind", path)
			}
		})
	}
}
