package plex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// prefsFixture is Preferences.xml as Plex writes it: one element, quoted
// attributes, a space, an escaped ampersand and a list.
const prefsFixture = `<?xml version="1.0" encoding="utf-8"?>
<Preferences MachineIdentifier="synthetic-machine" TranscoderTempDirectory="/srv/apps/plex/transcode" ButlerDatabaseBackupPath="/srv/plex/Backups" customCertificatePath="/srv/plex/a&amp;b.p12" CinemaTrailersPrerollID="/srv/media/preroll/one.mp4, /srv/media/preroll/two.mp4" iTunesLibraryXmlPath="/srv/media/iTunes Library.xml" ProcessedMachineIdentifier="synthetic-processed" />
`

// dbRows is what the library database answers for a covered install.
const dbRows = `@tables|section_locations
@tables|media_parts
@tables|media_streams
@tables|metadata_items
@section_locations|1|/srv/media/Movies
@section_locations|2|/srv/media/TV
@media_parts|10|/srv/media/Movies/Example Film (2020)/Example Film (2020).mkv
@media_streams|20|file:///srv/media/Movies/Example Film (2020)/Example Film (2020).en.srt
@media_streams|21|file://localhost/srv/media/Movies/sub.srt
@media_streams|22|metadata://artwork/1
@media_streams|23|https://example.com/sub.srt
@metadata_items|30|file:///srv/media/Movies/12345
@metadata_items|31|plex://movie/5d776b9c8b7c2b001f5f8b3a
@end|1
`

const planYAML = `
version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths:
      - from: /srv/apps/plex
        to: /config
      - from: /srv/plex
        to: /config/plex
      - from: /srv/media
        to: /data/media
`

// fakeRunner stands in for the image: it answers with dbRows and records each
// script.
type fakeRunner struct {
	rows   string
	reads  [][]byte
	writes [][]byte
	err    error
}

func (f *fakeRunner) run(_ context.Context, app string, argv []string, stdin []byte) ([]byte, error) {
	if app != "plex" {
		return nil, errors.New("runner called for " + app)
	}
	if f.err != nil {
		return nil, f.err
	}
	script := string(stdin)
	if strings.HasPrefix(script, "BEGIN;") {
		f.writes = append(f.writes, stdin)
		return nil, nil
	}
	f.reads = append(f.reads, stdin)
	return []byte(f.rows), nil
}

func setup(t *testing.T, prefs, rows string, withDB bool) (rewrite.Input, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, filepath.FromSlash(supportDir))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if prefs != "" {
		if err := os.WriteFile(filepath.Join(home, prefsName), []byte(prefs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withDB {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash("Plug-in Support/Databases")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(dbRel)), []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	f := &fakeRunner{rows: rows}
	in := rewrite.Input{
		App: "plex", Dir: dir, Plan: p,
		Targets: map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Run:     f.run,
	}
	return in, f
}

func TestRewritesPreferencesAndLibrary(t *testing.T) {
	ctx := context.Background()
	in, runner := setup(t, prefsFixture, dbRows, true)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	// One script, and no directories.path: it is relative to the section root.
	if len(runner.reads) != 1 {
		t.Fatalf("Check ran %d read scripts, want 1", len(runner.reads))
	}
	read := string(runner.reads[0])
	for _, want := range []string{
		"section_locations", "media_parts", "media_streams", "metadata_items",
	} {
		if !strings.Contains(read, "FROM "+want) {
			t.Errorf("the read script does not query %s:\n%s", want, read)
		}
	}
	if strings.Contains(read, "directories") {
		t.Errorf("the read script queries directories:\n%s", read)
	}

	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(runner.writes) != 1 {
		t.Fatalf("Write ran %d scripts, want 1", len(runner.writes))
	}
	script := string(runner.writes[0])
	for _, want := range []string{
		"BEGIN;",
		"COMMIT;",
		"UPDATE section_locations SET root_path = '/data/media/Movies' WHERE id = 1;",
		"UPDATE section_locations SET root_path = '/data/media/TV' WHERE id = 2;",
		"UPDATE media_parts SET file = '/data/media/Movies/Example Film (2020)/Example Film (2020).mkv' WHERE id = 10;",
		"UPDATE media_streams SET url = 'file:///data/media/Movies/Example Film (2020)/Example Film (2020).en.srt' WHERE id = 20;",
		"UPDATE media_streams SET url = 'file://localhost/data/media/Movies/sub.srt' WHERE id = 21;",
		"UPDATE metadata_items SET guid = 'file:///data/media/Movies/12345' WHERE id = 30;",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the write script is missing %q:\n%s", want, script)
		}
	}
	// A non-file URI and a GUID Plex matched itself are left alone.
	for _, unwanted := range []string{"id = 22;", "id = 23;", "id = 31;"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("the write script touched %s:\n%s", unwanted, script)
		}
	}

	// The path attributes moved; the rest is byte for byte.
	raw, err := os.ReadFile(filepath.Join(in.Dir, filepath.FromSlash(supportDir), prefsName))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		`TranscoderTempDirectory="/config/transcode"`,
		`ButlerDatabaseBackupPath="/config/plex/Backups"`,
		`customCertificatePath="/config/plex/a&amp;b.p12"`,
		`CinemaTrailersPrerollID="/data/media/preroll/one.mp4, /data/media/preroll/two.mp4"`,
		`iTunesLibraryXmlPath="/data/media/iTunes Library.xml"`,
		`MachineIdentifier="synthetic-machine"`,
		`ProcessedMachineIdentifier="synthetic-processed"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Preferences.xml is missing %q:\n%s", want, got)
		}
	}

	// A second Check reports the same rows, since the fake shell applies nothing;
	// the gated docker test proves a written database has nothing left to do.
}

func TestCheckWritesNothing(t *testing.T) {
	in, runner := setup(t, prefsFixture, dbRows, true)
	before := treeHash(t, in.Dir)
	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
	if len(runner.writes) != 0 {
		t.Errorf("Check ran a write script: %s", runner.writes)
	}
}

// TestRelativeMediaFileIsUncovered: a relative media_parts.file is the pathology,
// a gap rather than a guessed root.
func TestRelativeMediaFileIsUncovered(t *testing.T) {
	ctx := context.Background()
	rows := strings.Replace(dbRows,
		"@media_parts|10|/srv/media/Movies/Example Film (2020)/Example Film (2020).mkv",
		"@media_parts|10|Example Film (2020)/Example Film (2020).mkv", 1)
	in, runner := setup(t, prefsFixture, rows, true)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
	if u := rep.Uncovered[0]; u.Key != "media_parts.file" || u.Path != "Example Film (2020)/Example Film (2020).mkv" {
		t.Errorf("Uncovered[0] = %+v", u)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered path")
	}
	if len(runner.writes) != 0 {
		t.Errorf("refused Write still ran a script: %s", runner.writes)
	}
}

// TestUncoveredRootPathBlocks: a folder outside every rule is a gap.
func TestUncoveredRootPathBlocks(t *testing.T) {
	ctx := context.Background()
	rows := strings.Replace(dbRows, "@section_locations|2|/srv/media/TV",
		"@section_locations|2|/mnt/other/TV", 1)
	in, _ := setup(t, prefsFixture, rows, true)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "section_locations.root_path" {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
}

// TestUncoveredPreferenceBlocks: the same for an attribute.
func TestUncoveredPreferenceBlocks(t *testing.T) {
	ctx := context.Background()
	prefs := strings.Replace(prefsFixture, `TranscoderTempDirectory="/srv/apps/plex/transcode"`,
		`TranscoderTempDirectory="/mnt/other/transcode"`, 1)
	in, _ := setup(t, prefs, dbRows, true)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "TranscoderTempDirectory" {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
}

// TestNeedsTheRunner: a Check that cannot run Plex's shell must say so.
func TestNeedsTheRunner(t *testing.T) {
	ctx := context.Background()
	in, _ := setup(t, prefsFixture, dbRows, true)
	in.Run = nil
	_, err := rewriter{}.Check(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "Plex SQLite") {
		t.Fatalf("Check without a runner = %v, want a refusal naming Plex SQLite", err)
	}
	// Without a database there is nothing the runner would be needed for.
	noDB, _ := setup(t, prefsFixture, dbRows, false)
	if _, err := (rewriter{}).Check(ctx, noDB); err != nil {
		t.Errorf("Check of a Preferences.xml only = %v", err)
	}
}

func TestNoPlexHomeIsRefused(t *testing.T) {
	dir := t.TempDir()
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	_, err = rewriter{}.Check(context.Background(), rewrite.Input{App: "plex", Dir: dir, Plan: p})
	if err == nil || !strings.Contains(err.Error(), prefsName) {
		t.Fatalf("Check of an empty directory = %v, want a refusal naming Preferences.xml", err)
	}
}

// TestDatabaseInTheDataDir: a source mounting the data directory itself has
// Preferences.xml at the config root.
func TestDatabaseInTheDataDir(t *testing.T) {
	ctx := context.Background()
	in, runner := setup(t, "", dbRows, false)
	if err := os.WriteFile(filepath.Join(in.Dir, prefsName), []byte(prefsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(in.Dir, filepath.FromSlash("Plug-in Support/Databases")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.Dir, filepath.FromSlash(dbRel)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("Blocked: %v", rep.Uncovered)
	}
	if len(runner.reads) != 1 {
		t.Errorf("the database at the config root was not read")
	}
}

// treeHash is every file's name, mode and bytes: a pass that writes changes it.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(rel))
		h.Write([]byte(info.Mode().String()))
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestFollowsTheLibrarysCategory: the plan's rule for the library directory is more
// specific than the library's, so roots and files follow it onto the category.
func TestFollowsTheLibrarysCategory(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, filepath.FromSlash(supportDir))
	if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash("Plug-in Support/Databases")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, prefsName), []byte(prefsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(dbRel)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(`version: 2
data:
  root: /srv
  downloads: Downloads
  media: media
  folders:
    - {from: Movies, to: movies}
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    paths:
      - from: /srv/apps/plex
        to: /config
      - from: /srv/plex
        to: /config/plex
      - from: /srv/media/Movies
        to: /data/media/movies
      - from: /srv/media
        to: /data/media
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    category: movies
    paths: []
`))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	runner := &fakeRunner{rows: dbRows}
	in := rewrite.Input{
		App: "plex", Dir: dir, Plan: p,
		Targets: map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Run:     runner.run,
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(runner.writes) != 1 {
		t.Fatalf("Write ran %d scripts, want 1", len(runner.writes))
	}
	script := string(runner.writes[0])
	for _, want := range []string{
		"UPDATE section_locations SET root_path = '/data/media/movies' WHERE id = 1;",
		"UPDATE media_parts SET file = '/data/media/movies/Example Film (2020)/Example Film (2020).mkv' WHERE id = 10;",
		// The source's TV keeps its directory; only the mapped one is renamed.
		"UPDATE section_locations SET root_path = '/data/media/TV' WHERE id = 2;",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the write script is missing %q:\n%s", want, script)
		}
	}
}
