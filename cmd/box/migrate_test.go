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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
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
	for _, statement := range []string{
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/media/tv')`,
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Path TEXT)`,
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
	text, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root})
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
	if _, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root, Rehearsal: true}); err != nil {
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
	_, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root})
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
	text, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: empty, Root: empty})
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
	if _, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root}); err != nil {
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
