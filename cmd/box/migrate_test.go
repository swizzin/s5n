package main

// migrate_test.go drives `box migrate discover` over a saved manifest set and a
// copy of the source's host directories: no cluster, no API server.
//
// What it pins is the snapshot: a survey holds the plan, the report and the
// source's manifests and no config at all, --rehearsal adds one config
// directory per planned app, the plan is one `plan.Load` reads back as the same
// plan (apply trusts it with the data root), and the report the command prints
// is the report inside the file, naming that file.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

// sourceManifests is a two-workload source: qBittorrent with the data root
// mounted at /data, and a Sonarr that sees the same directory at /library. Every
// name, image and path is invented.
const sourceManifests = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: qbittorrent
  namespace: media
spec:
  serviceName: qbittorrent
  selector:
    matchLabels:
      app.kubernetes.io/name: qbittorrent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: qbittorrent
    spec:
      containers:
        - name: qbittorrent
          image: ghcr.io/home-operations/qbittorrent:5.3.0
          ports:
            - name: http
              containerPort: 8080
          volumeMounts:
            - name: config
              mountPath: /config
            - name: data
              mountPath: /data
      volumes:
        - name: config
          hostPath:
            path: /srv/apps/qbittorrent
        - name: data
          hostPath:
            path: /srv/library
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: sonarr
  namespace: media
spec:
  serviceName: sonarr
  selector:
    matchLabels:
      app.kubernetes.io/name: sonarr
  template:
    metadata:
      labels:
        app.kubernetes.io/name: sonarr
    spec:
      containers:
        - name: sonarr
          image: ghcr.io/home-operations/sonarr:5.0.0.2907
          ports:
            - name: http
              containerPort: 8989
          volumeMounts:
            - name: config
              mountPath: /config
            - name: library
              mountPath: /library
      volumes:
        - name: config
          hostPath:
            path: /srv/apps/sonarr
        - name: library
          hostPath:
            path: /srv/library
`

// fixedCharts is a chart source with one release behind it: no registry, and
// the chart's own files.
type fixedCharts struct {
	chart release.Chart
	err   error
}

func (f fixedCharts) Load(context.Context, release.ChartRef) (release.Chart, error) {
	return f.chart, f.err
}

// quiet is the reporter a test that reads the returned text does not read: the
// command still says every default out loud, and this is where it goes.
func quiet(t *testing.T) say.Reporter {
	t.Helper()
	return &say.Plain{W: io.Discard}
}

// chartWithVariants is the release a read loads: the chart's own variants.yaml,
// the catalog its apps are named in, and the values whose ports and URL bases
// are what a settled default points at.
func chartWithVariants(t *testing.T) release.Chart {
	t.Helper()
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "s5n", name))
		if err != nil {
			t.Fatalf("read the chart's %s: %v", name, err)
		}
		return raw
	}
	return release.Chart{
		Ref:      testChartRepo + ":" + testChartVersion,
		Name:     "s5n",
		Version:  testChartVersion,
		Variants: read("variants.yaml"),
		Catalog:  read("catalog.yaml"),
		Values:   read("values.yaml"),
		Schema:   read("values.schema.json"),
	}
}

// readOf is what a read of the fixture source is handed: the chart flags every
// command registers, and the release behind them.
func readOf(t *testing.T) discoverOptions {
	t.Helper()
	repo, version := testChartRepo, testChartVersion
	return discoverOptions{
		Box:    testChartVersion,
		chart:  &chartFlags{repo: &repo, version: &version},
		Charts: fixedCharts{chart: chartWithVariants(t)},
	}
}

// discoverySource builds the two directories the command reads: the saved
// manifests and a root that holds the source's host paths as a copy.
func discoverySource(t *testing.T) (manifests, root string) {
	t.Helper()
	manifests = t.TempDir()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(manifests, "source.yaml"), []byte(sourceManifests), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"srv/library/Downloads", "srv/library/media/tv", "srv/apps/qbittorrent/qBittorrent", "srv/apps/sonarr"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// qBittorrent writes a save path unquoted unless it needs quoting, and
	// discovery reads the value as written.
	conf := "[BitTorrent]\nSession\\DefaultSavePath=/data/Downloads\n\n[WebUI]\nPort=8080\n"
	if err := os.WriteFile(filepath.Join(root, "srv/apps/qbittorrent/qBittorrent/qBittorrent.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	xml := "<?xml version=\"1.0\"?>\n<Config>\n  <Port>8989</Port>\n  <ApiKey>0123456789abcdef0123456789abcdef</ApiKey>\n</Config>\n"
	if err := os.WriteFile(filepath.Join(root, "srv/apps/sonarr/config.xml"), []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "srv/apps/sonarr/sonarr.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadWrite(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The tables a real Sonarr's database has and its rewriter reads: a
	// migration checks them, because a plan that could not read them would
	// refuse at apply instead of here.
	for _, statement := range []string{
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/media/tv')`,
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Enable INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return manifests, root
}

func TestMigrateDiscoverWritesASurveyThatHoldsNoConfig(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	text, err := migrateDiscover(context.Background(), o, quiet(t))
	if err != nil {
		t.Fatalf("discover: %v\n%s", err, text)
	}

	m, err := archive.Read(out)
	if err != nil {
		t.Fatalf("the snapshot does not read: %v", err)
	}
	if m.Mode != archive.ModeSurvey {
		t.Errorf("mode = %q, want %q: a plain discover copies no configs", m.Mode, archive.ModeSurvey)
	}
	if m.Kind != archive.KindMigrate {
		t.Errorf("kind = %q, want %q", m.Kind, archive.KindMigrate)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the snapshot is %04o, want 0600: it holds the source's API keys", info.Mode().Perm())
	}

	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	if len(opened.Configs) != 0 {
		t.Errorf("the survey carries configs %v, and a survey carries none", opened.Configs)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	if p.Data == nil || p.Data.Root != "/srv/library" || p.Data.Downloads != "Downloads" || p.Data.Media != "media" {
		t.Errorf("data = %+v, want /srv/library with Downloads and media", p.Data)
	}
	for _, name := range []string{"qbittorrent", "sonarr"} {
		if _, ok := p.Apps[name]; !ok {
			t.Errorf("the plan has no %s", name)
		}
	}
	// The manifests the plan was built from are inside it, under the key
	// discovery files them by.
	if _, ok := opened.Source["StatefulSet/media/sonarr"]; !ok {
		t.Errorf("the source's manifests are not in the snapshot: %v", sourceKeys(opened.Source))
	}
	// The report says where the snapshot is and what the plan holds, and the
	// text the command printed is the text inside the file.
	for _, want := range []string{"plan: " + out, "app: sonarr", "data: /srv/library"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	if string(opened.Report) != text {
		t.Errorf("the snapshot's report differs from what the command printed:\n%s", opened.Report)
	}
}

func TestMigrateDiscoverRehearsalCarriesEveryConfig(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root, o.Rehearsal = out, manifests, root, true
	if _, err := migrateDiscover(context.Background(), o, quiet(t)); err != nil {
		t.Fatalf("discover --rehearsal: %v", err)
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	if opened.Mode != archive.ModeRehearsal {
		t.Fatalf("mode = %q, want %q", opened.Mode, archive.ModeRehearsal)
	}
	for _, name := range []string{"qbittorrent", "sonarr"} {
		dir, ok := opened.Configs[name]
		if !ok {
			t.Fatalf("the rehearsal carries no config for %s: %v", name, sourceKeys(opened.Configs))
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s's config is not a directory in the snapshot: %v", name, err)
		}
	}
	// The Arr's whole tree travels, its database included: apply rewrites the
	// copy, and a file it never reads has to come across too.
	if _, err := os.Stat(filepath.Join(opened.Configs["sonarr"], "sonarr.db")); err != nil {
		t.Errorf("the rehearsal does not carry sonarr's database: %v", err)
	}
}

func TestMigrateDiscoverRefusesToWriteOverASnapshot(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(out, []byte("a snapshot that is already there"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	_, err := migrateDiscover(context.Background(), o, quiet(t))
	if err == nil || !strings.Contains(err.Error(), "there already") {
		t.Fatalf("discover over an existing file was refused with %v", err)
	}
}

func TestMigrateDiscoverReportsAPartialRead(t *testing.T) {
	// No manifests at all: discovery cannot read a source, and the report says
	// so while the command fails. Nothing is written: there is no plan to put
	// in a snapshot.
	empty := t.TempDir()
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, empty, empty
	text, err := migrateDiscover(context.Background(), o, quiet(t))
	if err == nil {
		t.Fatal("discovery of an empty directory planned something")
	}
	if !strings.Contains(text, "plan: not written") {
		t.Errorf("the partial report does not say the plan was not written:\n%s", text)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a failed read left a snapshot behind: %v", err)
	}
}

// TestMigrateDiscoverDefaultsToTheSnapshotDirectory: with no --out the snapshot
// goes where apply looks for it, under a name that says when it was taken and
// what it is.
func TestMigrateDiscoverDefaultsToTheSnapshotDirectory(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 5, 3, 0, time.UTC)
	o := discoverOptions{Now: func() time.Time { return at }}
	if got, want := snapshotPath(o, archive.ModeSurvey), filepath.Join(bundle.Dir, "migrate-20261117T090503Z-survey.tar.gz"); got != want {
		t.Errorf("survey path = %q, want %q", got, want)
	}
	if got, want := snapshotPath(o, archive.ModeFinal), filepath.Join(bundle.Dir, "migrate-20261117T090503Z-final.tar.gz"); got != want {
		t.Errorf("final path = %q, want %q", got, want)
	}
	// --out wins, because a snapshot from another host is the file to pass.
	o.Out = "/media/usb/source.tar.gz"
	if got := snapshotPath(o, archive.ModeFinal); got != o.Out {
		t.Errorf("--out was ignored: %q", got)
	}
}

// TestMigrateDiscoverReadsAQuotedSavePath is a defect in
// internal/migrate/discover, kept here as a failing test because that package
// belongs to another lane and this one may not change it.
//
// qBittorrent's settings are QSettings, and QSettings quotes a value it has to
// escape: a save path holding a space is written as
// `Session\DefaultSavePath="/srv/My Downloads"`. Read verbatim, the quoted text
// covers no host mount and refuses the whole plan with a message that names
// neither the quotes nor the file. The rewriter in
// internal/migrate/qbit unquotes its own values, and discovery now reads the
// value through the same qbtconf.Unquote, so the two agree on the path.
func TestMigrateDiscoverReadsAQuotedSavePath(t *testing.T) {
	manifests, root := discoverySource(t)
	// The same source, with the downloads one directory deeper so the path
	// holds a space, which is what makes QSettings quote it.
	moved := filepath.Join(root, "srv", "library", "My Downloads")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "[BitTorrent]\nSession\\DefaultSavePath=\"/data/My Downloads\"\n\n[WebUI]\nPort=8080\n"
	if err := os.WriteFile(filepath.Join(root, "srv/apps/qbittorrent/qBittorrent/qBittorrent.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	if _, err := migrateDiscover(context.Background(), o, quiet(t)); err != nil {
		t.Fatalf("discovery refused a save path it should read: %v", err)
	}
}

// sourceKeys is a map's keys, for a failure message.
func sourceKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMigrateDiscoverMapsImagesOntoTheChartsVariants: a read is handed the
// release's variants.yaml, so a source image the chart ships maps onto that
// variant in the plan. Without it every app keeps the chart's default image and
// nothing says so.
func TestMigrateDiscoverMapsImagesOntoTheChartsVariants(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	text, err := migrateDiscover(context.Background(), o, quiet(t))
	if err != nil {
		t.Fatalf("discover: %v\n%s", err, text)
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	// Both fixture images are exactly what the chart ships, so both map onto
	// the variant they already are.
	for _, name := range []string{"qbittorrent", "sonarr"} {
		app := p.Apps[name]
		if app.Image == "" {
			t.Fatalf("the plan has no %s", name)
		}
		if app.Variant != "default" {
			t.Errorf("%s's image %s mapped onto variant %q, want the one the chart ships it as", name, app.Image, app.Variant)
		}
	}
	if strings.Contains(text, "no chart variants") {
		t.Errorf("the report says the read had no variants, and it was handed the chart's:\n%s", text)
	}
}

// TestMigrateDiscoverRefusesAChartWithNoVariants: the file is the chart's, and
// a read that cannot map an image to a variant would plan a migration that
// silently keeps the chart's default image for every app.
func TestMigrateDiscoverRefusesAChartWithNoVariants(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	chart := chartWithVariants(t)
	chart.Variants = nil
	o.Charts = fixedCharts{chart: chart}
	text, err := migrateDiscover(context.Background(), o, quiet(t))
	if err == nil || !strings.Contains(err.Error(), "variants.yaml") {
		t.Fatalf("a chart with no variants was accepted: %v\n%s", err, text)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused read wrote a snapshot: %v", err)
	}
}

// plexSourceTree builds the config directory a source Plex keeps: the two files
// a migration needs, the companion a database writes while it is open, the
// Metadata and Media trees of artwork and preview thumbnails it built, the caches
// it rebuilds, and one file of the config root that is not Plex's at all.
func plexSourceTree(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "srv", "apps", "plex")
	server := filepath.Join(dir, "Library", "Application Support", "Plex Media Server")
	for name, body := range map[string]string{
		filepath.Join(server, "Preferences.xml"):                                                    "<Preferences/>\n",
		filepath.Join(server, "Plug-in Support", "Databases", "com.plexapp.plugins.library.db"):     "sqlite\n",
		filepath.Join(server, "Plug-in Support", "Databases", "com.plexapp.plugins.library.db-wal"): "wal\n",
		filepath.Join(server, "Metadata", "Posters", "poster.jpg"):                                  "jpeg\n",
		filepath.Join(server, "Media", "localhost", "bif", "index.bif"):                             "bif\n",
		filepath.Join(server, "Cache", "PhotoTranscoder", "thumb.jpg"):                              "jpeg\n",
		filepath.Join(server, "Plug-in Support", "Caches", "c.bin"):                                 "cache\n",
		filepath.Join(dir, ".s5n-e2e-source"):                                                       "marker\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWriteSnapshotCarriesPlexsStateOrItsWholeData: the plan says how much of a
// Plex config directory the snapshot takes, and the snapshot carries exactly that
// -- its state alone, or its whole data less the caches Plex rebuilds -- while
// every other app's config travels whole. The one line about it is what the
// report and the confirm read, and it is a different line for each.
func TestWriteSnapshotCarriesPlexsStateOrItsWholeData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		carry string
		state bool
	}{
		{name: "its state alone", carry: plan.CarryMinimal, state: true},
		{name: "its whole data", carry: plan.CarryFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, opened := snapshotOfPlex(t, tc.carry)
			checkPlexSnapshot(t, opened, tc.state)
			checkCarryLine(t, p, tc.name, tc.state)
		})
	}
}

// checkPlexSnapshot is what one snapshot carries of Plex: its state always, the
// library only when the plan says its whole data, and never a cache or the mount
// around its home. The other app's config travels whole either way.
func checkPlexSnapshot(t *testing.T, opened *bundle.Opened, state bool) {
	t.Helper()
	plex := opened.Configs["plex"]
	for _, f := range []string{"Preferences.xml", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		if !inSnapshot(t, plex, plexHome(f)) {
			t.Errorf("the snapshot carries no %s, and Plex starts on it", f)
		}
	}
	for _, f := range []string{"Metadata/Posters/poster.jpg", "Media/localhost/bif/index.bif"} {
		if got := inSnapshot(t, plex, plexHome(f)); got == state {
			t.Errorf("the snapshot carries the library's %s when the plan says the state alone: %v", f, got)
		}
	}
	for _, f := range []string{"Cache/PhotoTranscoder/thumb.jpg", "Plug-in Support/Caches/c.bin"} {
		if inSnapshot(t, plex, plexHome(f)) {
			t.Errorf("the snapshot carries %s, a cache Plex rebuilds", f)
		}
	}
	// The config root is not Plex's home: a full carry is the tree Plex keeps,
	// not whatever else the source's directory holds.
	if inSnapshot(t, plex, ".s5n-e2e-source") {
		t.Error("the snapshot carries the source's own file at the config root")
	}
	// The other app's config is its state: an archive that trimmed it would be
	// one apply could not use.
	if !inSnapshot(t, opened.Configs["sonarr"], "logs/sonarr.txt") {
		t.Error("sonarr's whole config did not travel")
	}
}

// checkCarryLine is the one line about what the snapshot carries: the report's
// and the confirm's, and one for the one app whose config is more than its state.
func checkCarryLine(t *testing.T, p *plan.Plan, name string, state bool) {
	t.Helper()
	lines := carryLines(p)
	if len(lines) != 1 {
		t.Fatalf("the read's one line about Plex is %v", lines)
	}
	want, not := "its whole data", "carried Preferences.xml and its databases;"
	if state {
		want, not = not, "its whole data"
	}
	if !strings.Contains(lines[0], want) || strings.Contains(lines[0], not) {
		t.Errorf("the line about a %s snapshot is %q, and it should say %q", name, lines[0], want)
	}
}

// snapshotOfPlex is a read of the fixture source, written as a final snapshot with
// the plan saying which of Plex's two carries it is, and the snapshot opened
// again: what a test compares is what apply would read.
func snapshotOfPlex(t *testing.T, carry string) (*plan.Plan, *bundle.Opened) {
	t.Helper()
	root := t.TempDir()
	plexSourceTree(t, root)
	sonarr := filepath.Join(root, "srv", "apps", "sonarr")
	if err := os.MkdirAll(filepath.Join(sonarr, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(sonarr, "config.xml"):         "<Config/>\n",
		filepath.Join(sonarr, "sonarr.db"):          "sqlite\n",
		filepath.Join(sonarr, "logs", "sonarr.txt"): "starting\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := plan.Parse([]byte(`version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    carry: ` + carry + `
    paths: []
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    paths: []
`))
	if err != nil {
		t.Fatalf("the fixture plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "final.tar.gz")
	read := migrationRead{plan: p, text: "plan: the read\n"}
	if err := encodeRead(&read); err != nil {
		t.Fatalf("encode the read: %v", err)
	}
	o := discoverOptions{Root: root, Box: testChartVersion}
	if _, err := writeSnapshot(path, archive.ModeFinal, o, read); err != nil {
		t.Fatalf("write the snapshot: %v", err)
	}
	opened, err := bundle.Open(path, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	return p, opened
}

// inSnapshot is whether one file of an opened config tree is there.
func inSnapshot(t *testing.T, config, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(config, filepath.FromSlash(rel)))
	return err == nil
}

// plexHome is one of Plex's files as the config tree holds it: its home is under
// Library/Application Support, which is what the image mounts.
func plexHome(rel string) string {
	return "Library/Application Support/Plex Media Server/" + rel
}
