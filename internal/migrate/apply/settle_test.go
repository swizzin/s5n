package apply

// settle_test.go proves the read-only pass that runs before an install: the
// rewriters' checks on a copy, the plan that carries every unsettled value as a
// default, and the list of what the operator is told.
//
// The fixture is one small source install of the shape the first real run met:
// two Arrs with a disabled client of a type box does not run, autobrr pointing
// its Arrs at a whole URL, Seerr with a root folder in its database, Maintainerr
// on a Plex address nothing can place, and PeerBanHelper on the public name of
// the very qBittorrent being migrated. Every address is from the documentation
// ranges and every name is invented; the databases are built row by row, so what
// a rewriter reads is this test's own SQL.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// rehearsal is the source install the tests below settle: where it lives, and
// the plan discovery would have written for it.
type rehearsal struct {
	t   *testing.T
	dir string
	// configRoot and dataRoot are the seedbox's two host paths, for the apply
	// half of the tests.
	configRoot, dataRoot string
	doc                  string
}

// newRehearsal builds the source tree and the plan that describes it.
func newRehearsal(t *testing.T, hosts string, plex bool) *rehearsal {
	t.Helper()
	r := &rehearsal{
		t:          t,
		dir:        t.TempDir(),
		configRoot: "",
		dataRoot:   "",
	}
	r.configRoot = filepath.Join(r.dir, "seedbox", "config")
	r.dataRoot = filepath.Join(r.dir, "seedbox", "data")
	for _, d := range []string{r.configRoot, filepath.Join(r.dataRoot, "downloads"), filepath.Join(r.dataRoot, "media")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.writeSonarr()
	r.writeRadarr()
	r.writeQbittorrent()
	r.writeAutobrr()
	r.writeSeerr()
	r.writeMaintainerr()
	r.writePbh()
	if plex {
		// Plex's home is the settings file it rewrites on every upgrade; this
		// one is a fresh install's, with no library database beside it.
		writeFile(r.t, filepath.Join(r.path("plex"), "Preferences.xml"),
			"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<Preferences MachineIdentifier=\"synthetic-machine\" FriendlyName=\"seedbox\"/>\n", 0o600)
	}
	r.doc = r.planDoc(hosts, plex)
	return r
}

// path is one app's config directory on the source host.
func (r *rehearsal) path(app string) string {
	return filepath.Join(r.dir, "src", app)
}

// planDoc is the plan discovery would have written: the Arrs' library and the
// shared downloads under one root, the apps' ports, and the Service rules it
// resolved. hosts is the whole hosts block, so a test can add to it or replace
// it.
func (r *rehearsal) planDoc(hosts string, plex bool) string {
	apps := []string{
		`  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /src/qbittorrent
    ports: [8081]
    paths:
      - from: /downloads
        to: /data/downloads`,
		`  sonarr:
    kind: kubernetes
    source: sonarr
    config: /src/sonarr
    ports: [8989]
    paths:
      - from: /media
        to: /data/media
      - from: /downloads
        to: /data/downloads`,
		`  radarr:
    kind: kubernetes
    source: radarr
    config: /src/radarr
    ports: [7878]
    paths:
      - from: /media
        to: /data/media
      - from: /downloads
        to: /data/downloads`,
		`  autobrr:
    kind: kubernetes
    source: autobrr
    config: /src/autobrr
    ports: [7474]
    paths:
      - from: /downloads
        to: /data/downloads`,
		`  seerr:
    kind: kubernetes
    source: seerr
    config: /src/seerr
    ports: [5055]
    paths: []`,
		`  maintainerr:
    kind: kubernetes
    source: maintainerr
    config: /src/maintainerr
    ports: [6246]
    paths: []`,
		`  pbh:
    kind: kubernetes
    source: pbh
    config: /src/pbh
    ports: [9898]
    paths: []`,
	}
	if plex {
		apps = append(apps, `  plex:
    kind: kubernetes
    source: plex
    config: /src/plex
    ports: [32400]
    paths:
      - from: /media
        to: /data/media`)
	}
	return "version: 2\ndata:\n  root: " + r.dataRoot + "\n  downloads: downloads\n  media: media\napps:\n" +
		strings.Join(apps, "\n") + "\nhosts:\n" + hosts
}

// plan parses the fixture's plan.
func (r *rehearsal) plan() *plan.Plan {
	r.t.Helper()
	p, err := plan.Parse([]byte(r.doc))
	if err != nil {
		r.t.Fatalf("parse the rehearsal plan: %v", err)
	}
	return p
}

// settleInput is what a caller with no image runner has: the catalog, the
// source paths, and the chart's endpoints.
func (r *rehearsal) settleInput() SettleInput {
	return SettleInput{
		Catalog: rehearsalCatalog,
		Configs: func(app string) string { return r.path(app) },
		Targets: rehearsalTargets(),
	}
}

// snapshotConfigs is a snapshot's config/ tree: one directory per planned app,
// copied from the source the way a snapshot copies it. It is the only place
// apply reads an app's config from (Env.Configs), so a test that runs apply over
// a plan settles the source and applies the snapshot.
func (r *rehearsal) snapshotConfigs() string {
	r.t.Helper()
	dir := filepath.Join(r.dir, "snapshot", "config")
	for app := range r.plan().Apps {
		if _, err := copyTree(filepath.Join(dir, app), r.path(app), os.Geteuid(), os.Getegid()); err != nil {
			r.t.Fatalf("copy %s into the snapshot: %v", app, err)
		}
	}
	return dir
}

// rehearsalCatalog is the chart's apps the fixture uses.
var rehearsalCatalog = []string{
	"qbittorrent", "sonarr", "radarr", "prowlarr", "plex", "tautulli",
	"seerr", "qui", "autobrr", "pbh", "maintainerr",
}

// rehearsalTargets is where the release serves each instance.
func rehearsalTargets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"radarr":      {Host: "radarr", Port: 7878, URLBase: "/radarr"},
		"autobrr":     {Host: "autobrr", Port: 7474, URLBase: "/autobrr/"},
		"seerr":       {Host: "seerr", Port: 5055},
		"maintainerr": {Host: "maintainerr", Port: 6246, URLBase: "/maintainerr"},
		"pbh":         {Host: "pbh", Port: 9898},
		"plex":        {Host: "plex", Port: 32400},
	}
}

// The two Arrs: their own library and the shared downloads, a qBittorrent
// client that resolves, and — the case the first real run met — a client of a
// type box does not run, with its address on the docker bridge.
const arrClientStatements = `
INSERT INTO DownloadClients (Id, Name, Enable, Implementation, Settings) VALUES
 (1, 'qBittorrent', 1, 'QBittorrent', '{"host":"qbittorrent.media.svc","port":8081,"useSsl":false,"urlBase":"","tvCategory":"tv"}'),
 (2, 'rTorrent', 0, 'RTorrent', '{"host":"172.17.0.1","port":443,"useSsl":true,"tvDirectory":"/downloads/tv"}'),
 (3, 'Deluge', 0, 'Deluge', '{"host":"172.17.0.1","port":10365,"useSsl":false,"downloadDirectory":"/downloads"}');
`

func (r *rehearsal) writeSonarr() {
	dir := r.path("sonarr")
	writeFile(r.t, filepath.Join(dir, "config.xml"), sonarrConfigXML, 0o600)
	newDB(r.t, filepath.Join(dir, "sonarr.db"),
		`CREATE TABLE Series (Id INTEGER PRIMARY KEY, Title TEXT, Path TEXT)`,
		`INSERT INTO Series (Id, Title, Path) VALUES (1, 'Night Shift', '/media/tv/Night Shift')`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/media/tv')`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Enable INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		arrClientStatements,
	)
}

func (r *rehearsal) writeRadarr() {
	dir := r.path("radarr")
	writeFile(r.t, filepath.Join(dir, "config.xml"),
		strings.Replace(sonarrConfigXML, "<Port>8989</Port>", "<Port>7878</Port>", 1), 0o600)
	newDB(r.t, filepath.Join(dir, "radarr.db"),
		`CREATE TABLE Movies (Id INTEGER PRIMARY KEY, Title TEXT, Path TEXT)`,
		`INSERT INTO Movies (Id, Title, Path) VALUES (1, 'Blue Ferry', '/media/movies/Blue Ferry')`,
		`CREATE TABLE Collections (Id INTEGER PRIMARY KEY, Title TEXT, RootFolderPath TEXT)`,
		`INSERT INTO Collections (Id, Title, RootFolderPath) VALUES (1, 'A Trilogy', '/media/movies')`,
		`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT)`,
		`INSERT INTO RootFolders (Id, Path) VALUES (1, '/media/movies')`,
		`CREATE TABLE ImportLists (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT, RootFolderPath TEXT)`,
		`CREATE TABLE RemotePathMappings (Id INTEGER PRIMARY KEY, Host TEXT, RemotePath TEXT, LocalPath TEXT)`,
		`CREATE TABLE DownloadClients (Id INTEGER PRIMARY KEY, Name TEXT, Enable INTEGER, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Notifications (Id INTEGER PRIMARY KEY, Name TEXT, Implementation TEXT, Settings TEXT)`,
		`CREATE TABLE Config (Id INTEGER PRIMARY KEY, Key TEXT, Value TEXT)`,
		`CREATE TABLE Commands (Id INTEGER PRIMARY KEY, Name TEXT, Status INTEGER, Body TEXT)`,
		`CREATE TABLE History (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		`CREATE TABLE DownloadHistory (Id INTEGER PRIMARY KEY, SourceTitle TEXT, Data TEXT)`,
		arrClientStatements,
	)
}

func (r *rehearsal) writeQbittorrent() {
	dir := r.path("qbittorrent")
	writeFile(r.t, filepath.Join(dir, "qBittorrent", "qBittorrent.conf"), qbitConf, 0o600)
	newDB(r.t, filepath.Join(dir, "qBittorrent", "torrents.db"),
		`CREATE TABLE torrents (id INTEGER PRIMARY KEY, target_save_path TEXT, download_path TEXT, libtorrent_resume_data BLOB)`,
		`INSERT INTO torrents (id, target_save_path, download_path, libtorrent_resume_data) VALUES (1, '/downloads/tv', '', NULL)`,
	)
}

// writeAutobrr is autobrr's config.toml and its client table: the Arrs as the
// whole URL autobrr stores for them, its qBittorrent, and a client it has
// switched off.
func (r *rehearsal) writeAutobrr() {
	dir := r.path("autobrr")
	writeFile(r.t, filepath.Join(dir, "config.toml"), `# config.toml
host = "127.0.0.1"
port = 7474
baseUrlModeLegacy = true
sessionSecret = "synthetic-secret"
`, 0o600)
	newDB(r.t, filepath.Join(dir, "autobrr.db"),
		`CREATE TABLE client (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    enabled BOOLEAN,
    type TEXT,
    host TEXT NOT NULL,
    port INTEGER,
    tls BOOLEAN,
    tls_skip_verify BOOLEAN,
    username TEXT,
    password TEXT,
    settings JSON
)`,
		`CREATE TABLE action (
    id INTEGER PRIMARY KEY, name TEXT, type TEXT, enabled BOOLEAN,
    exec_cmd TEXT, exec_args TEXT, watch_folder TEXT, category TEXT, tags TEXT,
    label TEXT, save_path TEXT, download_path TEXT, paused BOOLEAN,
    ignore_rules BOOLEAN, limit_upload_speed INT, limit_download_speed INT,
    client_id INTEGER, filter_id INTEGER
)`,
		`INSERT INTO client (id, name, enabled, type, host, port, tls) VALUES
 (1, 'qBittorrent', 1, 'QBITTORRENT', 'localhost', 8081, 0),
 (2, 'Sonarr', 1, 'SONARR', 'http://sonarr:8989/sonarr', 10000, 0),
 (3, 'Radarr', 1, 'RADARR', 'http://radarr:7878/radarr', 10000, 0),
 (4, 'old nas', 0, 'QBITTORRENT', 'http://192.0.2.10:8081', 8081, 1)`,
		`INSERT INTO action (id, name, type, enabled, save_path, download_path) VALUES
 (1, 'Save', 'QBITTORRENT', 1, '/downloads/{{ .Category }}', '/downloads')`,
	)
}

// writeSeerr is Seerr's settings and its database: one override rule that names
// both Arrs, and one completed request sent to Radarr. The folders are in the
// Arrs' trees, not Seerr's, which is the whole point of the rule that moves them.
func (r *rehearsal) writeSeerr() {
	dir := r.path("seerr")
	writeFile(r.t, filepath.Join(dir, "settings.json"), `{
  "clientId": "synthetic-client-id",
  "main": {
    "apiKey": "synthetic-api-key",
    "applicationUrl": "https://requests.example.com"
  },
  "plex": {
    "name": "seedbox",
    "ip": "plex.media.svc",
    "port": 32400,
    "useSsl": false
  },
  "radarr": [
    {
      "id": 0,
      "name": "Radarr",
      "hostname": "radarr.media.svc",
      "port": 7878,
      "apiKey": "synthetic-radarr-key",
      "useSsl": false,
      "baseUrl": "",
      "activeDirectory": "/media/movies"
    }
  ],
  "sonarr": [
    {
      "id": 1,
      "name": "Sonarr",
      "hostname": "sonarr.media.svc",
      "port": 8989,
      "apiKey": "synthetic-sonarr-key",
      "useSsl": false,
      "baseUrl": "",
      "activeDirectory": "/media/tv",
      "activeAnimeDirectory": "/media/anime"
    }
  ]
}
`, 0o600)
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	newDB(r.t, filepath.Join(dir, "db", "db.sqlite3"),
		`CREATE TABLE override_rule (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    radarrServiceId integer,
    sonarrServiceId integer,
    rootFolder varchar
)`,
		`CREATE TABLE media_request (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type varchar,
    serverId integer,
    rootFolder varchar
)`,
		`INSERT INTO override_rule (id, radarrServiceId, sonarrServiceId, rootFolder) VALUES (1, 0, 1, '/media/movies')`,
		`INSERT INTO media_request (id, type, serverId, rootFolder) VALUES (1, 'movie', 0, '/media/movies')`,
	)
}

// writeMaintainerr is Maintainerr's settings row, pointing its Plex connection
// at a pod address: nothing in the plan names it, and it does not survive the
// move.
func (r *rehearsal) writeMaintainerr() {
	if err := os.MkdirAll(r.path("maintainerr"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	newDB(r.t, filepath.Join(r.path("maintainerr"), "maintainerr.sqlite"),
		`CREATE TABLE settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    applicationUrl varchar NOT NULL DEFAULT 'localhost',
    plex_name varchar,
    plex_hostname varchar,
    plex_port integer DEFAULT 32400,
    plex_ssl integer,
    plex_auth_token varchar,
    seerr_url varchar,
    tautulli_url varchar,
    download_client_url varchar
)`,
		`CREATE TABLE radarr_settings (id INTEGER PRIMARY KEY AUTOINCREMENT, serverName varchar NOT NULL, url varchar, apiKey varchar)`,
		`CREATE TABLE sonarr_settings (id INTEGER PRIMARY KEY AUTOINCREMENT, serverName varchar NOT NULL, url varchar, apiKey varchar)`,
		`INSERT INTO settings (id, applicationUrl, plex_hostname, plex_port, plex_ssl,
    seerr_url, tautulli_url, download_client_url)
 VALUES (1, 'https://maintainerr.example.com', '10.244.0.56', 32400, 1,
    'http://seerr.media.svc:5055', '', 'http://qbittorrent.media.svc:8081')`,
		`INSERT INTO radarr_settings (serverName, url, apiKey) VALUES ('Radarr', 'http://radarr.media.svc:7878/radarr', 'k')`,
		`INSERT INTO sonarr_settings (serverName, url, apiKey) VALUES ('Sonarr', 'http://sonarr.media.svc:8989/sonarr', 'k')`,
	)
}

// writePbh is PeerBanHelper's config.yml, with its downloader reached through
// the public name of the very qBittorrent being migrated.
func (r *rehearsal) writePbh() {
	if err := os.MkdirAll(filepath.Join(r.path("pbh"), "config"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	writeFile(r.t, filepath.Join(r.path("pbh"), "config", "config.yml"), `config-version: 48
language: default
server:
  http: 9898
  address: "127.0.0.1"
  prefix: "http://127.0.0.1:9898"
  token: "source-token"
client:
  qbt_main:
    type: qbittorrent
    name: qBittorrent
    endpoint: https://example.com/qbittorrent
    username: admin
    password: synthetic-password
`, 0o600)
}

// rehearsalHosts are the Service rules discovery resolved for the addresses the
// apps store.
func rehearsalHosts(plex bool) string {
	hosts := `  - service: qbittorrent.media
    app: qbittorrent
  - service: sonarr.media
    app: sonarr
  - service: radarr.media
    app: radarr
  - service: seerr.media
    app: seerr
`
	if plex {
		hosts += "  - service: plex.media\n    app: plex\n"
	}
	return hosts
}

// TestSettleCallsOutEveryUnsettledValue is the case the first real run met,
// settled: nothing blocks, every decision is in the plan with its reason, and the list
// says what box will do with each one.
func TestSettleCallsOutEveryUnsettledValue(t *testing.T) {
	ctx := context.Background()
	r := newRehearsal(t, rehearsalHosts(true), true)
	p := r.plan()
	rules := len(p.Hosts)
	before := hashTree(t, r.dir)

	settled, called, err := Settle(ctx, p, r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// The source is what the operator's install is: a read-only pass leaves it
	// byte for byte as it was.
	sameTree(t, "the source tree", before, hashTree(t, r.dir))

	// Every address and path that was not settled is now a default with a
	// reason, in the plan the caller hands on to apply.
	want := []plan.HostRule{
		{Host: "10.244.0.56", Port: 32400, App: "plex", Default: true,
			Why: "rewritten to plex: it is Maintainerr's Plex connection, and this box runs Plex"},
		{Host: "example.com", Port: 443, App: "qbittorrent", Default: true,
			Why: "rewritten to qbittorrent: it is a qBittorrent downloader, and this box runs one qBittorrent"},
	}
	if got := settled.Hosts[len(settled.Hosts)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Errorf("the rules Settle added = %+v, want %+v", got, want)
	}
	// The plan the caller gave is untouched.
	if len(p.Hosts) != rules {
		t.Errorf("Settle added %d rules to the caller's own plan", len(p.Hosts)-rules)
	}

	// The called-out list is what report.txt carries, grouped by decision.
	wantLines := []string{
		"checked at apply:",
		"  plex (its database is only readable through its own image, and this run has no way to start one)",
		"left as it is:",
		"  172.17.0.1:10365 (\"Deluge\" is a Deluge client and this box runs only qBittorrent, so a rewrite would point it at an app that does not speak its protocol; delete it in the Arr if you do not need it)",
		"    radarr: radarr.db DownloadClients.Settings.host",
		"    sonarr: sonarr.db DownloadClients.Settings.host",
		"  172.17.0.1:443 (\"rTorrent\" is a RTorrent client and this box runs only qBittorrent, so a rewrite would point it at an app that does not speak its protocol; delete it in the Arr if you do not need it)",
		"    radarr: radarr.db DownloadClients.Settings.host",
		"    sonarr: sonarr.db DownloadClients.Settings.host",
		"  192.0.2.10:8081 (\"old nas\" is switched off, so 192.0.2.10:8081 is never dialled and nothing is migrated to it)",
		"    autobrr: autobrr.db client.host",
		"rewritten to plex:",
		"  10.244.0.56:32400 (it is Maintainerr's Plex connection, and this box runs Plex)",
		"    maintainerr: maintainerr.sqlite settings.plex_hostname",
		"rewritten to qbittorrent:",
		"  example.com:443 (it is a qBittorrent downloader, and this box runs one qBittorrent)",
		"    pbh: config/config.yml client/qbt_main/endpoint",
	}
	if got := called.Lines(); !reflect.DeepEqual(got, wantLines) {
		t.Errorf("called out:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantLines, "\n"))
	}
	if len(called) != 4 {
		t.Errorf("Settled has %d groups, want left, checked at apply and the two rewrites", len(called))
	}
}

// TestSettledPlanIsAValidPlan: what Settle writes is a plan this box reads back,
// with every default carrying its reason. A rule the plan validation refuses
// would reach the operator as a broken file instead of a decision.
func TestSettledPlanIsAValidPlan(t *testing.T) {
	r := newRehearsal(t, rehearsalHosts(false), false)
	settled, _, err := Settle(context.Background(), r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	raw, err := yaml.Marshal(settled)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Parse(raw); err != nil {
		t.Fatalf("the settled plan does not read back: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), "why: \"\"") {
		t.Errorf("a default without a reason:\n%s", raw)
	}
}

// TestHandWrittenRuleIsNeverReplaced: a rule the operator wrote is the answer,
// so a default is only ever added beside it, and the plan it was written in
// comes back as it was.
func TestHandWrittenRuleIsNeverReplaced(t *testing.T) {
	ctx := context.Background()
	hosts := rehearsalHosts(false) + `  - host: 198.51.100.20
    port: 8081
    app: qbittorrent
  - host: 198.51.100.20
    port: 8082
    app: qbittorrent
`
	r := newRehearsal(t, hosts, false)
	p := r.plan()
	handWritten := append([]plan.HostRule(nil), p.Hosts...)

	settled, called, err := Settle(ctx, p, r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !reflect.DeepEqual(settled.Hosts[:len(handWritten)], handWritten) {
		t.Errorf("the operator's rules changed:\n got %+v\nwant %+v", settled.Hosts[:len(handWritten)], handWritten)
	}
	if !reflect.DeepEqual(p.Hosts, handWritten) {
		t.Error("Settle changed the plan it was given")
	}
	// The plan's own answer still wins: 198.51.100.20:8081 is the box's
	// qBittorrent by the operator's rule, not by anything Settle wrote.
	res := settled.Resolve("radarr", "198.51.100.20", 8081)
	if res.Verdict != plan.Rewrite || res.App != "qbittorrent" {
		t.Errorf("Resolve(198.51.100.20:8081) = %+v, want the operator's rewrite", res)
	}
	for _, line := range called.Lines() {
		if strings.Contains(line, "198.51.100.20") {
			t.Errorf("a rule the operator wrote was called out as a default: %q", line)
		}
	}
}

// TestSettleLeavesAnAmbiguousLocalhostAddressToThePlan: a localhost address is
// settled by the apps' ports and by nothing else, and a rule may not name one
// (the plan validation refuses it). A plan whose ports settle it twice is a plan
// this box cannot apply, so Settle stops and says so rather than writing a rule
// that would never be read.
func TestSettleLeavesAnAmbiguousLocalhostAddressToThePlan(t *testing.T) {
	r := newRehearsal(t, rehearsalHosts(false), false)
	p := r.plan()
	// Two apps claim one port, so in.Resolve cannot say which app a localhost
	// address at it means: autobrr's qBittorrent client is stored as localhost
	// and the port column.
	sonarr := p.Apps["sonarr"]
	sonarr.Ports = append(sonarr.Ports, 8081)
	p.Apps["sonarr"] = sonarr

	_, _, err := Settle(context.Background(), p, r.settleInput())
	if err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("Settle = %v, want it to stop and name the localhost address", err)
	}
}

// TestApplyWritesWhatSettleCalledOut: the settled plan is what apply runs with,
// and it refuses nothing. What each decision says is what the migrated config
// holds: the two rewrites point at the box, and every value left as it is is
// exactly as the source stored it.
func TestApplyWritesWhatSettleCalledOut(t *testing.T) {
	ctx := context.Background()
	r := newRehearsal(t, rehearsalHosts(true), true)
	settled, called, err := Settle(ctx, r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if called.Empty() {
		t.Fatal("the fixture called nothing out, so there is nothing to prove")
	}

	raw, err := yaml.Marshal(settled)
	if err != nil {
		t.Fatal(err)
	}
	var shown Summary
	env := Env{
		ConfigRoot: r.configRoot,
		DataRoot:   r.dataRoot,
		// The configs come out of the snapshot apply runs, never off the
		// source host: the settled plan is applied over a copy of the tree the
		// source keeps, which is what a snapshot carries.
		Configs: r.snapshotConfigs(),
		PUID:    os.Geteuid(),
		PGID:    os.Getegid(),
		Targets: rehearsalTargets(),
		// Plex's own image is the only thing that may read its database, and
		// this run has no way to start one. Its config here holds neither the
		// settings file nor the database, so its check reads nothing.
		Run: func(context.Context, string, []string, []byte) ([]byte, error) {
			return []byte("@end|1\n"), nil
		},
		Enabled: func(string) bool { return false },
		Owned:   &fakeOwned{},
		Secret:  newFakeKeys(),
		Values:  &fakeValues{},
		Confirm: yes(t, &shown),
		Catalog: rehearsalCatalog,
	}
	res, err := Run(ctx, settled, raw, env)
	if err != nil {
		t.Fatalf("apply refused the settled plan: %v", err)
	}
	if len(res.Moved) != len(settled.Apps) {
		t.Errorf("moved %v, want every app in the plan", res.Moved)
	}

	// The two rewrites: Maintainerr's Plex connection and PeerBanHelper's
	// downloader both point at the box now.
	if got := query(t, filepath.Join(r.configRoot, "maintainerr", "maintainerr.sqlite"),
		`SELECT plex_hostname || ':' || plex_port FROM settings WHERE id = 1`); got != "plex:32400" {
		t.Errorf("maintainerr's Plex connection = %q, want plex:32400", got)
	}
	pbh := r.readMoved("pbh", "config/config.yml")
	if !strings.Contains(pbh, "endpoint: http://qbittorrent:8080") {
		t.Errorf("PeerBanHelper's downloader is not the box's qBittorrent:\n%s", pbh)
	}

	// The values left as they are: the Arr clients box does not run keep the
	// address they had, and autobrr's disabled client keeps its own.
	for _, app := range []string{"sonarr", "radarr"} {
		blob := query(t, filepath.Join(r.configRoot, app, app+".db"),
			`SELECT Settings FROM DownloadClients WHERE Name = 'rTorrent'`)
		var f map[string]any
		if err := json.Unmarshal([]byte(blob), &f); err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		if f["host"] != "172.17.0.1" || f["port"] != float64(443) {
			t.Errorf("%s rewrote a client box does not run: %v", app, f)
		}
	}
	if rows := query(t, filepath.Join(r.configRoot, "autobrr", "autobrr.db"),
		`SELECT host FROM client WHERE id = 4`); rows != "http://192.0.2.10:8081" {
		t.Errorf("autobrr's disabled client = %q, want it left as it was", rows)
	}
	// Everything else moved: the Arrs' own client row, autobrr's Arr rows, and
	// Seerr's database folders through the Arrs' tables.
	if got := query(t, filepath.Join(r.configRoot, "autobrr", "autobrr.db"),
		`SELECT host FROM client WHERE id = 2`); got != "http://sonarr:8989/sonarr" {
		t.Errorf("autobrr's Sonarr row = %q", got)
	}
	if got := query(t, filepath.Join(r.configRoot, "seerr", "db", "db.sqlite3"),
		`SELECT rootFolder FROM override_rule WHERE id = 1`); got != "/data/media/movies" {
		t.Errorf("seerr's override rule = %q, want the folder in the Arrs' trees", got)
	}
	if got := query(t, filepath.Join(r.configRoot, "seerr", "db", "db.sqlite3"),
		`SELECT rootFolder FROM media_request WHERE id = 1`); got != "/data/media/movies" {
		t.Errorf("seerr's request = %q", got)
	}
}

// TestSettleLeavesAnUncoveredPathAsItIs: a path no rule covers becomes an
// identity rule — left exactly as it is, with the reason — in the table of the
// app whose filesystem it is on. Two keys holding one directory make one rule,
// because two rules for one From is a plan the validation refuses.
func TestSettleLeavesAnUncoveredPathAsItIs(t *testing.T) {
	ctx := context.Background()
	r := newRehearsal(t, rehearsalHosts(false), false)
	// One directory the plan knows nothing about, held by two of Sonarr's
	// columns.
	exec(t, filepath.Join(r.path("sonarr"), "sonarr.db"),
		`INSERT INTO Series (Id, Title, Path) VALUES (2, 'Elsewhere', '/mnt/other/shows')`,
		`INSERT INTO RootFolders (Id, Path) VALUES (2, '/mnt/other/shows')`)

	settled, called, err := Settle(ctx, r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	var rules []plan.PathRule
	for _, rule := range settled.Apps["sonarr"].Paths {
		if rule.From == "/mnt/other/shows" {
			rules = append(rules, rule)
		}
	}
	want := plan.PathRule{
		From: "/mnt/other/shows", To: "/mnt/other/shows", Default: true,
		Why: "left as it is: box cannot tell what /mnt/other/shows is on this box, so it is left as stored",
	}
	if len(rules) != 1 || rules[0] != want {
		t.Fatalf("sonarr's rules for /mnt/other/shows = %+v, want exactly %+v", rules, want)
	}
	// One line, both places named beneath it.
	var found int
	for _, g := range called {
		for _, it := range g.Items {
			if it.Value != "/mnt/other/shows" {
				continue
			}
			found++
			if g.Action != actionLeave || len(it.Holds) != 2 {
				t.Errorf("the path was called out as %q with %d places", g.Action, len(it.Holds))
			}
		}
	}
	if found != 1 {
		t.Errorf("the path appears in the called-out list %d times, want once", found)
	}

	// The plan it wrote is a plan this box reads, and settling it again finds
	// nothing new: the rules are the answer, not a step on the way to one.
	raw, err := yaml.Marshal(settled)
	if err != nil {
		t.Fatal(err)
	}
	again, err := plan.Parse(raw)
	if err != nil {
		t.Fatalf("the settled plan does not read back: %v\n%s", err, raw)
	}
	twice, calledAgain, err := Settle(ctx, again, r.settleInput())
	if err != nil {
		t.Fatalf("second Settle: %v", err)
	}
	if len(twice.Hosts) != len(again.Hosts) {
		t.Errorf("a second settle added %d host rules", len(twice.Hosts)-len(again.Hosts))
	}
	if len(twice.Apps["sonarr"].Paths) != len(again.Apps["sonarr"].Paths) {
		t.Errorf("a second settle added %d path rules", len(twice.Apps["sonarr"].Paths)-len(again.Apps["sonarr"].Paths))
	}
	for _, line := range calledAgain.Lines() {
		if strings.Contains(line, "/mnt/other/shows") {
			t.Errorf("a second settle still calls out the path it settled: %q", line)
		}
	}
}

// readMoved reads one file of an app's config after apply moved it into place.
func (r *rehearsal) readMoved(app, rel string) string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.configRoot, app, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatalf("read %s/%s: %v", app, rel, err)
	}
	return string(raw)
}
