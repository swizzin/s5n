package bazarr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// planYAML is what a discovery run writes for a source whose libraries live
// under /srv/media: every path in it is invented.
const planYAML = `version: 2
apps:
  bazarr:
    kind: kubernetes
    source: bazarr
    config: /srv/apps/bazarr
    namespace: media
    ports: [6767]
    paths:
      - from: /media
        to: /data/media
      - from: /data/media
        to: /data/media
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
    paths:
      - from: /library
        to: /data/media
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    namespace: media
    ports: [7878]
    paths:
      - from: /library
        to: /data/media
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
hosts:
  - host: 192.0.2.10
    leave: true
  - service: sonarr.media
    app: sonarr
  - service: radarr.media
    app: radarr
`

// settingsFileFixture is the shape the pinned image writes, with the two Arrs
// pointing at the source's Services and a path mapping whose two sides are the
// two apps' own views of one library. Everything this package does not own is
// kept in the fixture verbatim, so a test can hold the pass to not touching it.
const settingsFileFixture = `auth:
  apikey: 0123456789abcdef0123456789abcdef
  password: ''
  type: null
  username: ''
general:
  base_url: /bazarr
  branch: master
  instance_name: Bazarr
  path_mappings:
  - - /library
    - /media
  path_mappings_movie: []
  port: 6767
  use_radarr: true
  use_sonarr: true
radarr:
  apikey: radarr-key
  base_url: /radarr
  ip: radarr.media
  port: 7878
  ssl: false
sonarr:
  apikey: sonarr-key
  base_url: /
  ip: sonarr.media
  port: 8989
  ssl: false
`

// dbFixture is the two library tables with the paths the source stored, and
// one subtitle row with no path yet (a subtitle the app has not placed).
const dbFixture = `
CREATE TABLE table_shows (
    sonarrSeriesId INTEGER PRIMARY KEY,
    title TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE
);
CREATE TABLE table_episodes (
    sonarrEpisodeId INTEGER PRIMARY KEY,
    sonarrSeriesId INTEGER,
    title TEXT NOT NULL,
    path TEXT NOT NULL
);
CREATE TABLE table_episodes_subtitles (
    id INTEGER PRIMARY KEY,
    sonarrEpisodeId INTEGER,
    language TEXT NOT NULL,
    path TEXT
);
CREATE TABLE table_movies (
    radarrId INTEGER PRIMARY KEY,
    title TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE
);
CREATE TABLE table_movies_subtitles (
    id INTEGER PRIMARY KEY,
    radarrId INTEGER,
    language TEXT NOT NULL,
    path TEXT
);
INSERT INTO table_shows (sonarrSeriesId, title, path) VALUES (1, 'A Show', '/media/tv/A Show');
INSERT INTO table_episodes (sonarrEpisodeId, sonarrSeriesId, title, path)
    VALUES (11, 1, 'One', '/media/tv/A Show/Season 1/A Show - S01E01.mkv');
INSERT INTO table_episodes_subtitles (id, sonarrEpisodeId, language, path) VALUES (21, 11, 'en', NULL);
INSERT INTO table_movies (radarrId, title, path) VALUES (2, 'A Film', '/media/movies/A Film (2020)');
INSERT INTO table_movies_subtitles (id, radarrId, language, path)
    VALUES (22, 2, 'en', '/media/movies/A Film (2020)/A Film (2020).en.srt');
`

// setup stages a config directory holding both files and returns the input.
func setup(t *testing.T) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(settingsFileFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, dbFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, dbFixture); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "bazarr", Dir: dir, Plan: p, Targets: targets(), Runs: runs()}
}

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"bazarr": {Host: "bazarr", Port: 6767, URLBase: "/bazarr"},
		"sonarr": {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"radarr": {Host: "radarr", Port: 7878, URLBase: "/radarr"},
		"plex":   {Host: "plex", Port: 32400},
	}
}

func runs() map[string][]string {
	return map[string][]string{
		"bazarr": {"bazarr"}, "sonarr": {"sonarr"}, "radarr": {"radarr"}, "plex": {"plex"},
	}
}

// TestRewritesBothArrsAndTheLibrary: the two sections point at the box's own
// Services, and every library path moves with the media root, including the
// subtitle files beside it.
func TestRewritesBothArrsAndTheLibrary(t *testing.T) {
	in := setup(t)
	ctx := context.Background()
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	got := map[string]int{}
	for _, c := range rep.Changes {
		got[c.Where] += c.Count
	}
	// The port each side already names is the release's own, so it is not a
	// change; the sonarr side's empty base URL is.
	for where, n := range map[string]int{
		"sonarr.ip":                   1,
		"sonarr.base_url":             1,
		"radarr.ip":                   1,
		"general.path_mappings":       1, // the pair's two sides are now equal
		"table_shows.path":            1,
		"table_episodes.path":         1,
		"table_movies.path":           1,
		"table_movies_subtitles.path": 1,
	} {
		if got[where] != n {
			t.Errorf("%s: %d changes, want %d (all: %v)", where, got[where], n, got)
		}
	}
	if len(rep.Left) != 0 {
		t.Errorf("Left = %v, want none", rep.Left)
	}

	// The check wrote nothing: the staged files are the source's.
	before, err := os.ReadFile(filepath.Join(in.Dir, settingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != settingsFileFixture {
		t.Fatal("Check changed the settings file")
	}

	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	assertWrittenSettings(t, in.Dir)
	assertWrittenPaths(t, in.Dir)

	// A migrated copy sits where box wants it, so a second check has nothing to
	// say and its Write changes nothing.
	again, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes) != 0 || again.Blocked() {
		t.Fatalf("second check = %+v, want no changes", again)
	}
	written := mustRead(t, filepath.Join(in.Dir, settingsFile))
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, filepath.Join(in.Dir, settingsFile))) != string(written) {
		t.Fatal("a second Write changed the settings file")
	}
}

// TestASwitchedOffSideIsLeftAndSaidSo: nothing dials it, so an address the plan
// does not cover is not a gap.
func TestASwitchedOffSideIsLeftAndSaidSo(t *testing.T) {
	in := setup(t)
	doc := parseSettings(mustRead(t, filepath.Join(in.Dir, settingsFile)))
	doc.setScalar("general", "use_radarr", "false", false)
	if err := os.WriteFile(filepath.Join(in.Dir, settingsFile), doc.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// The radarr address now points somewhere no rule covers, which only a side
	// that is on would make a gap.
	doc.setScalar("radarr", "ip", "203.0.113.9", true)
	if err := os.WriteFile(filepath.Join(in.Dir, settingsFile), doc.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a switched-off side blocked the run: %v %v", rep.Uncovered, rep.Unclassified)
	}
	if len(rep.Left) != 1 || rep.Left[0].Where != "radarr.ip" || !strings.Contains(rep.Left[0].Why, "switched off") {
		t.Fatalf("Left = %+v, want the radarr side left and explained", rep.Left)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if v, _ := parseSettings(mustRead(t, filepath.Join(in.Dir, settingsFile))).scalar("radarr", "ip"); v != "203.0.113.9" {
		t.Fatalf("radarr.ip = %q, want it left alone", v)
	}
}

// TestAnAddressNoRuleCoversBlocks: a stored address the plan does not settle is
// the operator's to decide, and the run stops rather than guess.
func TestAnAddressNoRuleCoversBlocks(t *testing.T) {
	in := setup(t)
	doc := parseSettings(mustRead(t, filepath.Join(in.Dir, settingsFile)))
	doc.setScalar("sonarr", "ip", "203.0.113.9", true)
	if err := os.WriteFile(filepath.Join(in.Dir, settingsFile), doc.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Blocked() {
		t.Fatal("want the run blocked by an unsettled address")
	}
	if len(rep.Unclassified) != 1 || rep.Unclassified[0].Host != "203.0.113.9" {
		t.Fatalf("Unclassified = %+v", rep.Unclassified)
	}
}

// TestAMappingWithTwoArrsIsLeft: a mapping says "the path this Arr reports",
// and with two Sonarrs the pair cannot say which one's. Leaving it (and saying
// so) is the decision; rewriting it at a guess would point one library's files
// at the other's.
func TestAMappingWithTwoArrsIsLeft(t *testing.T) {
	in := setup(t)
	in.Runs["sonarr"] = []string{"sonarr", "sonarr-4k"}
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a mapping box cannot settle blocked the run: %v %v", rep.Uncovered, rep.Unclassified)
	}
	if len(rep.Left) != 1 || rep.Left[0].Where != "general.path_mappings" {
		t.Fatalf("Left = %+v, want the mapping left and explained", rep.Left)
	}
	if got, _, err := parseSettings(mustRead(t, filepath.Join(in.Dir, settingsFile))).pairs("general", "path_mappings"); err != nil || len(got) != 1 {
		t.Fatalf("path_mappings = %v, %v, want the source's one pair", got, err)
	}
}

// TestAStoredPathNoRuleCoversBlocks: a library row outside every rule is a gap,
// not a value to leave.
func TestAStoredPathNoRuleCoversBlocks(t *testing.T) {
	in := setup(t)
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(in.Dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE table_shows SET path = '/elsewhere/tv/A Show' WHERE sonarrSeriesId = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Blocked() || len(rep.Uncovered) != 1 {
		t.Fatalf("Uncovered = %+v, want the one stored path", rep.Uncovered)
	}
	if rep.Uncovered[0].Key != "table_shows.path" {
		t.Fatalf("gap names %q", rep.Uncovered[0].Key)
	}
}

// TestTheFileIsOnlyTouchedWhereItMust: the pass replaces the owned values and
// keeps every other byte, including the app's own empty scalars and nulls. The
// expected file is the fixture with exactly the owned lines rewritten, which is
// what makes this a byte-level proof and not a value-level one.
func TestTheFileIsOnlyTouchedWhereItMust(t *testing.T) {
	in := setup(t)
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		// The mapping's two sides now name one path, so the pair goes.
		"  path_mappings:\n  - - /library\n    - /media\n", "  path_mappings: []\n",
		"  ip: sonarr.media", `  ip: "sonarr"`,
		"  ip: radarr.media", `  ip: "radarr"`,
		"  base_url: /\ns", "  base_url: \"/sonarr\"\ns", // the sonarr section's, below
	).Replace(settingsFileFixture)
	// The sonarr base_url is the section's own line; the replacer above cannot
	// tell it from another "/", so it is set here.
	want = strings.Replace(want, "  apikey: sonarr-key\n  base_url: /\n", "  apikey: sonarr-key\n  base_url: \"/sonarr\"\n", 1)
	got := string(mustRead(t, filepath.Join(in.Dir, settingsFile)))
	if got != want {
		t.Fatalf("the settings file is not the fixture with only the owned lines rewritten:\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

// assertWrittenSettings is the settings half of the rewrite: both Arrs at the
// release's address, the mapping gone, and everything else the file said.
func assertWrittenSettings(t *testing.T, dir string) {
	t.Helper()
	doc := parseSettings(mustRead(t, filepath.Join(dir, settingsFile)))
	for section, want := range map[string]map[string]string{
		"sonarr": {"ip": "sonarr", "port": "8989", "base_url": "/sonarr", "ssl": "false", "apikey": "sonarr-key"},
		"radarr": {"ip": "radarr", "port": "7878", "base_url": "/radarr", "ssl": "false", "apikey": "radarr-key"},
	} {
		for key, value := range want {
			if got, ok := doc.scalar(section, key); !ok || got != value {
				t.Errorf("%s.%s = %q (present %v), want %q", section, key, got, ok, value)
			}
		}
	}
	if pairs, present, err := doc.pairs("general", "path_mappings"); err != nil || !present || len(pairs) != 0 {
		t.Errorf("general.path_mappings = %v, %v, %v; want it dropped", pairs, present, err)
	}
	if got, _ := doc.scalar("general", "base_url"); got != "/bazarr" {
		t.Errorf("general.base_url = %q, want it untouched", got)
	}
	if got, _ := doc.scalar("auth", "apikey"); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("auth.apikey = %q, want it untouched", got)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(dir, settingsFile))), "instance_name: Bazarr") {
		t.Error("general.instance_name was lost")
	}
}

// assertWrittenPaths is the library half: every stored path moved with the
// media root, the subtitle beside its film included.
func assertWrittenPaths(t *testing.T, dir string) {
	t.Helper()
	paths := readPaths(t, dir)
	for table, want := range map[string]string{
		"table_shows":            "/data/media/tv/A Show",
		"table_episodes":         "/data/media/tv/A Show/Season 1/A Show - S01E01.mkv",
		"table_movies":           "/data/media/movies/A Film (2020)",
		"table_movies_subtitles": "/data/media/movies/A Film (2020)/A Film (2020).en.srt",
	} {
		if paths[table] != want {
			t.Errorf("%s.path = %q, want %q", table, paths[table], want)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readPaths reads the first path out of each table by its own key.
func readPaths(t *testing.T, dir string) map[string]string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]string{}
	for _, c := range pathColumns {
		if c.table == "table_episodes_subtitles" || c.table == "table_shows_rootfolder" || c.table == "table_movies_rootfolder" {
			continue
		}
		var path string
		if err := db.QueryRowContext(ctx, "SELECT "+c.column+" FROM "+c.table+" ORDER BY "+c.key+" LIMIT 1").Scan(&path); err != nil {
			t.Fatalf("%s: %v", c.table, err)
		}
		out[c.table] = path
	}
	return out
}
