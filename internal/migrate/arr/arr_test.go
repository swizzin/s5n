package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// sonarrPlan is the plan a discovery run would write for a Docker source: the
// library and the downloads sit under /library, qBittorrent sees its downloads
// at /downloads, and a Plex server answers on 32400. Every path in it is
// invented.
const sonarrPlan = `version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
    paths:
      - from: /library/media
        to: /data/media
      - from: /library/Downloads
        to: /data/downloads
      - from: /library/tls
        to: /config/tls
      - from: /library/scripts
        to: /config/scripts
      # What discovery writes on a second run, when the tree already sits where
      # box wants it; a check of a migrated copy needs them to be clean.
      - from: /data/media
        to: /data/media
      - from: /data/downloads
        to: /data/downloads
      - from: /config/tls
        to: /config/tls
      - from: /config/scripts
        to: /config/scripts
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
hosts:
  - host: 192.0.2.50
    leave: true
`

// targets is what the release serves each app at.
var targets = map[string]rewrite.Endpoint{
	"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
	"qbittorrent": {Host: "qbittorrent", Port: 8080},
	"plex":        {Host: "plex", Port: 32400},
}

func testPlan(t *testing.T, doc string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse the test plan: %v", err)
	}
	return p
}

// newDB creates an SQLite file with the given statements, the way a stopped
// app leaves its own: the schema comes from the app, not from this package.
func newDB(t *testing.T, path string, statements ...string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadWrite(context.Background(), path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// query runs one read against a database the test built.
func query(t *testing.T, path, sql string, args ...any) [][]any {
	t.Helper()
	db, err := sqlitedb.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	rows, err := db.Query(sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// one reads the first column of every row as a string.
func one(t *testing.T, path, sql string) []string {
	t.Helper()
	var out []string
	for _, row := range query(t, path, sql) {
		switch v := row[0].(type) {
		case string:
			out = append(out, v)
		case []byte:
			out = append(out, string(v))
		case nil:
			out = append(out, "")
		case int64:
			out = append(out, strconv.FormatInt(v, 10))
		}
	}
	return out
}

func setting(t *testing.T, path, sql string) map[string]any {
	t.Helper()
	rows := query(t, path, sql)
	if len(rows) != 1 {
		t.Fatalf("%s: %d rows, want 1", sql, len(rows))
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(rows[0][0].(string)), &f); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// dirBytes is every file under dir, so a test can prove a pass wrote nothing.
func dirBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = data
	}
	return out
}

func input(t *testing.T, dir, app, doc string) rewrite.Input {
	t.Helper()
	return rewrite.Input{App: app, Dir: dir, Plan: testPlan(t, doc), Targets: targets}
}

// sonarrSchema is every table this rewriter reads, as Sonarr 4.0.14 creates
// them: the columns it touches, with the rest left out.
func sonarrSchema() []string {
	return []string{
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, EnableAutomaticAdd INTEGER, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Enable INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
	}
}

// lidarrSchema is Lidarr's tables at v3.1.2.4902, trimmed to the columns this
// rewriter reads. It differs from Sonarr's and Radarr's in two ways that matter
// here: the media table is Artists, and TrackFiles keeps the absolute path of
// every file (migration 030 dropped RelativePath), where Sonarr's and Radarr's
// file paths are computed from the library folder.
func lidarrSchema() []string {
	return []string{
		`CREATE TABLE Artists (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE Albums (Id INTEGER PRIMARY KEY, Title TEXT)`,
		`CREATE TABLE TrackFiles (Id INTEGER PRIMARY KEY, Path TEXT, AlbumId INTEGER)`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, EnableAutomaticAdd INTEGER, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Enable INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
	}
}

func radarrSchema() []string {
	return []string{
		`CREATE TABLE Movies (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE Collections (Id INTEGER PRIMARY KEY, RootFolderPath TEXT)`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, EnableAutomaticAdd INTEGER, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
	}
}

// prowlarrSchema is Prowlarr's tables: no library, no root folders, no remote
// path mappings (prowlarr@v2.7.0.5723:src/NzbDrone.Core/Datastore/TableMapping.cs:41-101).
func prowlarrSchema() []string {
	return []string{
		`CREATE TABLE Applications (Id INTEGER PRIMARY KEY, Name TEXT, SyncLevel INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, Data TEXT)`,
	}
}

// sonarrStatements is the source database: one row per shape §2.1 and §2.2
// lists, with the paths an invented library would have.
func sonarrStatements() []string {
	return append(sonarrSchema(),
		`INSERT INTO Series (Id, Path) VALUES (1, '/library/media/tv/Night Shift')`,
		`INSERT INTO Series (Id, Path) VALUES (2, '/library/media/tv/Blue Ferry')`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/media/tv')`,
		`INSERT INTO ImportLists (Id, Name, Implementation, Settings, RootFolderPath) VALUES
			(1, 'Another Sonarr', 'Sonarr', '{"baseUrl":"http://localhost:8989","apiKey":"k","rootFolderPaths":["/library/media/tv"]}', '/library/media/tv')`,
		// The host a mapping stores is the download client's host, so this one
		// inherits the qBittorrent client's path table.
		`INSERT INTO RemotePathMappings (Id, Host, RemotePath, LocalPath) VALUES (1, 'localhost', '/downloads/tv', '/library/Downloads/tv')`,
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES
			(1, 'qBit', 'QBittorrent', '{"host":"localhost","port":8080,"urlBase":"/","useSsl":true,"tvCategory":"TV"}'),
			(2, 'seedbox', 'Transmission', '{"host":"192.0.2.50","port":9091,"useSsl":true,"tvDirectory":"/srv/remote/tv"}'),
			(3, 'blackhole', 'TorrentBlackhole', '{"torrentFolder":"/library/Downloads/watch","watchFolder":"/library/Downloads/watch"}')`,
		`INSERT INTO Notifications (Id, Name, Implementation, Settings) VALUES
			(1, 'script', 'CustomScript', '{"path":"/library/scripts/import.sh"}'),
			(2, 'plex', 'PlexServer', '{"host":"localhost","port":32400,"useSsl":true,"authToken":"t"}')`,
		`INSERT INTO Config (Id, Key, Value) VALUES
			(1, 'backupfolder', 'Backups'),
			(2, 'scriptimportpath', '/library/scripts/import.sh'),
			(3, 'chmodfolder', '755')`,
		`INSERT INTO Commands (Id, Name, Status, Body) VALUES
			(1, 'DownloadedEpisodesScan', 1, '{"path":"/library/Downloads/tv"}'),
			(2, 'RefreshSeries', 2, '{}')`,
		`INSERT INTO History (Id, SourceTitle, Data) VALUES
			(1, '/library/media/tv/Night Shift/Season 01/e01.mkv',
			 '{"droppedPath":"/library/Downloads/tv/e01","importedPath":"/library/media/tv/Night Shift/Season 01/e01.mkv","releaseGroup":"GRP"}'),
			(2, 'Some.Release-GRP', '{"droppedPath":"/mnt/elsewhere/e01","releaseGroup":"GRP"}')`,
		`INSERT INTO DownloadHistory (Id, SourceTitle, Data) VALUES
			(1, '/library/Downloads/tv/e01', '{"sourcePath":"/library/Downloads/tv/e01","destinationPath":"/mnt/elsewhere/e01"}')`,
	)
}

func sonarrConfig() string {
	return `<Config>
  <LogLevel>info</LogLevel>
  <UpdateMechanism>Docker</UpdateMechanism>
  <EnableSsl>False</EnableSsl>
  <Port>8989</Port>
  <SslPort>9898</SslPort>
  <UrlBase>/sonarr-old</UrlBase>
  <BindAddress>127.0.0.1</BindAddress>
  <ApiKey>0123456789abcdef0123456789abcdef</ApiKey>
  <AuthenticationMethod>Forms</AuthenticationMethod>
  <SslCertPath>/library/tls/sonarr.pfx</SslCertPath>
  <SslKeyPath></SslKeyPath>
  <UpdateScriptPath>/library/scripts/update.sh</UpdateScriptPath>
</Config>`
}

// sonarrSource builds a config directory the way a stopped Sonarr leaves one.
func sonarrSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), sonarrStatements()...)
	writeFile(t, filepath.Join(dir, "config.xml"), sonarrConfig())
	return dir
}

func TestSonarrRewritesEveryPath(t *testing.T) {
	dir := sonarrSource(t)
	in := input(t, dir, "sonarr", sonarrPlan)
	before := dirBytes(t, dir)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !reflect.DeepEqual(dirBytes(t, dir), before) {
		t.Fatal("Check changed the config directory, which it only reads")
	}
	if len(rep.Uncovered) != 0 {
		t.Errorf("uncovered = %v, want none: every stored path has a rule", rep.Uncovered)
	}
	if len(rep.Unclassified) != 0 {
		t.Errorf("unclassified = %v, want none: the plan leaves the LAN client alone", addressStrings(rep.Unclassified))
	}
	if rep.Blocked() {
		t.Errorf("report blocked the run: %v", rep.Uncovered)
	}
	wantOwned := []rewrite.Owned{{Kind: "downloadclient", ID: 1, Name: "qBit", Target: "qbittorrent"}}
	if !reflect.DeepEqual(rep.Owned, wantOwned) {
		t.Errorf("owned = %v, want %v", rep.Owned, wantOwned)
	}
	assertChanges(t, rep.Changes, map[string]int{
		"sonarr.db Series.Path":                            2,
		"sonarr.db RootFolders.Path":                       1,
		"sonarr.db ImportLists.RootFolderPath":             1,
		"sonarr.db DownloadClients.Settings":               1,
		"sonarr.db DownloadClients.Settings.torrentFolder": 1,
		"sonarr.db DownloadClients.Settings.watchFolder":   1,
		"sonarr.db Notifications.Settings.path":            1,
		"sonarr.db Notifications.Settings":                 1,
		"sonarr.db ImportLists.Settings":                   1,
		"sonarr.db Config.scriptimportpath":                1,
		"sonarr.db Commands":                               1,
		"sonarr.db History.Data.droppedPath":               1,
		"sonarr.db History.Data.importedPath":              1,
		"sonarr.db History.SourceTitle":                    1,
		"sonarr.db DownloadHistory.Data.sourcePath":        1,
		"sonarr.db DownloadHistory.SourceTitle":            1,
		"sonarr.db RemotePathMappings":                     1,
		"config.xml BindAddress":                           1,
		"config.xml UrlBase":                               1,
		"config.xml SslCertPath":                           1,
		"config.xml UpdateScriptPath":                      1,
	})

	got, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !reflect.DeepEqual(got, rep) {
		t.Errorf("Write reported %+v, Check reported %+v", got, rep)
	}

	assertSonarrDatabase(t, dir)
	assertSonarrConfig(t, dir)

	// Nothing left to do: a second check finds no change, which is what "write
	// makes exactly what check reported" means for the next run.
	rep2, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("second Check still reports changes: %+v", rep2.Changes)
	}
	if len(rep2.Unclassified) != 0 || rep2.Blocked() {
		t.Errorf("second Check blocks on the pass's own work: %v %v", rep2.Uncovered, addressStrings(rep2.Unclassified))
	}
}

// assertSonarrDatabase checks every column and settings blob the pass rewrote.
func assertSonarrDatabase(t *testing.T, dir string) {
	t.Helper()
	db := filepath.Join(dir, "sonarr.db")
	assertSonarrRows(t, db)
	assertSonarrSettings(t, db)
	assertSonarrHistory(t, db)
}

func assertSonarrRows(t *testing.T, db string) {
	t.Helper()
	if paths := one(t, db, "SELECT Path FROM Series ORDER BY Id"); !reflect.DeepEqual(paths,
		[]string{"/data/media/tv/Night Shift", "/data/media/tv/Blue Ferry"}) {
		t.Errorf("Series.Path = %v", paths)
	}
	if paths := one(t, db, "SELECT Path FROM RootFolders"); !reflect.DeepEqual(paths, []string{"/data/media/tv"}) {
		t.Errorf("RootFolders.Path = %v", paths)
	}
	if got := one(t, db, "SELECT RootFolderPath FROM ImportLists"); !reflect.DeepEqual(got, []string{"/data/media/tv"}) {
		t.Errorf("ImportLists.RootFolderPath = %v", got)
	}
	if rows := one(t, db, "SELECT Id FROM RemotePathMappings"); len(rows) != 0 {
		t.Errorf("identity remote path mapping survived: %v", rows)
	}
	if rows := one(t, db, "SELECT Id FROM Commands ORDER BY Id"); !reflect.DeepEqual(rows, []string{"2"}) {
		t.Errorf("Commands = %v, want only the finished one", rows)
	}
	if got := one(t, db, "SELECT Value FROM Config WHERE Key = 'backupfolder'"); !reflect.DeepEqual(got, []string{"Backups"}) {
		t.Errorf("relative backupfolder = %v, want it left alone", got)
	}
	if got := one(t, db, "SELECT Value FROM Config WHERE Key = 'scriptimportpath'"); !reflect.DeepEqual(got, []string{"/config/scripts/import.sh"}) {
		t.Errorf("scriptimportpath = %v", got)
	}
}

func assertSonarrSettings(t *testing.T, db string) {
	t.Helper()
	qbit := setting(t, db, "SELECT Settings FROM DownloadClients WHERE Id = 1")
	if qbit["host"] != "qbittorrent" || qbit["port"] != float64(8080) || qbit["useSsl"] != false || qbit["urlBase"] != "" {
		t.Errorf("qBittorrent client = %v, want the box's endpoint with HTTPS off", qbit)
	}
	if qbit["tvCategory"] != "TV" {
		t.Errorf("category name changed: %v", qbit)
	}
	remote := setting(t, db, "SELECT Settings FROM DownloadClients WHERE Id = 2")
	if remote["host"] != "192.0.2.50" || remote["tvDirectory"] != "/srv/remote/tv" || remote["useSsl"] != true {
		t.Errorf("a client the plan leaves moved: %v", remote)
	}
	blackhole := setting(t, db, "SELECT Settings FROM DownloadClients WHERE Id = 3")
	if blackhole["torrentFolder"] != "/data/downloads/watch" || blackhole["watchFolder"] != "/data/downloads/watch" {
		t.Errorf("blackhole folders = %v", blackhole)
	}
	list := setting(t, db, "SELECT Settings FROM ImportLists")
	if list["baseUrl"] != "http://sonarr:8989/sonarr" {
		t.Errorf("ImportLists baseUrl = %v", list["baseUrl"])
	}
	if paths, ok := list["rootFolderPaths"].([]any); !ok || len(paths) != 1 || paths[0] != "/library/media/tv" {
		t.Errorf("rootFolderPaths = %v, want the remote filter left alone", list["rootFolderPaths"])
	}
	script := setting(t, db, "SELECT Settings FROM Notifications WHERE Id = 1")
	if script["path"] != "/config/scripts/import.sh" {
		t.Errorf("CustomScript path = %v", script["path"])
	}
	plex := setting(t, db, "SELECT Settings FROM Notifications WHERE Id = 2")
	if plex["host"] != "plex" || plex["port"] != float64(32400) || plex["useSsl"] != false {
		t.Errorf("Plex notification = %v", plex)
	}
}

func assertSonarrHistory(t *testing.T, db string) {
	t.Helper()
	history := one(t, db, "SELECT Data FROM History ORDER BY Id")
	var first map[string]string
	if err := json.Unmarshal([]byte(history[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["droppedPath"] != "/data/downloads/tv/e01" || first["importedPath"] != "/data/media/tv/Night Shift/Season 01/e01.mkv" {
		t.Errorf("History paths = %v", first)
	}
	if history[1] != `{"droppedPath":"/mnt/elsewhere/e01","releaseGroup":"GRP"}` {
		t.Errorf("a history row no rule covers was changed: %s", history[1])
	}
	if titles := one(t, db, "SELECT SourceTitle FROM History ORDER BY Id"); !reflect.DeepEqual(titles,
		[]string{"/data/media/tv/Night Shift/Season 01/e01.mkv", "Some.Release-GRP"}) {
		t.Errorf("History.SourceTitle = %v", titles)
	}
	download := one(t, db, "SELECT Data FROM DownloadHistory")
	if download[0] != `{"destinationPath":"/mnt/elsewhere/e01","sourcePath":"/data/downloads/tv/e01"}` {
		t.Errorf("DownloadHistory = %s", download[0])
	}
}

// assertSonarrConfig checks the elements config.xml had to change, and the
// ones it had to leave alone.
func assertSonarrConfig(t *testing.T, dir string) {
	t.Helper()
	conf := readFile(t, filepath.Join(dir, "config.xml"))
	if !strings.Contains(conf, "<ApiKey>0123456789abcdef0123456789abcdef</ApiKey>") {
		t.Errorf("config.xml lost its API key: %s", conf)
	}
	for _, want := range []string{
		"<Port>8989</Port>",
		"<UrlBase>/sonarr</UrlBase>",
		"<BindAddress>*</BindAddress>",
		"<SslCertPath>/config/tls/sonarr.pfx</SslCertPath>",
		"<SslKeyPath></SslKeyPath>",
		"<UpdateScriptPath>/config/scripts/update.sh</UpdateScriptPath>",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config.xml does not contain %q:\n%s", want, conf)
		}
	}
	if strings.Index(conf, "<LogLevel>") > strings.Index(conf, "<Port>") {
		t.Errorf("config.xml order changed:\n%s", conf)
	}
}

func TestUncoveredLivePathBlocks(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO Series (Id, Path) VALUES (1, '/mnt/elsewhere/tv')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)
	before := dirBytes(t, dir)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rep.Blocked() {
		t.Fatal("a live path no rule covers did not block the run")
	}
	want := []plan.Uncovered{{App: "sonarr", File: "sonarr.db", Key: "Series.Path", Path: "/mnt/elsewhere/tv"}}
	if !reflect.DeepEqual(rep.Uncovered, want) {
		t.Errorf("uncovered = %v, want %v", rep.Uncovered, want)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err == nil {
		t.Fatal("Write ran over a gap Check found")
	}
	if !reflect.DeepEqual(dirBytes(t, dir), before) {
		t.Error("a refused Write changed the config directory")
	}
}

func TestHistoryNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO History (Id, SourceTitle, Data) VALUES (1, '/mnt/elsewhere/e01.mkv', '{"droppedPath":"/mnt/elsewhere/e01"}')`,
		`INSERT INTO DownloadHistory (Id, SourceTitle, Data) VALUES (1, '/mnt/elsewhere/e01', '{"sourcePath":"/mnt/elsewhere/e01"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("history blocked the run: %v", rep.Uncovered)
	}
	if len(rep.Uncovered) != 0 {
		t.Errorf("uncovered = %v, want a history path never reported", rep.Uncovered)
	}
	for _, c := range rep.Changes {
		if strings.HasPrefix(c.Where, "History") || strings.HasPrefix(c.Where, "DownloadHistory") {
			t.Errorf("a history row is in the report as %+v", c)
		}
	}
	if len(rep.Changes) == 0 {
		t.Error("a config.xml with the wrong bind address reported no change")
	}
}

func TestUnclassifiedClientReported(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES
			(1, 'seedbox', 'QBittorrent', '{"host":"192.0.2.77","port":8080,"useSsl":true,"tvCategory":"tv"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	// The plan from above leaves 192.0.2.50 alone; this client is another
	// address, so nothing settles it.
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []rewrite.Address{{
		App: "sonarr", File: "sonarr.db", Where: "DownloadClients.Settings.host", Host: "192.0.2.77", Port: 8080,
	}}
	if !reflect.DeepEqual(rep.Unclassified, want) {
		t.Errorf("unclassified = %v, want %v", addressStrings(rep.Unclassified), addressStrings(want))
	}
	if !rep.Blocked() {
		t.Error("an address no rule settles did not block the run")
	}
}

// TestDisabledClientIsLeft: a client the app never dials is not a gap. Its
// address stays exactly as it was, the report says so, and the run is not
// stopped by an address no rule settles.
func TestDisabledClientIsLeft(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO DownloadClients (Id, Name, Enable, Implementation, Settings) VALUES
			(1, 'old box', 0, 'QBittorrent', '{"host":"192.0.2.77","port":8080,"useSsl":true,"tvCategory":"tv"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("a disabled client blocked the run: %v", addressStrings(rep.Unclassified))
	}
	if len(rep.Left) != 1 || rep.Left[0].What != "192.0.2.77:8080" {
		t.Fatalf("Left = %+v, want one entry for 192.0.2.77:8080", rep.Left)
	}
	if !strings.Contains(rep.Left[0].Why, "switched off") {
		t.Errorf("why = %q, want it to say the client is switched off", rep.Left[0].Why)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := setting(t, filepath.Join(dir, "sonarr.db"), "SELECT Settings FROM DownloadClients WHERE Id = 1"); got["host"] != "192.0.2.77" {
		t.Errorf("a disabled client's row was rewritten: %v", got)
	}
}

// TestDisabledSyncIsLeft: an import list or a Prowlarr application the app has
// switched off is never dialled either, and each app stores that flag in its own
// column (ImportListDefinition.Enable is EnableAutomaticAdd,
// ApplicationDefinition.Enable is SyncLevel). A base URL nothing settles there is
// left and called out, not a refusal.
func TestDisabledSyncIsLeft(t *testing.T) {
	ctx := context.Background()
	t.Run("import list", func(t *testing.T) {
		dir := t.TempDir()
		newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
			`INSERT INTO ImportLists (Id, Name, Implementation, Settings, RootFolderPath, EnableAutomaticAdd) VALUES
				(1, 'off', 'Sonarr', '{"baseUrl":"http://192.0.2.60:8989","apiKey":"k"}', '', 0),
				(2, 'on', 'Sonarr', '{"baseUrl":"http://192.0.2.61:8989","apiKey":"k"}', '', 1)`,
		)...)
		writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
		in := input(t, dir, "sonarr", sonarrPlan)
		rep, err := (rewriter{kind: kindSonarr}).Check(ctx, in)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(rep.Left) != 1 || rep.Left[0].What != "192.0.2.60:8989" {
			t.Errorf("Left = %+v, want the list that is switched off", rep.Left)
		}
		if want := []string{"sonarr sonarr.db ImportLists.Settings.baseUrl: 192.0.2.61:8989"}; !reflect.DeepEqual(addressStrings(rep.Unclassified), want) {
			t.Errorf("unclassified = %v, want %v", addressStrings(rep.Unclassified), want)
		}
	})
	t.Run("prowlarr application", func(t *testing.T) {
		dir := t.TempDir()
		newDB(t, filepath.Join(dir, "prowlarr.db"), append(prowlarrSchema(),
			`INSERT INTO Applications (Id, Name, Implementation, Settings, SyncLevel) VALUES
				(1, 'off', 'Sonarr', '{"baseUrl":"http://192.0.2.60:8989","prowlarrUrl":"http://192.0.2.60:9696"}', 0),
				(2, 'on', 'Sonarr', '{"baseUrl":"http://192.0.2.61:8989","prowlarrUrl":"http://192.0.2.61:9696"}', 1)`,
		)...)
		writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>9696</Port>\n</Config>")
		in := input(t, dir, "prowlarr", prowlarrPlan)
		in.Targets["prowlarr"] = rewrite.Endpoint{Host: "prowlarr", Port: 9696, URLBase: "/prowlarr"}
		rep, err := (rewriter{kind: kindProwlarr}).Check(ctx, in)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(rep.Left) != 2 {
			t.Errorf("Left = %+v, want the application's two URLs", rep.Left)
		}
		for _, a := range rep.Unclassified {
			if a.Host != "192.0.2.61" {
				t.Errorf("unclassified = %v, want only the application that syncs", addressStrings(rep.Unclassified))
			}
		}
	})
}

// TestClientTypeBoxDoesNotRunExistsIsLeft: box runs qBittorrent and nothing
// else. A client of another type is left exactly as it is and called out,
// because a rewrite would point it at an app that does not speak its protocol.
func TestClientTypeBoxDoesNotRunExistsIsLeft(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO DownloadClients (Id, Name, Enable, Implementation, Settings) VALUES
			(1, 'deluge', 1, 'Deluge', '{"host":"192.0.2.77","port":8112,"useSsl":false,"downloadDirectory":"/srv/remote/downloads"}'),
			(2, 'rtorrent', 0, 'RTorrent', '{"host":"192.0.2.78","port":443,"useSsl":true,"tvDirectory":"/srv/remote/tv"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(ctx, in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("a client type box does not run blocked the run: %v", addressStrings(rep.Unclassified))
	}
	var got []string
	for _, l := range rep.Left {
		got = append(got, l.What)
	}
	if want := []string{"192.0.2.77:8112", "192.0.2.78:443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Left = %v, want %v", got, want)
	}
	if len(rep.Owned) != 0 {
		t.Errorf("owned = %v, want none: box does not adopt a client it does not run", rep.Owned)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(ctx, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	db := filepath.Join(dir, "sonarr.db")
	for id, host := range map[int]string{1: "192.0.2.77", 2: "192.0.2.78"} {
		got := setting(t, db, fmt.Sprintf("SELECT Settings FROM DownloadClients WHERE Id = %d", id))
		if got["host"] != host {
			t.Errorf("client %d was rewritten: %v", id, got)
		}
	}
}

func TestRemoteMappingUsesEachSideItsOwnTable(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES
			(1, 'qBit', 'QBittorrent', '{"host":"localhost","port":8080,"tvCategory":"TV"}')`,
		// qBittorrent's /downloads/tv is the Arr's /library/Downloads/tv, and
		// after migration the two sit under different box directories only
		// because the test plan says so.
		`INSERT INTO RemotePathMappings (Id, Host, RemotePath, LocalPath) VALUES (1, 'localhost', '/downloads/tv', '/library/Downloads/tv')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	// This plan maps the client's downloads somewhere the Arr does not see, so
	// the mapping survives and both sides are rewritten from different tables.
	doc := strings.Replace(sonarrPlan, "      - from: /downloads\n        to: /data/downloads",
		"      - from: /downloads\n        to: /data/torrents", 1)
	in := input(t, dir, "sonarr", doc)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rows := query(t, filepath.Join(dir, "sonarr.db"), "SELECT RemotePath, LocalPath FROM RemotePathMappings")
	if len(rows) != 1 || rows[0][0] != "/data/torrents/tv" || rows[0][1] != "/data/downloads/tv" {
		t.Errorf("mapping = %v, want the client's table on the remote side and the Arr's on the local", rows)
	}
}

func TestRadarrTables(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "radarr.db"), append(radarrSchema(),
		`INSERT INTO Movies (Id, Path) VALUES (1, '/library/media/movies/Blue Ferry')`,
		`INSERT INTO Collections (Id, RootFolderPath) VALUES (1, '/library/media/movies')`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/media/movies')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>7878</Port>\n</Config>")
	in := input(t, dir, "radarr", strings.ReplaceAll(sonarrPlan, "sonarr", "radarr"))
	in.Targets = map[string]rewrite.Endpoint{"radarr": {Host: "radarr", Port: 7878, URLBase: "/radarr"}}

	rep, err := (rewriter{kind: kindRadarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	if _, err := (rewriter{kind: kindRadarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	db := filepath.Join(dir, "radarr.db")
	if got := one(t, db, "SELECT Path FROM Movies"); !reflect.DeepEqual(got, []string{"/data/media/movies/Blue Ferry"}) {
		t.Errorf("Movies.Path = %v", got)
	}
	if got := one(t, db, "SELECT RootFolderPath FROM Collections"); !reflect.DeepEqual(got, []string{"/data/media/movies"}) {
		t.Errorf("Collections.RootFolderPath = %v", got)
	}
}

// prowlarrPlan is the target-side shape §6.2 describes: Prowlarr holds an
// application to a Sonarr and a download client, and no library.
const prowlarrPlan = `version: 2
apps:
  prowlarr:
    kind: kubernetes
    source: prowlarr
    config: /srv/apps/prowlarr
    namespace: media
    ports: [9696]
    paths:
      - from: /library/backups
        to: /config/backups
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
`

// lidarrPlan is the target-side shape for the third Arr: music under
// /library/media/music, downloads in the same /library/Downloads tree.
const lidarrPlan = `version: 2
apps:
  lidarr:
    kind: kubernetes
    source: lidarr
    config: /srv/apps/lidarr
    namespace: media
    ports: [8686]
    paths:
      - from: /library/media
        to: /data/media
      - from: /library/Downloads
        to: /data/downloads
      - from: /library/scripts
        to: /config/scripts
      - from: /data/media
        to: /data/media
      - from: /data/downloads
        to: /data/downloads
      - from: /config/scripts
        to: /config/scripts
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
`

// TestLidarrTables: Lidarr keeps one absolute path on every row the app reads
// back to act on a file. Artists.Path is its library folder, RootFolders.Path
// the root, ImportLists.RootFolderPath where a list adds artists, and
// TrackFiles.Path the file itself -- a real column in Lidarr, unlike Sonarr's
// EpisodeFiles and Radarr's MovieFiles, because migration 30 dropped
// RelativePath and stores the full path instead
// (Datastore/Migration/030_add_mediafilerepository_mtime.cs at v3.1.2.4902).
func TestLidarrTables(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "lidarr.db"), append(lidarrSchema(),
		`INSERT INTO Artists (Id, Path) VALUES (1, '/library/media/music/Harbour Lights')`,
		`INSERT INTO Artists (Id, Path) VALUES (2, '/library/media/music/The Long Way')`,
		`INSERT INTO Albums (Id, Title) VALUES (1, 'First Light')`,
		`INSERT INTO TrackFiles (Id, Path, AlbumId) VALUES
			(1, '/library/media/music/Harbour Lights/First Light/01 - Breakwater.flac', 1),
			(2, '/library/media/music/The Long Way/Open Road/01 - Mile Zero.flac', 1)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/media/music')`,
		`INSERT INTO ImportLists (Id, Name, Implementation, Settings, RootFolderPath) VALUES
			(1, 'Another Lidarr', 'Lidarr', '{"baseUrl":"http://localhost:8686","apiKey":"k","rootFolderPaths":["/library/media/music"]}', '/library/media/music')`,
		`INSERT INTO DownloadClients (Id, Name, Enable, Implementation, Settings) VALUES
			(1, 'qBit', 1, 'QBittorrent', '{"host":"localhost","port":8080,"useSsl":true,"musicCategory":"lidarr"}')`,
		`INSERT INTO Config (Id, Key, Value) VALUES (1, 'scriptimportpath', '/library/scripts/import.sh')`,
		`INSERT INTO History (Id, SourceTitle, Data) VALUES
			(1, '/library/media/music/Harbour Lights/First Light/01 - Breakwater.flac',
			 '{"droppedPath":"/library/Downloads/lidarr/First.Light","importedPath":"/library/media/music/Harbour Lights/First Light/01 - Breakwater.flac"}')`,
		`INSERT INTO DownloadHistory (Id, SourceTitle, Data) VALUES
			(1, '/library/Downloads/lidarr/First.Light', '{"sourcePath":"/library/Downloads/lidarr/First.Light"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8686</Port>\n</Config>")
	in := input(t, dir, "lidarr", lidarrPlan)
	in.Targets = map[string]rewrite.Endpoint{
		"lidarr":      {Host: "lidarr", Port: 8686, URLBase: "/lidarr"},
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
	}

	rep, err := (rewriter{kind: kindLidarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	assertChanges(t, rep.Changes, map[string]int{
		"lidarr.db Artists.Path":                    2,
		"lidarr.db TrackFiles.Path":                 2,
		"lidarr.db RootFolders.Path":                1,
		"lidarr.db ImportLists.RootFolderPath":      1,
		"lidarr.db DownloadClients.Settings":        1,
		"lidarr.db Config.scriptimportpath":         1,
		"lidarr.db History.Data.droppedPath":        1,
		"lidarr.db History.Data.importedPath":       1,
		"lidarr.db DownloadHistory.Data.sourcePath": 1,
		"lidarr.db DownloadHistory.SourceTitle":     1,
		"lidarr.db History.SourceTitle":             1,
		"lidarr.db ImportLists.Settings":            1,
		"config.xml BindAddress":                    1,
		"config.xml EnableSsl":                      1,
		"config.xml UrlBase":                        1,
	})
	if _, err := (rewriter{kind: kindLidarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	db := filepath.Join(dir, "lidarr.db")
	if got := one(t, db, "SELECT Path FROM Artists ORDER BY Id"); !reflect.DeepEqual(got,
		[]string{"/data/media/music/Harbour Lights", "/data/media/music/The Long Way"}) {
		t.Errorf("Artists.Path = %v", got)
	}
	if got := one(t, db, "SELECT Path FROM TrackFiles ORDER BY Id"); !reflect.DeepEqual(got,
		[]string{"/data/media/music/Harbour Lights/First Light/01 - Breakwater.flac",
			"/data/media/music/The Long Way/Open Road/01 - Mile Zero.flac"}) {
		t.Errorf("TrackFiles.Path = %v", got)
	}
	if got := one(t, db, "SELECT Path FROM RootFolders"); !reflect.DeepEqual(got, []string{"/data/media/music"}) {
		t.Errorf("RootFolders.Path = %v", got)
	}
	if got := one(t, db, "SELECT RootFolderPath FROM ImportLists"); !reflect.DeepEqual(got, []string{"/data/media/music"}) {
		t.Errorf("ImportLists.RootFolderPath = %v", got)
	}
	// The client's own category is not a path and is left alone.
	settings := setting(t, db, "SELECT Settings FROM DownloadClients WHERE Id = 1")
	if settings["musicCategory"] != "lidarr" {
		t.Errorf("musicCategory = %v, want it left as it is", settings["musicCategory"])
	}
}

func TestProwlarrApplications(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "prowlarr.db"), append(prowlarrSchema(),
		`INSERT INTO Applications (Id, Name, Implementation, Settings) VALUES
			(1, 'Sonarr', 'Sonarr', '{"prowlarrUrl":"http://localhost:9696","baseUrl":"http://localhost:8989","apiKey":"k","syncCategories":[5000]}')`,
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES
			(1, 'qBit', 'QBittorrent', '{"host":"localhost","port":8080,"useSsl":false,"tvCategory":"TV"}')`,
		`INSERT INTO Config (Id, Key, Value) VALUES (1, 'backupfolder', '/library/backups')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"),
		"<Config>\n  <Port>9696</Port>\n  <SslCertPath></SslCertPath>\n  <UpdateScriptPath></UpdateScriptPath>\n</Config>")
	in := input(t, dir, "prowlarr", prowlarrPlan)
	in.Targets = map[string]rewrite.Endpoint{
		"prowlarr":    {Host: "prowlarr", Port: 9696, URLBase: "/prowlarr"},
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
	}

	rep, err := (rewriter{kind: kindProwlarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v %v", rep.Uncovered, rep.Unclassified)
	}
	wantOwned := []rewrite.Owned{
		{Kind: "application", ID: 1, Name: "Sonarr", Target: "sonarr"},
		{Kind: "downloadclient", ID: 1, Name: "qBit", Target: "qbittorrent"},
	}
	if !reflect.DeepEqual(rep.Owned, wantOwned) {
		t.Errorf("owned = %v, want %v", rep.Owned, wantOwned)
	}
	if _, err := (rewriter{kind: kindProwlarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	db := filepath.Join(dir, "prowlarr.db")
	settings := setting(t, db, "SELECT Settings FROM Applications WHERE Id = 1")
	if settings["baseUrl"] != "http://sonarr:8989/sonarr" {
		t.Errorf("baseUrl = %v", settings["baseUrl"])
	}
	if settings["prowlarrUrl"] != "http://prowlarr:9696/prowlarr" {
		t.Errorf("prowlarrUrl = %v", settings["prowlarrUrl"])
	}
	if got := one(t, db, "SELECT Value FROM Config"); !reflect.DeepEqual(got, []string{"/config/backups"}) {
		t.Errorf("backupfolder = %v", got)
	}
	// Prowlarr's config file has no SslKeyPath, and this pass must not invent
	// one.
	if conf := readFile(t, filepath.Join(dir, "config.xml")); strings.Contains(conf, "SslKeyPath") {
		t.Errorf("config.xml gained a key Prowlarr does not have:\n%s", conf)
	}
}

func TestConfigXMLAddsMissingElements(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), sonarrSchema()...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <LogLevel>info</LogLevel>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	if _, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	conf := readFile(t, filepath.Join(dir, "config.xml"))
	for _, want := range []string{"<BindAddress>*</BindAddress>", "<Port>8989</Port>", "<UrlBase>/sonarr</UrlBase>"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config.xml does not contain %q:\n%s", want, conf)
		}
	}
	if !strings.Contains(conf, "</Config>") {
		t.Errorf("config.xml lost its root:\n%s", conf)
	}
}

func TestConfigXMLTurnsTLSOff(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), sonarrSchema()...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <EnableSsl>True</EnableSsl>\n  <SslPort>9898</SslPort>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	conf := readFile(t, filepath.Join(dir, "config.xml"))
	if !strings.Contains(conf, "<EnableSsl>False</EnableSsl>") || strings.Contains(conf, "<EnableSsl>True</EnableSsl>") {
		t.Errorf("EnableSsl was not turned off:\n%s", conf)
	}
	if !strings.Contains(conf, "<SslPort>9898</SslPort>") {
		t.Errorf("SslPort changed; only EnableSsl should:\n%s", conf)
	}
	found := false
	for _, c := range rep.Changes {
		if c.File == "config.xml" && strings.Contains(c.Where, "EnableSsl") {
			found = true
		}
	}
	if !found {
		t.Errorf("the report does not name the EnableSsl change: %+v", rep.Changes)
	}
}

func TestTornDatabaseRefused(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sonarr.db"), "this is not a database")
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	_, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "sonarr.db") {
		t.Fatalf("Check error = %v, want it to name the database", err)
	}
}

func TestUnknownDownloadClientNoted(t *testing.T) {
	dir := t.TempDir()
	newDB(t, filepath.Join(dir, "sonarr.db"), append(sonarrSchema(),
		`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES (1, 'odd', 'SomeNewClient', '{"savePath":"/library/Downloads"}')`,
	)...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "SomeNewClient") {
		t.Errorf("notes = %v, want one naming the unknown client", rep.Notes)
	}
}

// assertChanges compares the report against file + place -> count.
func assertChanges(t *testing.T, changes []rewrite.Change, want map[string]int) {
	t.Helper()
	got := make(map[string]int, len(changes))
	for _, c := range changes {
		got[c.File+" "+c.Where] += c.Count
	}
	for key, n := range want {
		if got[key] != n {
			t.Errorf("changes[%q] = %d, want %d (all: %s)", key, got[key], n, formatChanges(changes))
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected change %q in %s", key, formatChanges(changes))
		}
	}
}

func formatChanges(changes []rewrite.Change) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.File+" "+c.Where+" "+c.What+" "+strconv.Itoa(c.Count))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func addressStrings(addrs []rewrite.Address) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestEveryDownloadClientPathField writes one client per implementation the
// table names, each path field under the source's downloads root, and proves
// the pass rewrites every one of them.
func TestEveryDownloadClientPathField(t *testing.T) {
	dir := t.TempDir()
	statements := sonarrSchema()
	ids := make([]int64, 0, len(downloadPaths))
	want := make(map[int64][]string)
	id := int64(0)
	for implementation, keys := range downloadPaths {
		id++
		ids = append(ids, id)
		settings := make(map[string]any, len(keys))
		for _, key := range keys {
			settings[key] = "/library/Downloads/" + key
			want[id] = append(want[id], key)
		}
		blob, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		statements = append(statements, fmt.Sprintf(
			`INSERT INTO DownloadClients (Id, Name, Implementation, Settings) VALUES (%d, '%s', '%s', '%s')`,
			id, implementation, implementation, string(blob)))
	}
	newDB(t, filepath.Join(dir, "sonarr.db"), statements...)
	writeFile(t, filepath.Join(dir, "config.xml"), "<Config>\n  <Port>8989</Port>\n</Config>")
	in := input(t, dir, "sonarr", sonarrPlan)

	rep, err := (rewriter{kind: kindSonarr}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	db := filepath.Join(dir, "sonarr.db")
	for _, id := range ids {
		settings := setting(t, db, "SELECT Settings FROM DownloadClients WHERE Id = "+strconv.FormatInt(id, 10))
		for _, key := range want[id] {
			if settings[key] != "/data/downloads/"+key {
				t.Errorf("client %d %s = %v, want the box's path", id, key, settings[key])
			}
		}
	}
}

// TestSonarrFollowsTheLibrarysCategory: a migrated library is renamed onto box's
// own category, so the root folder the source had becomes /data/media/<category>
// and every series under it moves with it. That is what makes wiring's root
// folder step find the folder it expects instead of adding a second one.
func TestSonarrFollowsTheLibrarysCategory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.xml"), sonarrConfig())
	newDB(t, filepath.Join(dir, "sonarr.db"),
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO Series (Id, Path) VALUES (1, '/library/TV/Night Shift')`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/library/TV')`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, RootFolderPath TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, Data TEXT, SourceTitle TEXT)`,
	)
	// The plan discovery writes once the library has been mapped onto the
	// category the instance has: a rule for the directory itself, more
	// specific than the library's, and one for everything else under it.
	const mapped = `version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    category: tv
    paths:
      - from: /library/TV
        to: /data/media/tv
      - from: /library
        to: /data/media
`
	in := input(t, dir, "sonarr", mapped)
	if _, err := (rewriter{kind: kindSonarr}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := query(t, filepath.Join(dir, "sonarr.db"), `SELECT Path FROM RootFolders`)[0][0]; got != "/data/media/tv" {
		t.Errorf("root folder = %q, want /data/media/tv: wiring adds its own when the migrated one is not there", got)
	}
	if got := query(t, filepath.Join(dir, "sonarr.db"), `SELECT Path FROM Series`)[0][0]; got != "/data/media/tv/Night Shift" {
		t.Errorf("series path = %q, want /data/media/tv/Night Shift", got)
	}
}
