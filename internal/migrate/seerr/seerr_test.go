package seerr

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

// settingsFixture is a settings.json shaped like the one Seerr writes from its
// Settings interface (server/lib/settings/index.ts at v3.5.1), with Seerr's own
// two-space indentation.
const settingsFixture = `{
  "clientId": "synthetic-client-id",
  "main": {
    "apiKey": "synthetic-api-key",
    "applicationTitle": "Seerr",
    "applicationUrl": "https://requests.example.com",
    "cacheImages": false,
    "localLogin": true
  },
  "plex": {
    "name": "seedbox",
    "machineId": "synthetic-machine",
    "ip": "localhost",
    "port": 32400,
    "useSsl": true,
    "libraries": [
      {
        "id": "1",
        "name": "Movies",
        "enabled": true,
        "type": "movie"
      }
    ],
    "webAppUrl": "https://app.plex.tv/desktop"
  },
  "tautulli": {
    "hostname": "localhost",
    "port": 8181,
    "useSsl": true,
    "apiKey": "synthetic-tautulli-key"
  },
  "radarr": [
    {
      "id": 0,
      "name": "Radarr",
      "hostname": "localhost",
      "port": 7878,
      "apiKey": "synthetic-radarr-key",
      "useSsl": false,
      "baseUrl": "",
      "activeDirectory": "/srv/media/Movies"
    }
  ],
  "sonarr": [
    {
      "id": 0,
      "name": "Sonarr",
      "hostname": "sonarr.default.svc",
      "port": 8989,
      "apiKey": "synthetic-sonarr-key",
      "useSsl": true,
      "baseUrl": "/sonarr-old",
      "activeDirectory": "/srv/media/TV",
      "activeAnimeDirectory": "/srv/media/Anime"
    }
  ]
}
`

// dbFixture is the two root-folder columns, shaped after the TypeORM entities
// (server/entity/OverrideRule.ts and MediaRequest.ts at v3.5.1: the column
// keeps its property name, the table is the class name snake_cased).
const dbFixture = `
CREATE TABLE override_rule (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    radarrRootFolder varchar,
    sonarrRootFolder varchar,
    rootFolder varchar
);
CREATE TABLE media_request (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rootFolder varchar
);
INSERT INTO override_rule (id, rootFolder) VALUES (1, '/srv/media/Movies');
INSERT INTO media_request (id, rootFolder) VALUES (1, '/srv/media/Old Requests');
`

const planYAML = `
version: 1
apps:
  plex:
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
  tautulli:
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths: []
  sonarr:
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  radarr:
    source: radarr
    config: /srv/apps/radarr
    ports: [7878]
    paths: []
  seerr:
    source: seerr
    config: /srv/apps/seerr
    paths:
      - from: /srv/media
        to: /data/media
hosts:
  - service: sonarr.default
    app: sonarr
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"plex":     {Host: "plex", Port: 32400},
		"tautulli": {Host: "tautulli", Port: 8181},
		"sonarr":   {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"radarr":   {Host: "radarr", Port: 7878, URLBase: "/radarr"},
		"seerr":    {Host: "seerr", Port: 5055},
	}
}

func setup(t *testing.T, settings string) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, settingsName), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(dir, filepath.FromSlash(dbName))
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
	return rewrite.Input{App: "seerr", Dir: dir, Plan: p, Targets: targets()}
}

func TestRewritesDirectoriesAndConnections(t *testing.T) {
	ctx := context.Background()
	in := setup(t, settingsFixture)
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
	raw, err := os.ReadFile(filepath.Join(in.Dir, settingsName))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		`"applicationUrl": "https://requests.example.com"`,
		`"activeDirectory": "/data/media/Movies"`,
		`"activeDirectory": "/data/media/TV"`,
		`"activeAnimeDirectory": "/data/media/Anime"`,
		`"hostname": "radarr"`,
		`"hostname": "sonarr"`,
		`"baseUrl": "/radarr"`,
		`"baseUrl": "/sonarr"`,
		`"ip": "plex"`,
		`"useSsl": false`,
		// the libraries block is not a path and is untouched
		`"webAppUrl": "https://app.plex.tv/desktop"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("settings.json is missing %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"localhost"`) || strings.Contains(got, `"sonarr.default.svc"`) {
		t.Errorf("a connection still names the source:\n%s", got)
	}

	// The override rule moved; the request history did not, and was not
	// reported as a gap.
	if got := readColumn(t, in.Dir, "override_rule", "rootFolder"); got != "/data/media/Movies" {
		t.Errorf("override_rule.rootFolder = %q", got)
	}
	// The history path a rule covers is rewritten like any other; the one no
	// rule covers is the case the next test proves does not block.
	if got := readColumn(t, in.Dir, "media_request", "rootFolder"); got != "/data/media/Old Requests" {
		t.Errorf("media_request.rootFolder = %q", got)
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

// TestHistoryPathNeitherBlocksNorRewrites: a completed request records where it
// went and is never read again, so a path no rule covers there is not a gap.
func TestHistoryPathNeitherBlocksNorRewrites(t *testing.T) {
	ctx := context.Background()
	in := setup(t, settingsFixture)
	exec(t, in.Dir, `UPDATE media_request SET rootFolder = '/mnt/other/Requests'`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 0 {
		t.Errorf("Uncovered = %v, want history left alone", rep.Uncovered)
	}
	if rep.Blocked() {
		t.Error("history blocked the migration")
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := readColumn(t, in.Dir, "media_request", "rootFolder"); got != "/mnt/other/Requests" {
		t.Errorf("media_request.rootFolder = %q, want untouched", got)
	}
}

func TestCheckWritesNothing(t *testing.T) {
	in := setup(t, settingsFixture)
	before := treeHash(t, in.Dir)
	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
}

func TestUncoveredLivePathBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t, settingsFixture)
	exec(t, in.Dir, `UPDATE override_rule SET rootFolder = '/mnt/other/Movies'`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "override_rule.rootFolder" {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered path")
	}
}

func TestUncoveredSettingsPathBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(settingsFixture,
		`"activeDirectory": "/srv/media/Movies"`, `"activeDirectory": "/mnt/other/Movies"`, 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || !strings.Contains(rep.Uncovered[0].Key, "activeDirectory") {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
}

func TestUnclassifiedAddressBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(settingsFixture,
		"\"hostname\": \"localhost\",\n      \"port\": 7878", "\"hostname\": \"192.0.2.10\",\n      \"port\": 7878", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v", rep.Unclassified)
	}
	if got := rep.Unclassified[0]; got.Host != "192.0.2.10" || got.Port != 7878 {
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

// TestServiceNameConnection: the Sonarr entry stores the Service's DNS form,
// and the plan's host rule settles it.
func TestServiceNameConnection(t *testing.T) {
	ctx := context.Background()
	in := setup(t, settingsFixture)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("the Service name was not settled: %v", rep.Unclassified)
	}
}

func exec(t *testing.T, dir, sql string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, filepath.Join(dir, filepath.FromSlash(dbName)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

func readColumn(t *testing.T, dir, table, column string) string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, filepath.FromSlash(dbName)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out string
	if err := db.QueryRowContext(ctx, `SELECT `+column+` FROM `+table+` ORDER BY id LIMIT 1`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
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
