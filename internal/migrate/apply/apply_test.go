package apply

// apply_test.go drives Run end to end over a synthetic Sonarr and qBittorrent:
// the same code path a real migration takes, with the three things outside the
// plan (the ownership record, the values editor and the question) faked, and
// with the real rewriters doing the rewriting.
//
// What it is for: the order is invisible from any one step. A test that only
// checked the staged copy would pass while the run moved a config before its
// data was renamed, or enabled an instance over an existing one. So the
// assertions here are about the *end state* of all three trees -- the seedbox's
// config root, its data root, and the source, which must be untouched.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/wiring"
)

// TestRunAdoptsASonarrAndQbittorrent is the whole thing: stage, check, write,
// rename, move, adopt, enable, and every tree afterwards.
func TestRunAdoptsASonarrAndQbittorrent(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	source := hashTree(t, f.sourceRoot)

	values := &fakeValues{}
	owned := &fakeOwned{}
	var shown Summary
	res, err := Run(context.Background(), p, raw, f.env(values, owned, yes(t, &shown)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The source is the rollback: not one byte of it moved.
	sameTree(t, "the source tree", source, hashTree(t, f.sourceRoot))
	assertSonarrConfig(t, f)
	assertQbitConfig(t, f)
	assertDataRenamed(t, f)
	assertRecord(t, f, res, raw)
	assertOwned(t, owned)
	assertKeys(t, f, res)
	if got := strings.Join(values.enabled(), ","); got != "qbittorrent,sonarr" {
		t.Errorf("enabled %q, want qbittorrent,sonarr", got)
	}
	assertSummary(t, shown)
	assertStagingClean(t, f, res)
	if res.Done {
		t.Error("the first run reported itself done, which is the re-run's answer")
	}
}

// assertSonarrConfig is the Arr's half of the end state: its database rewritten
// in place, its own settings from the chart, and its key untouched.
func assertSonarrConfig(t *testing.T, f *fixture) {
	t.Helper()
	db := filepath.Join(f.configRoot, "sonarr", "sonarr.db")
	if got := query(t, db, `SELECT Path FROM Series WHERE Id = 1`); got != "/data/media/tv/Example Show" {
		t.Errorf("Series.Path = %q, want /data/media/tv/Example Show", got)
	}
	if got := query(t, db, `SELECT Path FROM RootFolders WHERE Id = 1`); got != "/data/media/tv" {
		t.Errorf("RootFolders.Path = %q, want /data/media/tv", got)
	}
	if got := query(t, db, `SELECT Data FROM History WHERE Id = 1`); !strings.Contains(got, "/data/downloads/Example.Show.S01E01.nzb") {
		t.Errorf("History.Data = %q, want the dropped path rewritten into /data/downloads", got)
	}
	if got := query(t, db, `SELECT Settings FROM DownloadClients WHERE Id = 1`); !strings.Contains(got, `"host":"qbittorrent"`) {
		t.Errorf("DownloadClients.Settings = %q, want the host pointed at the release's qbittorrent", got)
	}
	if got := query(t, db, `SELECT count(*) FROM Commands`); got != "0" {
		t.Errorf("Commands holds %s rows, want none: a command that never finished names a path the migration moved", got)
	}
	xml := f.readFile("config/sonarr/config.xml")
	for _, want := range []string{
		"<Port>8989</Port>", "<UrlBase>/sonarr</UrlBase>", "<BindAddress>*</BindAddress>",
		"<ApiKey>" + testAPIKey + "</ApiKey>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("config.xml does not hold %s:\n%s", want, xml)
		}
	}
	// The log the copy carried is not app knowledge and must arrive intact.
	if got := f.readFile("config/sonarr/logs/sonarr.txt"); !strings.Contains(got, "Starting") {
		t.Errorf("the copied log is %q", got)
	}
}

// assertQbitConfig is the client's half: the contract's save path in the file
// and in the resume store beside it.
func assertQbitConfig(t *testing.T, f *fixture) {
	t.Helper()
	conf := f.readFile("config/qbittorrent/qBittorrent/qBittorrent.conf")
	if !strings.Contains(conf, `Session\DefaultSavePath=/data/downloads`) {
		t.Errorf("qBittorrent.conf does not save into /data/downloads:\n%s", conf)
	}
	db := filepath.Join(f.configRoot, "qbittorrent", "qBittorrent", "torrents.db")
	if got := query(t, db, `SELECT target_save_path FROM torrents WHERE id = 1`); got != "/data/downloads/tv" {
		t.Errorf("torrents.target_save_path = %q, want /data/downloads/tv", got)
	}
}

// assertDataRenamed: the data is the same directory under the contract's name.
func assertDataRenamed(t *testing.T, f *fixture) {
	t.Helper()
	if !isDir(filepath.Join(f.dataRoot, "downloads")) {
		t.Errorf("%s/downloads does not exist", f.dataRoot)
	}
	if exists(filepath.Join(f.dataRoot, "Downloads")) {
		t.Errorf("%s/Downloads still exists", f.dataRoot)
	}
	if !isDir(filepath.Join(f.dataRoot, "media")) {
		t.Errorf("%s/media does not exist; the media directory already had the contract's name and must not move", f.dataRoot)
	}
}

// assertRecord is state.json: every step this run finished.
func assertRecord(t *testing.T, f *fixture, res Result, raw []byte) {
	t.Helper()
	st := f.state(res.PlanID)
	if st.Plan != PlanDigest(raw) {
		t.Errorf("state.Plan = %q, want the plan's digest", st.Plan)
	}
	if len(st.Renamed) != 1 || st.Renamed[0] != "Downloads" {
		t.Errorf("state.Renamed = %v, want [Downloads]", st.Renamed)
	}
	if strings.Join(st.Moved, ",") != "qbittorrent,sonarr" {
		t.Errorf("state.Moved = %v", st.Moved)
	}
	if !st.Secret || !st.Owned || !st.Values {
		t.Errorf("state says secret=%v owned=%v values=%v, want all three done", st.Secret, st.Owned, st.Values)
	}
}

// assertOwned: the connection the rewriter adopted is in wiring's record, keyed
// the way wiring looks it up (the Arr that stores the client).
func assertOwned(t *testing.T, owned *fakeOwned) {
	t.Helper()
	rec, err := wiring.ParseOwnedRecord(owned.data)
	if err != nil {
		t.Fatalf("parse owned.json: %v", err)
	}
	if got := rec.ArrDownloadClientID("sonarr"); got != 1 {
		t.Errorf("owned.json records download client %d for sonarr, want 1", got)
	}
}

// assertKeys: each app keeps the key it has today, in the release Secret and in
// its own file.
func assertKeys(t *testing.T, f *fixture, res Result) {
	t.Helper()
	if got := f.keys.values["sonarr-apikey"]; got != testAPIKey {
		t.Errorf("the release Secret holds %q for sonarr, want the source's key", got)
	}
	if got := strings.Join(f.keys.sets, ","); got != "sonarr-apikey" {
		t.Errorf("the run wrote %q into the release Secret, want only sonarr-apikey: qBittorrent has no key of its own to keep", got)
	}
	if got := strings.Join(res.Keys, ","); got != "sonarr-apikey" {
		t.Errorf("the result reports %q, want sonarr-apikey", got)
	}
}

// assertSummary: the review the operator was shown names the changes, not just
// a count.
func assertSummary(t *testing.T, shown Summary) {
	t.Helper()
	if len(shown.Instances) != 2 {
		t.Fatalf("summary holds %d instances, want 2", len(shown.Instances))
	}
	lines := strings.Join(shown.Lines(), "\n")
	for _, want := range []string{
		"sonarr.db Series.Path path 1",
		"config.xml UrlBase url base 1",
		"data: rename Downloads to downloads",
		"enable: sonarr",
		"sonarr: keep its API key (sonarr-apikey)",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the summary does not say %q:\n%s", want, lines)
		}
	}
}

// assertStagingClean: a finished run leaves the record and nothing else.
func assertStagingClean(t *testing.T, f *fixture, res Result) {
	t.Helper()
	entries, err := os.ReadDir(stateDir(f.configRoot, res.PlanID))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("%s is still in the staging directory after a finished run", e.Name())
		}
	}
}

// TestRunAgainIsANoOp is the second run of a plan that finished: nothing is
// renamed, moved, adopted or enabled twice, and the file mode says so.
func TestRunAgainIsANoOp(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	values := &fakeValues{}
	owned := &fakeOwned{}
	var shown Summary
	env := f.env(values, owned, yes(t, &shown))
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := hashTree(t, filepath.Join(f.root, "config"))

	// The second run has no question to ask: it is Done before it stages
	// anything, and the caller goes straight to its apply.
	asked := false
	env.Confirm = func(Summary) (bool, error) {
		asked = true
		return true, nil
	}
	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !res.Done {
		t.Error("the second run of a finished plan is not Done")
	}
	if asked {
		t.Error("the second run asked a question")
	}
	if len(values.enabled()) != 2 {
		t.Errorf("the second run enabled %v again", values.enabled())
	}
	sameTree(t, "the config root", before, hashTree(t, filepath.Join(f.root, "config")))
}

// TestRunFinishesAHalfDonePlan interrupts a run in its last step (the values
// edit) and proves the second run picks up where the first stopped: the
// renames and the moves are recorded, so they are not done twice.
func TestRunFinishesAHalfDonePlan(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	values := &fakeValues{fail: os.ErrPermission}
	owned := &fakeOwned{}
	var shown Summary
	env := f.env(values, owned, yes(t, &shown))

	if _, err := Run(context.Background(), p, raw, env); err == nil {
		t.Fatal("the run finished although the values editor refused")
	}
	st := f.state(PlanID(raw))
	if len(st.Moved) != 2 || len(st.Renamed) != 1 || st.Values || !st.Owned {
		t.Fatalf("state after the interrupted run = %+v, want renames and moves recorded and values not", st)
	}
	if !isDir(filepath.Join(f.configRoot, "sonarr")) {
		t.Fatal("the interrupted run did not move the instances into place")
	}

	values.fail = nil
	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Renamed) != 0 || len(res.Moved) != 0 {
		t.Errorf("the second run repeated work: renamed %v, moved %v", res.Renamed, res.Moved)
	}
	if got := strings.Join(res.Enabled, ","); got != "qbittorrent,sonarr" {
		t.Errorf("second run enabled %q, want both instances", got)
	}
	st = f.state(PlanID(raw))
	if !st.Values {
		t.Error("the second run did not record the values edit")
	}
	if owned.writes != 1 {
		t.Errorf("owned.json was written %d times, want once: the second run must not re-seed what is recorded", owned.writes)
	}
}

// TestRunWritesNothingOutsideStagingWhenCheckBlocks pins the promise the whole
// order exists for: an uncovered path stops the run before its first write.
func TestRunWritesNothingOutsideStagingWhenCheckBlocks(t *testing.T) {
	f := newFixture(t)
	// A series outside every rule the plan has.
	exec(t, filepath.Join(f.sourceRoot, "srv", "apps", "sonarr", "sonarr.db"),
		`UPDATE Series SET Path = '/mnt/elsewhere/Example Show' WHERE Id = 1`)
	p, raw := f.plan()
	values := &fakeValues{}
	var shown Summary
	if _, err := Run(context.Background(), p, raw, f.env(values, &fakeOwned{}, yes(t, &shown))); err == nil {
		t.Fatal("the run finished with an uncovered path")
	} else {
		for _, want := range []string{"/mnt/elsewhere/Example Show", "apps.sonarr.paths"} {
			if !contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q:\n%s", want, err)
			}
		}
	}
	if len(shown.Instances) != 0 {
		t.Error("the run showed a summary although a check blocked")
	}
	for _, dir := range []string{"sonarr", "qbittorrent"} {
		if exists(filepath.Join(f.configRoot, dir)) {
			t.Errorf("%s was moved into the config root although a check blocked", dir)
		}
	}
	if exists(filepath.Join(f.dataRoot, "downloads")) {
		t.Error("the data was renamed although a check blocked")
	}
	if len(values.enabled()) != 0 {
		t.Error("an instance was enabled although a check blocked")
	}
	// The staged copy is what a person looks at next, so it stays.
	if !isDir(filepath.Join(stateDir(f.configRoot, PlanID(raw)), "sonarr")) {
		t.Error("the staging directory does not hold the copy the check refused")
	}
}

// TestRunRefusesWhenTheOperatorSaysNo: a no leaves every tree as it was, with
// the staged copy kept for the next look.
func TestRunRefusesWhenTheOperatorSaysNo(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	source := hashTree(t, f.sourceRoot)
	values := &fakeValues{}
	env := f.env(values, &fakeOwned{}, func(Summary) (bool, error) { return false, nil })

	if _, err := Run(context.Background(), p, raw, env); err == nil {
		t.Fatal("the run went ahead after a no")
	} else if !contains(err.Error(), "cancelled") {
		t.Errorf("the refusal is %q, want one saying cancelled", err)
	}
	sameTree(t, "the source tree", source, hashTree(t, f.sourceRoot))
	if exists(filepath.Join(f.configRoot, "sonarr")) {
		t.Error("the config was moved although the answer was no")
	}
	if exists(filepath.Join(f.dataRoot, "downloads")) {
		t.Error("the data was renamed although the answer was no")
	}
	if len(values.enabled()) != 0 {
		t.Error("an instance was enabled although the answer was no")
	}
}

// TestRunAdoptsAConnectionIntoAnExistingRecord: the record keeps what wiring
// already owns (another app's client) and gains this run's.
func TestRunAdoptsAConnectionIntoAnExistingRecord(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	prior := `{"version":1,"arr":{"radarr":{"downloadClient":7}}}`
	owned := &fakeOwned{data: []byte(prior)}
	var shown Summary
	if _, err := Run(context.Background(), p, raw, f.env(&fakeValues{}, owned, yes(t, &shown))); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, err := wiring.ParseOwnedRecord(owned.data)
	if err != nil {
		t.Fatalf("parse owned.json: %v", err)
	}
	if rec.ArrDownloadClientID("radarr") != 7 {
		t.Error("the record lost the client wiring already owned on radarr")
	}
	if rec.ArrDownloadClientID("sonarr") != 1 {
		t.Error("the record does not hold the client this migration adopted on sonarr")
	}
	// The record is JSON the Job reads, so it round-trips.
	var raw2 map[string]any
	if err := json.Unmarshal(owned.data, &raw2); err != nil {
		t.Fatalf("owned.json is not JSON: %v", err)
	}
}

// TestRunRefusesAnOwnedConflict: a record that already names another id for the
// same object is someone else's wiring, and applying over it would make the
// Job update a client it does not own.
func TestRunRefusesAnOwnedConflict(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	owned := &fakeOwned{data: []byte(`{"version":1,"arr":{"sonarr":{"downloadClient":9}}}`)}
	var shown Summary
	_, err := Run(context.Background(), p, raw, f.env(&fakeValues{}, owned, yes(t, &shown)))
	if err == nil {
		t.Fatal("the run replaced an id wiring already recorded")
	}
	if !contains(err.Error(), "someone wired it first") {
		t.Errorf("the refusal is %q", err)
	}
	if owned.writes != 0 {
		t.Error("the record was written over")
	}
}

// TestRunRefusesAnAppWhoseKeyCannotBeRead: the ruling is that every app keeps
// the key it has today, so an app with none cannot be migrated -- a fresh key
// would answer a key none of its clients know.
func TestRunRefusesAnAppWhoseKeyCannotBeRead(t *testing.T) {
	f := newFixture(t)
	// An Arr that has never started has no key yet.
	writeFile(t, filepath.Join(f.sourceRoot, "srv", "apps", "sonarr", "config.xml"),
		strings.Replace(sonarrConfigXML, "<ApiKey>"+testAPIKey+"</ApiKey>", "<ApiKey></ApiKey>", 1), 0o600)
	p, raw := f.plan()
	var shown Summary
	_, err := Run(context.Background(), p, raw, f.env(&fakeValues{}, &fakeOwned{}, yes(t, &shown)))
	if err == nil {
		t.Fatal("the run went ahead without a key to keep")
	}
	for _, want := range []string{"apps.sonarr", "config.xml", "holds no API key"} {
		if !contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err)
		}
	}
	if len(f.keys.sets) != 0 {
		t.Errorf("the run wrote %q into the release Secret", f.keys.sets)
	}
	if exists(filepath.Join(f.configRoot, "sonarr")) {
		t.Error("the config was moved although a key could not be kept")
	}
	if exists(filepath.Join(f.dataRoot, "downloads")) {
		t.Error("the data was renamed although a key could not be kept")
	}
	if len(shown.Instances) != 0 {
		t.Error("the run asked its question although a key could not be kept")
	}
}

// TestRunRefusesAKeyWithNowhereToGo: a run without a release Secret cannot keep
// an app's key, which the preflight says before anything is staged.
func TestRunRefusesAKeyWithNowhereToGo(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	env.Secret = nil
	err := Preflight(p, raw, env)
	if err == nil {
		t.Fatal("preflight accepted a run with no release Secret")
	}
	if !contains(err.Error(), "release Secret") {
		t.Errorf("the refusal is %q", err)
	}
}

// TestAPIKeyReaders: the two config shapes the kept keys come from.
func TestAPIKeyReaders(t *testing.T) {
	key, ok, err := xmlAPIKey([]byte(sonarrConfigXML))
	if err != nil || !ok || key != testAPIKey {
		t.Errorf("xmlAPIKey = %q, %v, %v; want the config.xml key", key, ok, err)
	}
	if key, ok, _ := xmlAPIKey([]byte("<Config><Port>8989</Port></Config>")); ok || key != "" {
		t.Errorf("a config.xml with no ApiKey read %q", key)
	}
	if _, _, err := xmlAPIKey([]byte("<Config><ApiKey>x</Config>")); err == nil {
		t.Error("an unclosed ApiKey was accepted")
	}

	tautulli := "# Tautulli\n[General]\napi_key = abc123\nhttp_port = 8181\n\n[PMS]\nPMS_IP = 192.0.2.10\n"
	if key, ok, err := iniAPIKey([]byte(tautulli)); err != nil || !ok || key != "abc123" {
		t.Errorf("iniAPIKey = %q, %v, %v; want the config.ini key", key, ok, err)
	}
	if key, ok, _ := iniAPIKey([]byte("[General]\nhttp_port = 8181\n")); ok || key != "" {
		t.Errorf("a config.ini with no api_key read %q", key)
	}
}
