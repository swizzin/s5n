package apply

// survey_test.go is the pre-install pass: the summary before there is a box, and
// the checks the install is held to afterwards.
//
// The fixture is a snapshot of Plex and one Arr, because Plex is the app whose
// check runs through its own image: one pass can check the Arr and not Plex, and
// the run after the install can check both. The Plex "database" is an empty file
// and the shell the image would run is a fake answering canned rows, so nothing
// here needs an image, a cluster or a real library.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// plexSupport is where the image keeps Plex's home under the app's config dir.
const plexSupport = "Library/Application Support/Plex Media Server"

// plexWorld is one snapshot and the seedbox's two roots.
type plexWorld struct {
	t                          *testing.T
	root, configRoot, dataRoot string
	configs                    string
	doc                        string
}

// newPlexWorld builds the source tree: Plex's trimmed config, and the Arr's
// database and settings.
func newPlexWorld(t *testing.T) *plexWorld {
	t.Helper()
	root := t.TempDir()
	w := &plexWorld{
		t:          t,
		root:       root,
		configRoot: filepath.Join(root, "config"),
		dataRoot:   filepath.Join(root, "data"),
		configs:    filepath.Join(root, "snapshot", "config"),
	}
	for _, dir := range []string{
		w.configRoot,
		filepath.Join(w.dataRoot, "Downloads"),
		filepath.Join(w.dataRoot, "media"),
		filepath.Join(w.configs, "sonarr"),
		filepath.Join(w.configs, "plex", filepath.FromSlash(plexSupport), "Plug-in Support", "Databases"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Plex's settings file names no path of its own: this fixture is about the
	// database, and a preference a plan does not cover would be a different
	// refusal.
	writeFile(t, filepath.Join(w.configs, "plex", filepath.FromSlash(plexSupport), "Preferences.xml"),
		`<?xml version="1.0" encoding="utf-8"?>`+"\n"+`<Preferences MachineIdentifier="synthetic-machine" />`+"\n", 0o600)
	writeFile(t, filepath.Join(w.configs, "plex", filepath.FromSlash(plexSupport), "Plug-in Support", "Databases", "com.plexapp.plugins.library.db"), "", 0o600)
	writeFile(t, filepath.Join(w.configs, "sonarr", "config.xml"), sonarrConfigXML, 0o600)
	newDB(t, filepath.Join(w.configs, "sonarr", "sonarr.db"),
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Title TEXT, Path TEXT)`,
		`INSERT INTO Series (Id, Title, Path) VALUES (1, 'Example Show', '/library/tv/Example Show')`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/tv')`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Status INTEGER)`,
	)
	w.doc = `version: 2
data:
  root: ` + w.dataRoot + `
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
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
    paths:
      - from: /library/tv
        to: /data/media/tv
`
	return w
}

// plan parses the fixture's plan, with the bytes it was read from.
func (w *plexWorld) plan() (*plan.Plan, []byte) {
	w.t.Helper()
	p, err := plan.Parse([]byte(w.doc))
	if err != nil {
		w.t.Fatalf("parse the fixture plan: %v", err)
	}
	return p, []byte(w.doc)
}

// env is a run's environment: the fakes for the three things outside the plan,
// and the runner when the test gives one.
func (w *plexWorld) env(v *fakeValues, o *fakeOwned, keys *fakeKeys, ask confirmFunc, run rewrite.Runner) Env {
	return Env{
		ConfigRoot: w.configRoot,
		DataRoot:   w.dataRoot,
		Configs:    w.configs,
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets: map[string]rewrite.Endpoint{
			"plex":   {Host: "plex", Port: 32400},
			"sonarr": {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		},
		Enabled: func(string) bool { return false },
		Owned:   o,
		Secret:  keys,
		Values:  v,
		Confirm: ask,
		Catalog: []string{"plex", "sonarr", "qbittorrent"},
		Run:     run,
	}
}

// surveyEnv is what the pre-install pass gets: no cluster, and a config root the
// caller throws away.
func (w *plexWorld) surveyEnv(staging string) Env {
	return Env{
		ConfigRoot: staging,
		DataRoot:   w.dataRoot,
		Configs:    w.configs,
		Targets: map[string]rewrite.Endpoint{
			"plex":   {Host: "plex", Port: 32400},
			"sonarr": {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		},
		Enabled: func(string) bool { return false },
		Catalog: []string{"plex", "sonarr", "qbittorrent"},
	}
}

// plexShell is the pinned image's `Plex SQLite`, faked: reads answer rows, and
// every script is recorded so a test can prove a pass did or did not run one.
type plexShell struct {
	rows   string
	reads  [][]byte
	writes [][]byte
}

func (s *plexShell) run(_ context.Context, app string, _ []string, stdin []byte) ([]byte, error) {
	if app != "plex" {
		return nil, errors.New("the shell was run for " + app)
	}
	if strings.HasPrefix(string(stdin), "BEGIN;") {
		s.writes = append(s.writes, stdin)
		return nil, nil
	}
	s.reads = append(s.reads, stdin)
	return []byte(s.rows), nil
}

// coveredRows is a library whose every path the fixture's plan covers.
const coveredRows = `@tables|section_locations
@tables|media_parts
@tables|media_streams
@tables|metadata_items
@section_locations|1|/srv/apps/plex/Movies
@tables|media_parts
@media_parts|10|/srv/apps/plex/Movies/Example Film (2020)/Example Film (2020).mkv
@end|1
`

// uncoveredRows is one path no rule in the plan covers.
const uncoveredRows = `@tables|section_locations
@tables|media_parts
@tables|media_streams
@tables|metadata_items
@section_locations|1|/srv/elsewhere/Movies
@end|1
`

func TestSurveyDefersTheAppThatNeedsItsImage(t *testing.T) {
	w := newPlexWorld(t)
	p, raw := w.plan()
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	shell := &plexShell{rows: coveredRows}

	summary, err := Survey(context.Background(), p, raw, w.surveyEnv(staging))
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	if len(shell.reads) != 0 {
		t.Errorf("the pre-install pass ran the app's shell %d times, and there is no box to run it in yet", len(shell.reads))
	}
	byApp := map[string]InstanceSummary{}
	for _, in := range summary.Instances {
		byApp[in.App] = in
	}
	plex, ok := byApp["plex"]
	if !ok {
		t.Fatalf("the summary has no plex row: %+v", summary.Instances)
	}
	if plex.Deferred == "" {
		t.Error("Plex's row does not say its check is deferred to the install")
	}
	if !strings.Contains(plex.Deferred, "once Plex's image runs") {
		t.Errorf("Plex's deferred row reads %q, want it to say the check happens once its image runs", plex.Deferred)
	}
	if plex.Staged.Files == 0 {
		t.Error("Plex's row counts no staged files: the copy is what the operator reviews")
	}
	sonarr := byApp["sonarr"]
	if sonarr.Deferred != "" {
		t.Errorf("the Arr's row says its check is deferred: %q", sonarr.Deferred)
	}
	if len(sonarr.Changes) == 0 {
		t.Error("the Arr's row carries no changes, so its check did not run")
	}
	// The grouped screen says the same thing in Plex's row.
	rendered := renderGrouped(summary)
	if !strings.Contains(rendered, "its databases are checked once Plex's") {
		t.Errorf("the grouped summary does not say when Plex is checked:\n%s", rendered)
	}
	// The state record of the pass is inside the directory the caller removes,
	// and nowhere near the seedbox's own config root.
	if exists(filepath.Join(w.configRoot, ".box")) {
		t.Error("the pre-install pass wrote a state directory into the seedbox's config root")
	}
	if !exists(filepath.Join(staging, ".box", "migrate")) {
		t.Error("the pre-install pass recorded nothing in the directory it was given")
	}
}

func TestSurveyHoldsTheRunToWhatWasShown(t *testing.T) {
	w := newPlexWorld(t)
	p, raw := w.plan()
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	agreed, err := Survey(context.Background(), p, raw, w.surveyEnv(staging))
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	// The run after the install, over the same plan and the same snapshot: the
	// box exists, so the Arr and Plex are both checked.
	var shown Summary
	shell := &plexShell{rows: coveredRows}
	if _, err := Run(context.Background(), p, raw, w.env(&fakeValues{}, &fakeOwned{}, newFakeKeys(), yes(t, &shown), shell.run)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if why := shown.Holds(agreed); why != "" {
		t.Errorf("the run after the install no longer holds to what was shown: %s", why)
	}
	if why := agreed.Holds(agreed); why != "" {
		t.Errorf("a summary does not hold against itself: %s", why)
	}
	// One rewrite more, and it does not hold: the operator agreed to the first.
	differed := agreed
	differed.Instances = append([]InstanceSummary(nil), agreed.Instances...)
	for i := range differed.Instances {
		if differed.Instances[i].App != "sonarr" {
			continue
		}
		differed.Instances[i].Changes = append([]rewrite.Change(nil), differed.Instances[i].Changes...)
		differed.Instances[i].Changes = append(differed.Instances[i].Changes, rewrite.Change{File: "sonarr.db", Where: "Series.Path", What: "path", Count: 1})
	}
	if why := differed.Holds(agreed); why == "" {
		t.Error("a summary with one rewrite more than the agreed one was accepted")
	} else if !strings.Contains(why, "agreed") {
		t.Errorf("the refusal does not say what was agreed to: %s", why)
	}
}

func TestThePostInstallCheckStopsBeforeAnythingIsEnabled(t *testing.T) {
	w := newPlexWorld(t)
	p, raw := w.plan()
	values := &fakeValues{}
	owned := &fakeOwned{}
	keys := newFakeKeys()
	asked := false
	shell := &plexShell{rows: uncoveredRows}

	_, err := Run(context.Background(), p, raw, w.env(values, owned, keys, func(Summary) (bool, error) {
		asked = true
		return true, nil
	}, shell.run))
	if err == nil {
		t.Fatal("a Plex path no rule covers was accepted")
	}
	if !strings.Contains(err.Error(), "/srv/elsewhere/Movies") {
		t.Errorf("the refusal does not name the path: %v", err)
	}
	if asked {
		t.Error("the run asked its question after a check had already refused")
	}
	for _, app := range []string{"plex", "sonarr"} {
		if exists(filepath.Join(w.configRoot, app)) {
			t.Errorf("%s's config was moved into the seedbox's config root", app)
		}
	}
	if got := values.enabled(); len(got) != 0 {
		t.Errorf("the run switched on %v", got)
	}
	if len(keys.sets) != 0 {
		t.Errorf("the run wrote %v into the release Secret", keys.sets)
	}
	if owned.writes != 0 {
		t.Error("the run wrote the ownership record")
	}
}

func TestACleanPostInstallCheckAsksItsOneQuestion(t *testing.T) {
	w := newPlexWorld(t)
	p, raw := w.plan()
	var shown Summary
	questions := 0
	shell := &plexShell{rows: coveredRows}

	if _, err := Run(context.Background(), p, raw, w.env(&fakeValues{}, &fakeOwned{}, newFakeKeys(), func(s Summary) (bool, error) {
		questions++
		shown = s
		return true, nil
	}, shell.run)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if questions != 1 {
		t.Fatalf("the run asked %d questions, want one", questions)
	}
	if len(shell.reads) != 2 {
		// One read is the check, the other the write pass's own read before it
		// sends the UPDATEs.
		t.Fatalf("Plex's checks ran %d read scripts, want two (the check and the write)", len(shell.reads))
	}
	for _, in := range shown.Instances {
		if in.App != "plex" {
			continue
		}
		if in.Deferred != "" {
			t.Errorf("the row the run after the install shows is still deferred: %q", in.Deferred)
		}
		if len(in.Changes) == 0 {
			t.Error("Plex's row carries no changes, so its check had not run when the question was asked")
		}
	}
}

func TestCheckedRowsAreTheOnesTheQuestionDeferred(t *testing.T) {
	agreed := Summary{Instances: []InstanceSummary{
		{App: "plex", Staged: Staged{App: "plex", Files: 7, Bytes: 2 << 20}, Deferred: "its databases are checked once Plex's image runs", Enable: true},
		{App: "sonarr", Staged: Staged{App: "sonarr", Files: 12, Bytes: 5 << 20}, Enable: true},
	}}
	now := Summary{Instances: []InstanceSummary{
		{App: "plex", Staged: Staged{App: "plex", Files: 7, Bytes: 2 << 20}, Changes: []rewrite.Change{{File: "library.db", Where: "section_locations.root_path", What: "path", Count: 8264}}, Enable: true},
		{App: "sonarr", Staged: Staged{App: "sonarr", Files: 12, Bytes: 5 << 20}, Changes: []rewrite.Change{{File: "config.xml", Where: "UrlBase", What: "url base", Count: 1}}, Enable: true},
	}}
	rows := now.CheckedRows(agreed)
	if len(rows) != 1 {
		t.Fatalf("checking the deferred rows gave %d rows, want one: %+v", len(rows), rows)
	}
	if rows[0].Name != "plex" {
		t.Errorf("the filled row is %q, want plex", rows[0].Name)
	}
	if !strings.Contains(strings.Join(rows[0].Facts, " · "), "8,264 paths") {
		t.Errorf("the filled row does not count the paths the check found: %v", rows[0].Facts)
	}
}
