package main

// migrate_test.go drives `box migrate discover` over a saved manifest set and a
// copy of the source's host directories: no cluster, no API server.
//
// What it pins is the plan directory: plan.yaml must be a file the plan loader
// reads back as the same plan (apply trusts it with the data root), the source
// manifests must be kept beside it so the plan can be rebuilt after the source
// is gone, and a directory that already holds something must be refused rather
// than mixed with a second plan.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestMigrateDiscoverWritesAPlanThatReadsBack(t *testing.T) {
	manifests, root := discoverySource(t)
	out := filepath.Join(t.TempDir(), "plan")
	text, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root})
	if err != nil {
		t.Fatalf("discover: %v\n%s", err, text)
	}

	p, err := plan.Load(filepath.Join(out, "plan.yaml"))
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	if p.Data == nil {
		t.Fatal("the plan adopts no data")
	}
	if p.Data.Root != "/srv/library" || p.Data.Downloads != "Downloads" || p.Data.Media != "media" {
		t.Errorf("data = %+v, want /srv/library with Downloads and media", p.Data)
	}
	for _, name := range []string{"qbittorrent", "sonarr"} {
		app, ok := p.Apps[name]
		if !ok {
			t.Fatalf("the plan has no %s", name)
		}
		if app.Config == "" {
			t.Errorf("%s: the plan names no config directory", name)
		}
	}
	// The manifests the plan was built from are beside it, under the key
	// discovery files them by.
	if _, err := os.Stat(filepath.Join(out, "source", "StatefulSet", "media", "sonarr.json")); err != nil {
		t.Errorf("the source manifest is not kept beside the plan: %v", err)
	}
	// The modes are the plan directory's promise: a plan names a host path.
	for _, name := range []string{"plan.yaml", "report.txt"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %o, want 600", name, info.Mode().Perm())
		}
	}
	// The report says what the plan holds, in the same text the command
	// printed.
	for _, want := range []string{"plan: " + filepath.Join(out, "plan.yaml"), "app: sonarr", "data: /srv/library"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	saved, err := os.ReadFile(filepath.Join(out, "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != text {
		t.Error("report.txt differs from the text the command printed")
	}
}

func TestMigrateDiscoverRefusesANonEmptyOut(t *testing.T) {
	manifests, root := discoverySource(t)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "something.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root})
	if err == nil {
		t.Fatal("the command wrote into a directory that already held something")
	}
	if !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("the refusal is %q", err)
	}
}

func TestMigrateDiscoverReportsAPartialRead(t *testing.T) {
	// No manifests at all: discovery cannot read a source, and the report says
	// so while the command fails.
	empty := t.TempDir()
	out := filepath.Join(t.TempDir(), "plan")
	text, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: empty, Root: empty})
	if err == nil {
		t.Fatal("discovery of an empty directory planned something")
	}
	if !strings.Contains(text, "plan: not written") {
		t.Errorf("the partial report does not say the plan was not written:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(out, "report.txt")); err != nil {
		t.Errorf("the partial report was not kept: %v", err)
	}
}

func TestMigrateDiscoverNeedsADestination(t *testing.T) {
	if _, err := migrateDiscover(context.Background(), discoverOptions{}); err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("no destination was refused with %v", err)
	}
}

// TestMigrateDiscoverReadsAQuotedSavePath is a defect in
// internal/migrate/discover, kept here as a failing test because that package
// belongs to another lane and this one may not change it.
//
// qBittorrent's settings are QSettings, and QSettings quotes a value it has to
// escape: a save path holding a space is written as
// `Session\DefaultSavePath="/srv/My Downloads"`. Discovery reads that value
// verbatim, through qbtconf.Parse's Get, and hands the quoted text to the mount
// lookup, which then covers no host directory and refuses the whole plan with
// "qBittorrent saves under ... which no host-backed mount covers" -- a message
// that names neither the quotes nor the file. The rewriter in
// internal/migrate/qbit unquotes its own values, so a plan that does get built
// works; it is discovery that cannot build one. Skipped rather than removed, so
// fixing discover turns this green instead of leaving a dead fixture behind.
func TestMigrateDiscoverReadsAQuotedSavePath(t *testing.T) {
	t.Skip("internal/migrate/discover: data.go:137-162 reads qBittorrent's save path as written, so a QSettings-quoted path (which qBittorrent writes for one holding a space) keeps its quotes and hostDirOf finds no mount; reported, not fixed in this lane")
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
	out := filepath.Join(t.TempDir(), "plan")
	if _, err := migrateDiscover(context.Background(), discoverOptions{Out: out, FromDir: manifests, Root: root}); err != nil {
		t.Fatalf("discovery refused a save path it should read: %v", err)
	}
}
