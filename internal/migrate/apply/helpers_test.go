package apply

// helpers_test.go builds the fixtures every test in this package starts from: a
// synthetic Sonarr and qBittorrent source tree, an installed seedbox's two host
// paths, and the fakes for the three things apply talks to (the ownership
// record, the values editor and the question).
//
// Everything here is invented: example.com, 192.0.2.0/24, /srv and show titles
// that are plainly not real. The databases are built row by row in the test, so
// what a rewriter reads is the test's own SQL and not a copy of anyone's box.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// The synthetic source's API key. It is in the fixture so a test can prove the
// key in a migrated config is the one the source held, byte for byte: migrate
// carries the file over and never reads or writes a key (docs/box.md, "Keys:
// left alone").
const testAPIKey = "0123456789abcdef0123456789abcdef"

// sonarrConfigXML is an Arr's config.xml, in the flat shape the app writes: the
// elements migrate rewrites, one it must not (ApiKey), and the ones it rewrites
// only when they hold a path.
const sonarrConfigXML = `<?xml version="1.0" encoding="utf-8"?>
<Config>
  <BindAddress>127.0.0.1</BindAddress>
  <Port>8989</Port>
  <SslPort>9898</SslPort>
  <EnableSsl>False</EnableSsl>
  <UrlBase></UrlBase>
  <ApiKey>` + testAPIKey + `</ApiKey>
  <AuthenticationMethod>None</AuthenticationMethod>
  <SslCertPath></SslCertPath>
  <UpdateScriptPath></UpdateScriptPath>
  <LogLevel>info</LogLevel>
</Config>
`

const qbitConf = `[BitTorrent]
Session\DefaultSavePath="/downloads"
Session\TempPath="/downloads/temp"
Session\Port=6881

[WebUI]
Port=8080
`

// fixture is one run's world: the seedbox's config and data roots, the source's
// directories, and the plan that connects them.
type fixture struct {
	t          *testing.T
	root       string
	configRoot string
	dataRoot   string
	// configs is the snapshot's config/ directory: one tree per planned app,
	// which is the only place apply reads an app's config from.
	configs string
	// keys is the release Secret every run in this fixture writes to.
	keys *fakeKeys
	// doc is the plan's YAML, built by planDoc.
	doc string
}

// newFixture builds the source tree and the seedbox's roots under one temporary
// directory. The data root already holds the source's two directory names, one
// of which apply renames.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:          t,
		root:       root,
		configRoot: filepath.Join(root, "config"),
		dataRoot:   filepath.Join(root, "data"),
		configs:    filepath.Join(root, "snapshot", "config"),
		keys:       newFakeKeys(),
	}
	for _, dir := range []string{
		f.configRoot,
		filepath.Join(f.dataRoot, "Downloads"),
		filepath.Join(f.dataRoot, "media"),
		filepath.Join(f.configs, "sonarr"),
		filepath.Join(f.configs, "qbittorrent", "qBittorrent"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.writeSonarr()
	f.writeQbittorrent()
	f.doc = f.planDoc("/srv/apps/sonarr", "/srv/apps/qbittorrent")
	return f
}

// writeSonarr builds the Arr's config directory: its database, the config.xml
// beside it, and a log the migration must carry without reading.
func (f *fixture) writeSonarr() {
	dir := filepath.Join(f.configs, "sonarr")
	writeFile(f.t, filepath.Join(dir, "config.xml"), sonarrConfigXML, 0o600)
	writeFile(f.t, filepath.Join(dir, "logs", "sonarr.txt"), "13-11-2026 10:00:00|Info|Starting\n", 0o644)
	newDB(f.t, filepath.Join(dir, "sonarr.db"),
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Title TEXT, Path TEXT)`,
		`INSERT INTO Series (Id, Title, Path) VALUES (1, 'Example Show', '/library/tv/Example Show')`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/tv')`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES (1, 'qBittorrent', 'QBittorrent', `+
			`'{"host":"localhost","port":8080,"useSsl":false,"urlBase":"","username":"admin","password":"secret","tvCategory":"tv"}')`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`INSERT INTO Config (Id, Key, Value) VALUES (1, 'backupfolder', 'Backups')`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`INSERT INTO History (Id, Data, SourceTitle) VALUES (1, '{"droppedpath":"/library/Downloads/Example.Show.S01E01.nzb"}', 'Example.Show.S01E01')`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Status INTEGER)`,
		`INSERT INTO Commands (Id, Status) VALUES (1, 1)`,
	)
}

func (f *fixture) writeQbittorrent() {
	dir := filepath.Join(f.configs, "qbittorrent")
	writeFile(f.t, filepath.Join(dir, "qBittorrent", "qBittorrent.conf"), qbitConf, 0o600)
	newDB(f.t, filepath.Join(dir, "qBittorrent", "torrents.db"),
		`CREATE TABLE torrents (id INTEGER PRIMARY KEY, target_save_path TEXT, download_path TEXT, libtorrent_resume_data BLOB)`,
		`INSERT INTO torrents (id, target_save_path, download_path, libtorrent_resume_data) VALUES (1, '/downloads/tv', '', NULL)`,
	)
}

// planDoc is the plan a discovery run would have written for this source: the
// library and the downloads as the Arr saw them, the downloads as the client
// saw them, and the data root the grant names.
func (f *fixture) planDoc(sonarrConfig, qbitConfig string) string {
	return fmt.Sprintf(`version: 2
data:
  root: %s
  downloads: Downloads
  media: media
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: %s
    namespace: media
    ports: [8989]
    paths:
      - from: /library/tv
        to: /data/media/tv
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
`, f.dataRoot, sonarrConfig, qbitConfig)
}

func (f *fixture) plan() (*plan.Plan, []byte) {
	f.t.Helper()
	p, err := plan.Parse([]byte(f.doc))
	if err != nil {
		f.t.Fatalf("parse the fixture plan: %v", err)
	}
	return p, []byte(f.doc)
}

// targets is where the release serves the two apps.
func (f *fixture) targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
	}
}

// env is the full environment one run gets, with the fakes in place.
func (f *fixture) env(v *fakeValues, o *fakeOwned, ask confirmFunc) Env {
	return Env{
		ConfigRoot: f.configRoot,
		DataRoot:   f.dataRoot,
		Configs:    f.configs,
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets:    f.targets(),
		Enabled:    func(string) bool { return false },
		Owned:      o,
		Secret:     f.keys,
		Values:     v,
		Confirm:    ask,
		Catalog:    []string{"sonarr", "qbittorrent", "radarr", "prowlarr", "plex"},
	}
}

// confirmFunc records the summary it was shown and answers.
type confirmFunc func(Summary) (bool, error)

// yes is the guided run's answer: show what you were given, say yes.
func yes(t *testing.T, shown *Summary) confirmFunc {
	t.Helper()
	return func(s Summary) (bool, error) {
		*shown = s
		return true, nil
	}
}

// fakeValues is the seedbox's files, as apply edits them.
type fakeValues struct {
	mu     sync.Mutex
	on     []string
	fail   error
	failed bool
}

func (f *fakeValues) Enable(instance, downloadCategory string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		f.failed = true
		return f.fail
	}
	f.on = append(f.on, instance)
	return nil
}

func (f *fakeValues) enabled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.on...)
}

// fakeKeys is the release Secret: one data key at a time, and the values it was
// given, so a test can prove what a run kept.
type fakeKeys struct {
	mu     sync.Mutex
	values map[string]string
	sets   []string
	err    error
}

func newFakeKeys() *fakeKeys { return &fakeKeys{values: map[string]string{}} }

func (f *fakeKeys) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key], nil
}

func (f *fakeKeys) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.values[key] = value
	f.sets = append(f.sets, key)
	return nil
}

// fakeOwned is the release's ownership record.
type fakeOwned struct {
	data   []byte
	writes int
}

func (f *fakeOwned) Read(context.Context) ([]byte, error) { return f.data, nil }

func (f *fakeOwned) Write(_ context.Context, data []byte) error {
	f.data = data
	f.writes++
	return nil
}

// state is the record the run left, read back through the real loader.
func (f *fixture) state(id string) *State {
	f.t.Helper()
	st, err := loadState(stateDir(f.configRoot, id))
	if err != nil {
		f.t.Fatalf("read the state: %v", err)
	}
	return st
}

// readFile reads one file of the fixture, failing the test when it is absent.
func (f *fixture) readFile(rel string) string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.root, rel))
	if err != nil {
		f.t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// writeFile writes one file, creating its directory.
func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// newDB creates an SQLite database with the statements a test wrote, the way a
// stopped app leaves its own file.
func newDB(t *testing.T, path string, statements ...string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exec(t, path, statements...)
}

// exec runs statements against a database a run or an earlier test made.
func exec(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sqlitedb.OpenReadWrite(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// query reads one value out of a database a run left behind.
func query(t *testing.T, path, statement string) string {
	t.Helper()
	db, err := sqlitedb.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var out string
	if err := db.QueryRow(statement).Scan(&out); err != nil {
		t.Fatalf("%s in %s: %v", statement, path, err)
	}
	return out
}

// hashTree is every file under root by its relative path, mode and content, so
// a test can prove a run did not write a byte to a tree. Modification times are
// not part of it: reading a database updates nothing, but copying one must not
// change the original's mtime either, and a rename would.
func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %o", info.Mode().Perm())
		default:
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			out[rel] = fmt.Sprintf("file %o %s", info.Mode().Perm(), hex.EncodeToString(sum[:]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("hash %s: %v", root, err)
	}
	return out
}

// sameTree fails when two hashes disagree, naming the first difference.
func sameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(before)+len(after))
	for k := range before {
		keys = append(keys, k)
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if before[k] != after[k] {
			t.Fatalf("%s changed at %s: %q is now %q", what, k, before[k], after[k])
		}
	}
}

// contains is a small helper for the refusal tests: the message has to name the
// thing the operator has to fix.
func contains(msg, want string) bool { return strings.Contains(msg, want) }
