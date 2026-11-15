package plex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// plexImage is the pin from charts/s5n/variants.yaml, by digest: the test must
// run the `Plex SQLite` the release runs, not whatever a tag points at today.
const plexImage = "ghcr.io/home-operations/plex@sha256:06ef0fc23e9af9bbaf284c3f27972f94e703d13d6f745edeb6818fcbebf6c673"

// librarySchema is a minimal library database: the four tables this package
// reads, a directories table it must leave alone, and rows that cover every
// shape a value can have (a plain path, a file:// URL, a file://localhost URL,
// a plex:// GUID).
const librarySchema = `
CREATE TABLE section_locations (id INTEGER PRIMARY KEY, library_section_id INTEGER, root_path TEXT, available INTEGER DEFAULT 1);
CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT, size INTEGER, hash TEXT);
CREATE TABLE media_streams (id INTEGER PRIMARY KEY, media_part_id INTEGER, url TEXT, stream_type_id INTEGER);
CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, guid TEXT, title TEXT);
CREATE TABLE directories (id INTEGER PRIMARY KEY, path TEXT, parent_id INTEGER);
INSERT INTO section_locations VALUES (1, 1, '/srv/media/Movies', 1);
INSERT INTO section_locations VALUES (2, 2, '/srv/media/TV', 1);
INSERT INTO media_parts VALUES (10, 100, '/srv/media/Movies/Example Film (2020)/Example Film (2020).mkv', 1024, 'synthetic');
INSERT INTO media_streams VALUES (20, 10, 'file:///srv/media/Movies/Example Film (2020)/Example Film (2020).en.srt', 2);
INSERT INTO metadata_items VALUES (30, 'file:///srv/media/Movies/12345', 'Example Film (2020)');
INSERT INTO metadata_items VALUES (31, 'plex://movie/5d776b9c8b7c2b001f5f8b3a', 'Matched Film');
INSERT INTO directories VALUES (1, '', NULL);
INSERT INTO directories VALUES (2, 'Example Film (2020)', 1);
`

// TestPlexSQLiteDocker is the one test that runs the pinned image: it builds a
// library database with `Plex SQLite` itself, rewrites it through the real
// Runner, and checks the result with the same shell.
//
//	S5N_TEST_DOCKER=1 go test ./internal/migrate/plex/ -run TestPlexSQLiteDocker -v
//
// The command it runs, once per script, is
//
//	docker run --rm -i --user <uid>:<gid> -v <dir>:/config \
//	    --entrypoint "/usr/lib/plexmediaserver/Plex SQLite" <image> <db> [-bail]
//
// with the SQL on stdin.
func TestPlexSQLiteDocker(t *testing.T) {
	if os.Getenv("S5N_TEST_DOCKER") != "1" {
		t.Skip("set S5N_TEST_DOCKER=1 to run the pinned Plex image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	home := filepath.Join(dir, filepath.FromSlash(supportDir))
	for _, d := range []string{home, filepath.Join(home, filepath.FromSlash("Plug-in Support/Databases"))} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, prefsName), []byte(prefsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(home, filepath.FromSlash(dbRel))
	if err := os.WriteFile(dbFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := dockerRunner(dir)
	if out, err := run(ctx, "plex", []string{sqliteBin, "-bail", "/config/" + supportDir + "/" + dbRel},
		[]byte(librarySchema)); err != nil {
		t.Fatalf("build the library database: %v (output %q)", err, out)
	}

	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	in := rewrite.Input{
		App: "plex", Dir: dir, Plan: p,
		Targets: map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Run:     run,
	}

	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	want := map[string]int{
		"section_locations.root_path": 2,
		"media_parts.file":            1,
		"media_streams.url":           1,
		"metadata_items.guid":         1,
	}
	got := map[string]int{}
	for _, c := range rep.Changes {
		got[c.Where] += c.Count
	}
	for where, n := range want {
		if got[where] != n {
			t.Errorf("%s: %d changes, want %d (all: %v)", where, got[where], n, got)
		}
	}

	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}

	// The database the shell wrote reads back rewritten, and is sound.
	if got := query(t, run, "SELECT root_path FROM section_locations ORDER BY id"); got != "/data/media/Movies\n/data/media/TV" {
		t.Errorf("section_locations.root_path = %q", got)
	}
	if got := query(t, run, "SELECT file FROM media_parts WHERE id = 10"); got != "/data/media/Movies/Example Film (2020)/Example Film (2020).mkv" {
		t.Errorf("media_parts.file = %q", got)
	}
	if got := query(t, run, "SELECT url FROM media_streams WHERE id = 20"); got != "file:///data/media/Movies/Example Film (2020)/Example Film (2020).en.srt" {
		t.Errorf("media_streams.url = %q", got)
	}
	if got := query(t, run, "SELECT guid FROM metadata_items ORDER BY id"); got != "file:///data/media/Movies/12345\nplex://movie/5d776b9c8b7c2b001f5f8b3a" {
		t.Errorf("metadata_items.guid = %q", got)
	}
	if got := query(t, run, "SELECT path FROM directories ORDER BY id"); got != "\nExample Film (2020)" {
		t.Errorf("directories.path = %q, want untouched", got)
	}
	if got := query(t, run, "PRAGMA integrity_check"); got != "ok" {
		t.Errorf("integrity_check = %q", got)
	}

	// A second pass over the database the shell just wrote has nothing to do.
	rep2, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("after Write, Check still reports %v", rep2.Changes)
	}

	// Preferences.xml moved the same way as in the unit test.
	raw, err := os.ReadFile(filepath.Join(home, prefsName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `TranscoderTempDirectory="/config/transcode"`) {
		t.Errorf("Preferences.xml was not rewritten:\n%s", raw)
	}
}

// dockerRunner is the real Runner: the pinned image with the staged config at
// /config, running argv[0] as the container's command. --user keeps the files it
// writes ours, and -i feeds the SQL in on stdin.
func dockerRunner(dir string) rewrite.Runner {
	return func(ctx context.Context, app string, argv []string, stdin []byte) ([]byte, error) {
		if app != "plex" {
			return nil, fmt.Errorf("plex: runner asked for %q", app)
		}
		args := []string{
			"run", "--rm", "-i",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"-v", dir + ":/config",
			"--network", "none",
			"--entrypoint", argv[0],
			plexImage,
		}
		args = append(args, argv[1:]...)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = bytes.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%s %s: %w: %s", argv[0], strings.Join(argv[1:], " "), err, stderr.String())
		}
		if stderr.Len() > 0 && bytes.Contains(stderr.Bytes(), []byte("Error")) {
			return out, fmt.Errorf("%s: %s", strings.Join(argv, " "), stderr.String())
		}
		return out, nil
	}
}

// query runs one SELECT through the shell and trims the output.
func query(t *testing.T, run rewrite.Runner, sql string) string {
	t.Helper()
	out, err := run(context.Background(), "plex",
		[]string{sqliteBin, "-bail", "/config/" + supportDir + "/" + dbRel}, []byte(sql+";\n"))
	if err != nil {
		t.Fatalf("%s: %v (output %q)", sql, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}
