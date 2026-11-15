package qui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// configFixture is a config.toml shaped like qui's own template
// (internal/config/config.go: wDefaultConfig at v1.35.0): root settings only,
// baseUrl commented out, everything else a comment.
const configFixture = `# config.toml - Auto-generated on first run

# Hostname / IP
host = "localhost"

# Port
port = 7476

# Base URL
# Set custom baseUrl eg /qui/ to serve in subdirectory.
#baseUrl = "/qui/"

# Session secret
sessionSecret = "synthetic-secret"

# Log file path
#logPath = "log/qui.log"

# Data directory (default: next to config file)
#dataDir = "/var/db/qui"

# Backup directory (default: <dataDir>/backups)
backupDir = "/srv/apps/qui/backups"
`

// dbFixture is the subset of qui's schema this package reads, shaped after the
// migrations at v1.35.0: the string pool (010), instances (010/040), the Arr
// instances (043), the dir scanner (053), the orphan scanner (037), external
// programs (011), instance backups (009) and the cache tables those features
// fill. Values that qui interns are read through their *_id columns.
const dbFixture = `
CREATE TABLE string_pool (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    value TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE instances (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name_id INTEGER NOT NULL,
    host_id INTEGER NOT NULL,
    username_id INTEGER NOT NULL,
    password_encrypted TEXT NOT NULL,
    basic_username_id INTEGER,
    basic_password_encrypted TEXT,
    tls_skip_verify BOOLEAN NOT NULL DEFAULT 0,
    hardlink_base_dir TEXT NOT NULL DEFAULT ''
);
CREATE TABLE arr_instances (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    name_id INTEGER NOT NULL,
    base_url_id INTEGER NOT NULL,
    api_key_encrypted TEXT NOT NULL
);
CREATE TABLE dir_scan_directories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL,
    qbit_path_prefix TEXT,
    target_instance_id INTEGER NOT NULL
);
CREATE TABLE dir_scan_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    directory_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    triggered_by TEXT NOT NULL,
    scan_root TEXT
);
CREATE TABLE dir_scan_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    directory_id INTEGER NOT NULL,
    file_path TEXT NOT NULL,
    file_size INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE dir_scan_run_injections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL,
    save_path TEXT
);
CREATE TABLE orphan_scan_settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id INTEGER NOT NULL UNIQUE,
    ignore_paths TEXT
);
CREATE TABLE orphan_scan_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    triggered_by TEXT NOT NULL,
    scan_paths TEXT
);
CREATE TABLE orphan_scan_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL,
    file_path TEXT NOT NULL
);
CREATE TABLE external_programs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL,
    path_mappings TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE instance_backup_settings (
    instance_id INTEGER PRIMARY KEY,
    custom_path TEXT
);
CREATE TABLE instance_backup_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_id INTEGER NOT NULL,
    archive_path TEXT
);
CREATE TABLE instance_backup_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL,
    archive_rel_path TEXT
);
INSERT INTO string_pool (id, value) VALUES
    (1, 'seedbox'), (2, 'http://localhost:8081'), (3, 'admin'),
    (4, 'Sonarr'), (5, 'http://sonarr.default.svc:8989/sonarr');
INSERT INTO instances (id, name_id, host_id, username_id, password_encrypted, hardlink_base_dir)
VALUES (1, 1, 2, 3, 'encrypted', '/srv/downloads/hardlinks');
INSERT INTO arr_instances (id, type, name_id, base_url_id, api_key_encrypted)
VALUES (1, 'sonarr', 4, 5, 'encrypted');
INSERT INTO dir_scan_directories (id, path, qbit_path_prefix, target_instance_id)
VALUES (1, '/srv/media/Movies', '/srv/downloads', 1);
INSERT INTO dir_scan_runs (id, directory_id, status, triggered_by, scan_root)
VALUES (1, 1, 'completed', 'manual', '/srv/media/Movies');
INSERT INTO dir_scan_files (id, directory_id, file_path) VALUES
    (1, 1, '/srv/media/Movies/Example Film (2020)/Example Film (2020).mkv'),
    (2, 1, '/srv/media/Movies/Example Film (2020)/Example Film (2020).srt');
INSERT INTO dir_scan_run_injections (id, run_id, save_path) VALUES (1, 1, '/srv/downloads/Movies');
INSERT INTO orphan_scan_settings (id, instance_id, ignore_paths)
VALUES (1, 1, '["/srv/media/keep","/srv/media/Movies/ignore"]');
INSERT INTO orphan_scan_runs (id, instance_id, status, triggered_by, scan_paths)
VALUES (1, 1, 'completed', 'manual', '["/srv/downloads"]');
INSERT INTO orphan_scan_files (id, run_id, file_path) VALUES (1, 1, '/srv/downloads/orphan.mkv');
INSERT INTO external_programs (id, name, path, path_mappings)
VALUES (1, 'hook', '/srv/apps/qui/scripts/run.sh', '[{"from":"/srv/downloads/tv","to":"/srv/media/tv"}]');
INSERT INTO instance_backup_settings (instance_id, custom_path) VALUES (1, '/srv/apps/qui/backups');
INSERT INTO instance_backup_runs (id, instance_id, archive_path) VALUES (1, 1, '/srv/apps/qui/backups/one.tar');
INSERT INTO instance_backup_items (id, run_id, archive_rel_path) VALUES (1, 1, 'torrents/one.torrent');
`

const planYAML = `
version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8081]
    paths: []
  sonarr:
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  qui:
    source: qui
    config: /srv/apps/qui
    paths:
      - from: /srv/apps/qui
        to: /config
      - from: /srv/downloads
        to: /data/downloads
      - from: /srv/media
        to: /data/media
hosts:
  - service: sonarr.default
    app: sonarr
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"qui":         {Host: "qui", Port: 7476, URLBase: "/qui"},
	}
}

func setup(t *testing.T) rewrite.Input {
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
	if _, err := db.ExecContext(ctx, dbFixture); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "qui", Dir: dir, Plan: p, Targets: targets()}
}

func TestRewritesPathsAndConnections(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, in.Dir)
	for key, want := range map[string]string{
		"host":      "0.0.0.0",
		"port":      "7476",
		"baseUrl":   "/qui",
		"backupDir": "/config/backups",
	} {
		if cfg[key] != want {
			t.Errorf("config.toml %s = %q, want %q", key, cfg[key], want)
		}
	}

	q := query(t, in.Dir)
	for where, want := range map[string]string{
		"instances.host":                        "http://qbittorrent:8080",
		"arr_instances.base_url":                "http://sonarr:8989/sonarr",
		"dir_scan_directories.path":             "/data/media/Movies",
		"dir_scan_directories.qbit_path_prefix": "/data/downloads",
		"instances.hardlink_base_dir":           "/data/downloads/hardlinks",
		"external_programs.path":                "/config/scripts/run.sh",
		"instance_backup_settings.custom_path":  "/config/backups",
	} {
		if got := q[where]; got != want {
			t.Errorf("%s = %q, want %q", where, got, want)
		}
	}

	var ignores []string
	if err := json.Unmarshal([]byte(q["orphan_scan_settings.ignore_paths"]), &ignores); err != nil {
		t.Fatalf("ignore_paths: %v", err)
	}
	sort.Strings(ignores)
	wantIgnores := []string{"/data/media/Movies/ignore", "/data/media/keep"}
	if len(ignores) != 2 || ignores[0] != wantIgnores[0] || ignores[1] != wantIgnores[1] {
		t.Errorf("ignore_paths = %v, want %v", ignores, wantIgnores)
	}

	var mappings []struct{ From, To string }
	if err := json.Unmarshal([]byte(q["external_programs.path_mappings"]), &mappings); err != nil {
		t.Fatalf("path_mappings: %v", err)
	}
	if len(mappings) != 1 || mappings[0].From != "/data/downloads/tv" || mappings[0].To != "/data/media/tv" {
		t.Errorf("path_mappings = %+v", mappings)
	}

	// The interned value is a new pool row, not an edited old one: the old text
	// may be referenced from elsewhere, and the app's own interning only ever
	// appends.
	if !poolHas(t, in.Dir, "http://qbittorrent:8080") {
		t.Error("the rewritten instance host was not interned")
	}
	if !poolHas(t, in.Dir, "http://localhost:8081") {
		t.Error("the old interned value was removed")
	}

	// The caches a scan rebuilds are empty.
	for table, count := range map[string]int{
		"dir_scan_files": 0, "dir_scan_runs": 0, "dir_scan_run_injections": 0,
		"orphan_scan_files": 0, "orphan_scan_runs": 0,
		"instance_backup_items": 0, "instance_backup_runs": 0,
	} {
		if got := countRows(t, in.Dir, table); got != count {
			t.Errorf("%s holds %d rows, want %d", table, got, count)
		}
	}

	// A second pass has nothing left to do.
	rep2, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("after Write, Check still reports %v", rep2.Changes)
	}
}

// TestDataDirOutsideTheConfigIsAGap: qui's database sits wherever dataDir says.
// A dataDir no rule covers is a gap in the plan like any other path, and the run
// stops before it writes.
func TestDataDirOutsideTheConfigIsAGap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config := strings.Replace(configFixture, "#dataDir = \"/var/db/qui\"",
		"dataDir = \"/mnt/other/qui\"", 1)
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dbName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	in := rewrite.Input{App: "qui", Dir: dir, Plan: p, Targets: targets()}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range rep.Uncovered {
		if u.Key == "dataDir" && u.Path == "/mnt/other/qui" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Uncovered = %v, want the data directory", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered data directory")
	}
}

// TestDataDirInsideTheConfigIsFollowed: a dataDir under the staged config is
// where the database is, and it moves with the rest.
func TestDataDirInsideTheConfigIsFollowed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config := strings.Replace(configFixture, "#dataDir = \"/var/db/qui\"",
		"dataDir = \"/srv/apps/qui/state\"", 1)
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(state, dbName)
	if err := os.WriteFile(dbFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadWrite(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, dbFixture); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatal(err)
	}
	in := rewrite.Input{App: "qui", Dir: dir, Plan: p, Targets: targets()}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Changes {
		if c.File == configName && c.Where == "dataDir" && c.What == "path" {
			return
		}
	}
	t.Fatalf("the dataDir under /config was not rewritten: %v", rep.Changes)
}

func TestCheckWritesNothing(t *testing.T) {
	in := setup(t)
	before := treeHash(t, in.Dir)
	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
}

func TestUncoveredPathBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	exec(t, in.Dir, `UPDATE dir_scan_directories SET path = '/mnt/other/Movies' WHERE id = 1`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range rep.Uncovered {
		if u.Key == "dir_scan_directories.path" && u.Path == "/mnt/other/Movies" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Uncovered = %v, want the directory", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered path")
	}
}

func TestUnclassifiedAddressBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	exec(t, in.Dir, `INSERT OR IGNORE INTO string_pool (value) VALUES ('http://192.0.2.10:8081');
		UPDATE instances SET host_id = (SELECT id FROM string_pool WHERE value = 'http://192.0.2.10:8081') WHERE id = 1;`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v", rep.Unclassified)
	}
	if got := rep.Unclassified[0]; got.Host != "192.0.2.10" || got.Port != 8081 || got.Where != "instances.host" {
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

// TestServiceNameConnection: the Arr connection stores a Service's short name,
// and the plan's host rule is what settles it.
func TestServiceNameConnection(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("the Service name was not settled: %v", rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := query(t, in.Dir)["arr_instances.base_url"]; got != "http://sonarr:8989/sonarr" {
		t.Errorf("arr_instances.base_url = %q", got)
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

// query reads the columns the rewriter changes, keyed "table.column".
func query(t *testing.T, dir string) map[string]string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]string{}
	read := func(key, sql string, dest *string) {
		if err := db.QueryRowContext(ctx, sql).Scan(dest); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		out[key] = *dest
	}
	var host, base, path, prefix, hardlink, extPath, mappings, custom, ignores string
	read("instances.host", `SELECT p.value FROM instances i JOIN string_pool p ON i.host_id = p.id WHERE i.id = 1`, &host)
	read("arr_instances.base_url", `SELECT p.value FROM arr_instances a JOIN string_pool p ON a.base_url_id = p.id WHERE a.id = 1`, &base)
	read("dir_scan_directories.path", `SELECT path FROM dir_scan_directories WHERE id = 1`, &path)
	read("dir_scan_directories.qbit_path_prefix", `SELECT qbit_path_prefix FROM dir_scan_directories WHERE id = 1`, &prefix)
	read("instances.hardlink_base_dir", `SELECT hardlink_base_dir FROM instances WHERE id = 1`, &hardlink)
	read("external_programs.path", `SELECT path FROM external_programs WHERE id = 1`, &extPath)
	read("external_programs.path_mappings", `SELECT path_mappings FROM external_programs WHERE id = 1`, &mappings)
	read("instance_backup_settings.custom_path", `SELECT custom_path FROM instance_backup_settings WHERE instance_id = 1`, &custom)
	read("orphan_scan_settings.ignore_paths", `SELECT ignore_paths FROM orphan_scan_settings WHERE id = 1`, &ignores)
	return out
}

func poolHas(t *testing.T, dir, value string) bool {
	t.Helper()
	return countRows(t, dir, "string_pool", "value = ?", value) == 1
}

func countRows(t *testing.T, dir, table string, where ...any) int {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sql := "SELECT count(*) FROM " + table
	var args []any
	if len(where) > 0 {
		sql += " WHERE " + where[0].(string)
		args = where[1:]
	}
	var n int
	if err := db.QueryRowContext(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

func exec(t *testing.T, dir, sql string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, sql); err != nil {
		t.Fatal(err)
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
