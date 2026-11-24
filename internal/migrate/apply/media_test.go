package apply

// media_test.go is the library's half of the data contract: a migrated box ends
// up with box's own directories (media/movies, media/tv-4k), a rename inside the
// library and never a move, while the download tree keeps every name and every
// torrent.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// mediaFixture is the fixture with a second Sonarr instance whose library is a
// subdirectory of its own, and a plan that renames both onto box's categories.
func mediaFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.writeSonarrInstance("sonarr-4k", "/library/4K/TV", "4K/TV")
	// The plain Sonarr's library directory is called TV like the 4K one's
	// parent, which is what the plan below renames.
	exec(t, filepath.Join(f.configs, "sonarr", "sonarr.db"),
		`UPDATE RootFolders SET Path = '/library/TV'`,
		`UPDATE Series SET Path = '/library/TV/Example Show'`)
	for _, dir := range []string{
		filepath.Join(f.dataRoot, "media", "TV", "Show"),
		filepath.Join(f.dataRoot, "media", "4K", "TV", "Show4k"),
		filepath.Join(f.dataRoot, "Downloads", "TV"),
		filepath.Join(f.dataRoot, "Downloads", "4K", "TV"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(f.dataRoot, "media", "TV", "Show", "ep1.mkv"), "the plain copy\n", 0o644)
	writeFile(t, filepath.Join(f.dataRoot, "media", "4K", "TV", "Show4k", "ep1.mkv"), "the 4K copy\n", 0o644)
	writeFile(t, filepath.Join(f.dataRoot, "Downloads", "TV", "ep1.bin"), "the download\n", 0o644)
	writeFile(t, filepath.Join(f.dataRoot, "Downloads", "4K", "TV", "ep1.bin"), "the 4K download\n", 0o644)
	f.doc = mediaDoc(f, "/srv/apps/sonarr", "/srv/apps/sonarr-4k", "/srv/apps/qbittorrent")
	return f
}

// mediaDoc is the plan discovery writes for a source whose Arrs keep their
// libraries under two directories, and whose downloads are named after the same
// instances.
func mediaDoc(f *fixture, sonarr, sonarr4k, qbit string) string {
	return fmt.Sprintf(`version: 2
data:
  root: %s
  downloads: Downloads
  media: media
  folders:
    - {from: 4K/TV, to: tv-4k}
    - {from: TV, to: tv}
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: %s
    namespace: media
    ports: [8989]
    category: tv
    downloadCategory: TV
    paths:
      - from: /library/TV
        to: /data/media/tv
      - from: /library/Downloads
        to: /data/downloads
  sonarr-4k:
    kind: kubernetes
    source: sonarr4k
    config: %s
    namespace: media
    ports: [8990]
    category: tv-4k
    downloadCategory: 4K/TV
    paths:
      - from: /library/4K/TV
        to: /data/media/tv-4k
      - from: /library/Downloads
        to: /data/downloads
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: %s
    namespace: media
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
hosts:
  - host: 192.0.2.10
    leave: true
`, f.dataRoot, sonarr, sonarr4k, qbit)
}

// mediaEnv is the environment a media run gets: the second instance has an
// endpoint of its own, and the release is not a rehearsal.
func (f *fixture) mediaEnv(v *fakeValues, o *fakeOwned, ask confirmFunc) Env {
	e := f.env(v, o, ask)
	e.Targets["sonarr-4k"] = rewrite.Endpoint{Host: "sonarr-4k", Port: 8990, URLBase: "/sonarr-4k"}
	return e
}

// writeSonarrInstance writes one more Sonarr's config: its database names a
// root folder and a download category of its own.
func (f *fixture) writeSonarrInstance(name, folder, category string) {
	dir := filepath.Join(f.configs, name)
	writeFile(f.t, filepath.Join(dir, "config.xml"), sonarrConfigXML, 0o600)
	newDB(f.t, filepath.Join(dir, "sonarr.db"),
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Title TEXT, Path TEXT)`,
		fmt.Sprintf(`INSERT INTO Series (Id, Title, Path) VALUES (1, 'Example Show', '%s/Example Show')`, folder),
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		fmt.Sprintf(`INSERT INTO RootFolders (Id, Path) VALUES (1, '%s')`, folder),
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		fmt.Sprintf(`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES (1, 'the operator''s own', 'QBittorrent', '{"host":"localhost","port":8080,"tvCategory":"%s"}')`, category),
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER Key, Key TEXT, Value TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Status INTEGER)`,
	)
}

// TestRunRenamesTheLibraryOntoBoxsCategories: the source's TV and 4K/TV become
// media/tv and media/tv-4k, the emptied 4K parent goes, and the download tree is
// not touched at all.
func TestRunRenamesTheLibraryOntoBoxsCategories(t *testing.T) {
	f := mediaFixture(t)
	downloadsBefore := hashTree(t, filepath.Join(f.dataRoot, "Downloads"))
	// The download tree is renamed by the contract like every other source's
	// (Downloads -> downloads), and nothing inside it is touched: the
	// categories, their directories and the torrents stay where they are.
	var shown Summary
	res, err := Run(t.Context(), mustPlan(t, f), []byte(f.doc), f.mediaEnv(&fakeValues{}, &fakeOwned{}, yes(t, &shown)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, rel := range []string{"media/tv/Show/ep1.mkv", "media/tv-4k/Show4k/ep1.mkv"} {
		if _, err := os.Stat(filepath.Join(f.dataRoot, rel)); err != nil {
			t.Errorf("%s is not there after the rename: %v", rel, err)
		}
	}
	for _, rel := range []string{"media/TV", "media/4K"} {
		if _, err := os.Stat(filepath.Join(f.dataRoot, rel)); err == nil {
			t.Errorf("%s is still there; the library was supposed to be renamed out of it", rel)
		}
	}
	sameTree(t, "the downloads tree", downloadsBefore, hashTree(t, filepath.Join(f.dataRoot, "downloads")))
	// The summary is where the operator read it first: one row for the two
	// contract directories and one for the library.
	if row := dataRow(shown, "library"); !strings.Contains(row, "4K/TV → tv-4k · TV → tv") {
		t.Errorf("the confirm's library row is %q, want the library's two renames in it", row)
	}
	if row := dataRow(shown, "data"); !strings.Contains(row, "Downloads → downloads") {
		t.Errorf("the confirm's data row is %q, want the contract directory's rename too", row)
	}
	if len(res.Renamed) != 3 || !strings.Contains(strings.Join(res.Renamed, " "), "media/TV to media/tv") {
		t.Errorf("Renamed = %v, want the contract directory and the two library ones", res.Renamed)
	}
}

// TestRunLeavesTheLibraryAloneInARehearsal: a rehearsal renames nothing on disk,
// and its summary promises nothing it will not do.
func TestRunLeavesTheLibraryAloneInARehearsal(t *testing.T) {
	f := mediaFixture(t)
	before := hashTree(t, f.dataRoot)
	var shown Summary
	e := f.mediaEnv(&fakeValues{}, &fakeOwned{}, yes(t, &shown))
	e.Rehearsal = true
	if _, err := Run(t.Context(), mustPlan(t, f), []byte(f.doc), e); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sameTree(t, "the data root", before, hashTree(t, f.dataRoot))
	for _, name := range []string{"data", "library"} {
		if row := dataRow(shown, name); row != "" {
			t.Errorf("a rehearsal's confirm has a %s row: %q", name, row)
		}
	}
}

// TestRunRefusesAFullTargetBeforeItMoves: a directory already at the category's
// name with files in it is the operator's data, and moving the source onto it
// would either lose it or copy it. Nothing moves.
func TestRunRefusesAFullTargetBeforeItMoves(t *testing.T) {
	f := mediaFixture(t)
	writeFile(t, filepath.Join(f.dataRoot, "media", "tv", "theirs.mkv"), "someone else's\n", 0o644)
	before := hashTree(t, f.dataRoot)
	_, err := Run(t.Context(), mustPlan(t, f), []byte(f.doc), f.mediaEnv(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{})))
	if err == nil || !strings.Contains(err.Error(), "media/tv") {
		t.Fatalf("Run error = %v, want a refusal naming the occupied category", err)
	}
	if !strings.Contains(err.Error(), "media/TV") {
		t.Errorf("the refusal does not name the source directory: %v", err)
	}
	sameTree(t, "the data root", before, hashTree(t, f.dataRoot))
}

// TestRunResumesARenameItDidNotRecord: a run that renamed a directory and
// stopped before recording it leaves the target where it belongs, so the next
// run records it and does the rest instead of renaming the source again.
func TestRunResumesARenameItDidNotRecord(t *testing.T) {
	f := mediaFixture(t)
	if err := os.Rename(filepath.Join(f.dataRoot, "media", "TV"), filepath.Join(f.dataRoot, "media", "tv")); err != nil {
		t.Fatal(err)
	}
	res, err := Run(t.Context(), mustPlan(t, f), []byte(f.doc), f.mediaEnv(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{})))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dataRoot, "media", "tv-4k", "Show4k", "ep1.mkv")); err != nil {
		t.Errorf("the second directory was not renamed: %v", err)
	}
	if strings.Join(res.Renamed, " ") != "Downloads to downloads media/4K/TV to media/tv-4k" {
		t.Errorf("Renamed = %v, want only the directory this run moved, beside the contract one", res.Renamed)
	}
	st := f.state(res.PlanID)
	if !has(st.Renamed, "media/TV") {
		t.Errorf("the rename the earlier run made was not recorded: %v", st.Renamed)
	}
}

// TestRunRenamesNestedDirectoriesInTheLibrarysOwnName: the source's library is
// called something else, so the two contract renames come first and the folders
// are renamed inside the name box gives it.
func TestRunRenamesNestedDirectoriesInTheLibrarysOwnName(t *testing.T) {
	f := mediaFixture(t)
	f.doc = strings.Replace(f.doc, "  media: media\n", "  media: library\n", 1)
	// The library the source named library/, with the same two directories.
	if err := os.Rename(filepath.Join(f.dataRoot, "media"), filepath.Join(f.dataRoot, "library")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), mustPlan(t, f), []byte(f.doc), f.mediaEnv(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dataRoot, "media", "tv-4k", "Show4k", "ep1.mkv")); err != nil {
		t.Errorf("the 4K library directory is not under the renamed library: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dataRoot, "library")); err == nil {
		t.Error("the source's library directory is still there")
	}
}

// dataRow is the confirm's data row as one line, "" when it has none.
// dataRow is the confirm's row for one directory the run renames: the block has
// a row per directory, so a test names the one it is about.
func dataRow(s Summary, name string) string {
	for _, block := range s.Grouped().Blocks {
		for _, row := range block.Rows {
			if row.Name != name {
				continue
			}
			return strings.Join(row.Facts, " · ")
		}
	}
	return ""
}

// mustPlan parses the fixture's plan.
func mustPlan(t *testing.T, f *fixture) *plan.Plan {
	t.Helper()
	p, _ := f.plan()
	return p
}
