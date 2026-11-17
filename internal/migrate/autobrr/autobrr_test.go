package autobrr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// configFixture is a config.toml shaped like autobrr's own template
// (internal/config/config.go at v1.90.0): root settings, comments, baseUrl
// commented out and legacy mode on.
const configFixture = `# config.toml
host = "127.0.0.1"
port = 7474

# Set custom baseUrl eg /autobrr/ to serve in subdirectory.
#baseUrl = "/autobrr/"

# If you use baseUrl you can set this to false and skip any url rewrite in your proxy.
baseUrlModeLegacy = true

logLevel = "INFO"
logPath = "/srv/apps/autobrr/logs/autobrr.log"
sessionSecret = "synthetic-secret"

[metrics]
enabled = false
`

// dbFixture is the two tables this package reads, shaped after
// internal/database/migrations/sqlite at v1.90.0: a client row is a download
// client or an Arr, and an action carries the macro-capable paths.
const dbFixture = `
CREATE TABLE client (
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
);
CREATE TABLE action (
    id INTEGER PRIMARY KEY,
    name TEXT,
    type TEXT,
    enabled BOOLEAN,
    exec_cmd TEXT,
    exec_args TEXT,
    watch_folder TEXT,
    category TEXT,
    tags TEXT,
    label TEXT,
    save_path TEXT,
    download_path TEXT,
    paused BOOLEAN,
    ignore_rules BOOLEAN,
    limit_upload_speed INT,
    limit_download_speed INT,
    client_id INTEGER,
    filter_id INTEGER
);
INSERT INTO client (id, name, enabled, type, host, port, tls)
VALUES (1, 'qBittorrent', 1, 'QBITTORRENT', 'localhost', 8081, 1);
INSERT INTO client (id, name, enabled, type, host, port, tls)
VALUES (2, 'Sonarr', 1, 'SONARR', 'sonarr.default.svc', 8989, 1);
INSERT INTO action (id, name, type, exec_cmd, watch_folder, save_path, download_path)
VALUES (1, 'Save', 'QBITTORRENT', '/srv/apps/autobrr/bin/hook', '/srv/apps/autobrr/watch',
        '/srv/downloads/{{ .Category }}', '/srv/downloads');
`

const planYAML = `
version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8081]
    paths: []
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  autobrr:
    kind: kubernetes
    source: autobrr
    config: /srv/apps/autobrr
    paths:
      - from: /srv/apps/autobrr
        to: /config
      - from: /srv/downloads
        to: /data/downloads
hosts:
  - service: sonarr.default
    app: sonarr
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"autobrr":     {Host: "autobrr", Port: 7474, URLBase: "/autobrr"},
	}
}

func setup(t *testing.T, dbSQL string) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(configFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(dir, dbName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, dbSQL); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "autobrr", Dir: dir, Plan: p, Targets: targets(), Runs: runs()}
}

// TestArrRowResolvesByItsURLPath: an Arr row stores a whole URL, so its path
// says which app it meant on a host that serves several, and the box's own URL
// replaces it in the row the operator still sees in autobrr.
func TestArrRowResolvesByItsURLPath(t *testing.T) {
	ctx := context.Background()
	fixture := strings.Replace(dbFixture, "'sonarr.default.svc', 8989, 1",
		"'https://panel.example.com/sonarr/api', 10000, 1", 1)
	in := setup(t, fixture)
	// The source's own Service rule is gone: this host is the source's public
	// name, and only the paths say which app each address is.
	in.Plan.Hosts = []plan.HostRule{
		{Host: "panel.example.com", Path: "/sonarr", App: "sonarr"},
		{Host: "panel.example.com", Path: "/qbittorrent", App: "qbittorrent"},
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("an Arr URL with a path on a host of paths is not settled: %v %v", rep.Uncovered, rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readClients(t, in.Dir)[2].host; got != "http://sonarr:8989/sonarr" {
		t.Errorf("the Sonarr row = %q, want the box's URL with the source's path dropped", got)
	}
}

// runs is every instance the fixture plan has of each catalog app: one of each,
// which is what a proposal needs.
func runs() map[string][]string {
	return map[string][]string{
		"qbittorrent": {"qbittorrent"}, "sonarr": {"sonarr"}, "autobrr": {"autobrr"},
	}
}

// TestUnsettledClientProposesItsApp: a client row's type says which app its
// address can only mean, so an address nothing settles is proposed as that app
// when the box runs exactly one of it, and left and called out when it runs two.
func TestUnsettledClientProposesItsApp(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(dbFixture, "'localhost', 8081, 1", "'192.0.2.10', 8081, 1", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := rewrite.Default{
		Host: "192.0.2.10", Port: 8081, App: "qbittorrent",
		Why: "it is a QBITTORRENT client, and this box runs one qbittorrent",
	}
	if len(rep.Defaults) != 1 || rep.Defaults[0] != want {
		t.Fatalf("Defaults = %+v, want %+v", rep.Defaults, want)
	}

	// Two qBittorrents: an address nothing settles cannot mean one of them.
	in = setup(t, strings.Replace(dbFixture, "'localhost', 8081, 1", "'192.0.2.10', 8081, 1", 1))
	in.Runs["qbittorrent"] = []string{"qbittorrent", "qbittorrent-4k"}
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Defaults) != 0 {
		t.Errorf("Defaults = %+v, want none with two qBittorrents in the plan", rep.Defaults)
	}
	if len(rep.Unclassified) != 1 {
		t.Errorf("Unclassified = %v, want the address still reported and left", rep.Unclassified)
	}
}

func TestRewritesPathsAndConnections(t *testing.T) {
	// The Sonarr row is the shape autobrr stores for an Arr: a whole URL in
	// host, the port column left at the form's 10000. A bare host with the
	// port in the column is a download client's shape, not an Arr's.
	in := setup(t, arrFixture())
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	cfg := readConfig(t, in.Dir)
	for key, want := range map[string]string{
		"host":              "0.0.0.0",
		"port":              "7474",
		"baseUrl":           "/autobrr/",
		"baseUrlModeLegacy": "false",
		"logPath":           "/config/logs/autobrr.log",
		"sessionSecret":     "synthetic-secret",
	} {
		if cfg[key] != want {
			t.Errorf("config.toml %s = %q, want %q", key, cfg[key], want)
		}
	}
	// The inserted key went into the root table, not into [metrics], and the
	// comment that shipped with the file is still there.
	raw, err := os.ReadFile(filepath.Join(in.Dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# Set custom baseUrl") {
		t.Errorf("comments were not preserved:\n%s", raw)
	}
	if strings.Index(string(raw), "baseUrl = ") > strings.Index(string(raw), "[metrics]") {
		t.Errorf("baseUrl landed inside a table:\n%s", raw)
	}

	clients := readClients(t, in.Dir)
	for id, want := range map[int64]struct {
		host string
		port int
		tls  int
	}{
		1: {"qbittorrent", 8080, 0},
		// The Arr's address is its host URL, the box's this time; the port
		// column stays where the form left it, because nothing reads it.
		2: {"http://sonarr:8989/sonarr", 10000, 0},
	} {
		got := clients[id]
		if got.host != want.host || got.port != want.port || got.tls != want.tls {
			t.Errorf("client %d = %+v, want %+v", id, got, want)
		}
	}

	actions := readActions(t, in.Dir)
	wantAction := map[string]string{
		"exec_cmd":      "/config/bin/hook",
		"watch_folder":  "/config/watch",
		"save_path":     "/data/downloads/{{ .Category }}",
		"download_path": "/data/downloads",
	}
	for column, want := range wantAction {
		if actions[column] != want {
			t.Errorf("action.%s = %q, want %q", column, actions[column], want)
		}
	}

	// A second pass has nothing left to do, and no address is unclassified.
	rep2, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("after Write, Check still reports %v", rep2.Changes)
	}
	if len(rep2.Unclassified) != 0 {
		t.Errorf("after Write, Check reports unclassified addresses: %v", rep2.Unclassified)
	}
}

func TestCheckWritesNothing(t *testing.T) {
	in := setup(t, dbFixture)
	before := treeHash(t, in.Dir)
	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
}

func TestUncoveredActionPathBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(dbFixture,
		"'/srv/downloads')", "'/mnt/other')", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "action.download_path" {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered path")
	}
}

func TestUnclassifiedClientAddressBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(dbFixture,
		"'sonarr.default.svc'", "'192.0.2.10'", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v", rep.Unclassified)
	}
	if got := rep.Unclassified[0]; got.Host != "192.0.2.10" || got.Port != 8989 {
		t.Errorf("Unclassified[0] = %+v", got)
	}
	in.Plan.Hosts = append(in.Plan.Hosts, plan.HostRule{Host: "192.0.2.10", Leave: true})
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("a leave rule did not settle it: %v", rep.Unclassified)
	}
}

// TestMacroOnlyValueIsRelative: a save path that starts with a macro is
// qBittorrent's own save path plus text, so no rule can cover it and none is
// needed.
func TestMacroOnlyValueIsRelative(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(dbFixture,
		"'/srv/downloads/{{ .Category }}'", "'{{ .TorrentName }}'", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("a macro-only save path was treated as a path: %v", rep.Uncovered)
	}
}

// TestDisabledClientIsLeft: an address no rule settles on a client autobrr has
// switched off is not a gap. It is left exactly as it is, the report says so,
// and the run is not stopped.
func TestDisabledClientIsLeft(t *testing.T) {
	ctx := context.Background()
	fixture := strings.Replace(dbFixture, "'localhost', 8081, 1", "'192.0.2.10', 8081, 1", 1)
	on := setup(t, fixture)

	rep, err := rewriter{}.Check(ctx, on)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 || !rep.Blocked() {
		t.Fatalf("an enabled client at an address nothing settles = %v, want it reported", rep.Unclassified)
	}

	off := setup(t, strings.Replace(fixture, "'qBittorrent', 1,", "'qBittorrent', 0,", 1))
	rep, err = rewriter{}.Check(ctx, off)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a disabled client blocked the run: %v", rep.Unclassified)
	}
	if len(rep.Left) != 1 || rep.Left[0].What != "192.0.2.10:8081" {
		t.Fatalf("Left = %+v, want one entry for 192.0.2.10:8081", rep.Left)
	}
	if !strings.Contains(rep.Left[0].Why, "switched off") {
		t.Errorf("why = %q, want it to say the client is switched off", rep.Left[0].Why)
	}
	if _, err := (rewriter{}).Write(ctx, off); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readClients(t, off.Dir)[1]; got.host != "192.0.2.10" {
		t.Errorf("a disabled client's row was rewritten: %+v", got)
	}
}

// TestClientTypeBoxDoesNotRunIsLeft: box runs qBittorrent and the Arrs, and
// nothing else. Any other row is left and called out, never rewritten and never
// a refusal: pointing autobrr at an app that is not there is worse than leaving
// a row the operator can delete.
func TestClientTypeBoxDoesNotRunIsLeft(t *testing.T) {
	ctx := context.Background()
	fixture := strings.Replace(dbFixture,
		"VALUES (2, 'Sonarr', 1, 'SONARR', 'sonarr.default.svc', 8989, 1);",
		"VALUES (2, 'Deluge', 1, 'DELUGE', '192.0.2.10', 8112, 0);", 1)
	in := setup(t, fixture)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a client of a type box does not run blocked the run: %v", rep.Unclassified)
	}
	if len(rep.Left) != 1 || rep.Left[0].What != "192.0.2.10:8112" {
		t.Fatalf("Left = %+v, want one entry for 192.0.2.10:8112", rep.Left)
	}
	if !strings.Contains(rep.Left[0].Why, "DELUGE") {
		t.Errorf("why = %q, want it to name the client's type", rep.Left[0].Why)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readClients(t, in.Dir)[2]; got.host != "192.0.2.10" || got.port != 8112 {
		t.Errorf("a client box does not run was rewritten: %+v", got)
	}
}

func readConfig(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseTOML(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, v := range f.values {
		out[v.key] = v.str
	}
	return out
}

type client struct {
	host string
	port int
	tls  int
}

func readClients(t *testing.T, dir string) map[int64]client {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, host, port, tls FROM client`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]client{}
	for rows.Next() {
		var id int64
		var c client
		if err := rows.Scan(&id, &c.host, &c.port, &c.tls); err != nil {
			t.Fatal(err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readActions(t *testing.T, dir string) map[string]string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var execCmd, watch, save, download string
	if err := db.QueryRowContext(ctx,
		`SELECT exec_cmd, watch_folder, save_path, download_path FROM action WHERE id = 1`).
		Scan(&execCmd, &watch, &save, &download); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"exec_cmd": execCmd, "watch_folder": watch,
		"save_path": save, "download_path": download,
	}
}

// treeHash is every file's name, mode and bytes, so a pass that writes anything
// at all changes it.
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
