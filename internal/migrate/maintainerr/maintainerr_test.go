package maintainerr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// fixture is a database shaped after the tables and columns Maintainerr's own
// migrations create (apps/server/src/database/migrations at v3.32.0), with a
// single settings row and one server per Arr.
const fixture = `
CREATE TABLE settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    clientId varchar,
    applicationTitle varchar NOT NULL DEFAULT 'Maintainerr',
    applicationUrl varchar NOT NULL DEFAULT 'localhost',
    apikey varchar,
    locale varchar NOT NULL DEFAULT 'en',
    plex_name varchar,
    plex_hostname varchar,
    plex_port integer DEFAULT 32400,
    plex_ssl integer,
    plex_auth_token varchar,
    seerr_url varchar,
    seerr_api_key varchar,
    tautulli_url varchar,
    tautulli_api_key varchar,
    download_client_url varchar,
    jellyfin_url varchar,
    emby_url varchar
);
CREATE TABLE radarr_settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    serverName varchar NOT NULL,
    url varchar,
    apiKey varchar
);
CREATE TABLE sonarr_settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    serverName varchar NOT NULL,
    url varchar,
    apiKey varchar
);
INSERT INTO settings (id, applicationUrl, plex_hostname, plex_port, plex_ssl,
    seerr_url, tautulli_url, download_client_url)
VALUES (1, 'https://requests.example.com', 'localhost', 32400, 1,
    'http://localhost:5055', 'http://localhost:8181', 'http://localhost:8081');
INSERT INTO radarr_settings (serverName, url, apiKey) VALUES ('Radarr', 'http://localhost:7878/radarr', 'radarr-key');
INSERT INTO sonarr_settings (serverName, url, apiKey) VALUES ('Sonarr', 'http://localhost:8989/sonarr', 'sonarr-key');
`

const planYAML = `
version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
  seerr:
    kind: kubernetes
    source: seerr
    config: /srv/apps/seerr
    ports: [5055]
    paths: []
  tautulli:
    kind: kubernetes
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths: []
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    ports: [7878]
    paths: []
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
    ports: [8081]
    paths: []
  maintainerr:
    kind: kubernetes
    source: maintainerr
    config: /srv/apps/maintainerr
    paths: []
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"plex":        {Host: "plex", Port: 32400},
		"seerr":       {Host: "seerr", Port: 5055},
		"tautulli":    {Host: "tautulli", Port: 8181},
		"radarr":      {Host: "radarr", Port: 7878, URLBase: "/radarr"},
		"sonarr":      {Host: "sonarr", Port: 8989, URLBase: "/sonarr"},
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
		"maintainerr": {Host: "maintainerr", Port: 6246},
	}
}

// setup builds a staged config with the fixture database and returns the input.
func setup(t *testing.T) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, dbName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "maintainerr", Dir: dir, Plan: p, Targets: targets()}
}

func TestRewritesEveryConnection(t *testing.T) {
	in := setup(t)
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	want := map[string]int{
		"settings.plex_hostname":       1,
		"settings.plex_ssl":            1,
		"settings.seerr_url":           1,
		"settings.tautulli_url":        1,
		"settings.download_client_url": 1,
		"radarr_settings.url":          1,
		"sonarr_settings.url":          1,
	}
	got := map[string]int{}
	for _, c := range rep.Changes {
		got[c.Where] += c.Count
	}
	for where, n := range want {
		if got[where] != n {
			t.Errorf("%s: %d changes, want %d (all: %v)", where, got[where], n, got)
		}
	}

	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	values := readSettingsRow(t, in.Dir)
	for column, want := range map[string]string{
		"plex_hostname":       "plex",
		"plex_port":           "32400",
		"plex_ssl":            "0",
		"seerr_url":           "http://seerr:5055",
		"tautulli_url":        "http://tautulli:8181",
		"download_client_url": "http://qbittorrent:8080",
		// the public address people open is not this app's to rewrite
		"applicationUrl": "https://requests.example.com",
	} {
		if values[column] != want {
			t.Errorf("settings.%s = %q, want %q", column, values[column], want)
		}
	}
	for table, want := range map[string]string{
		"radarr_settings": "http://radarr:7878/radarr",
		"sonarr_settings": "http://sonarr:8989/sonarr",
	} {
		if got := readURL(t, in.Dir, table); got != want {
			t.Errorf("%s.url = %q, want %q", table, got, want)
		}
	}

	// A second pass over the written copy has nothing left to do, and no
	// address is unclassified.
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
	in := setup(t)
	before := treeHash(t, in.Dir)
	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
}

func TestUnclassifiedAddressBlocks(t *testing.T) {
	in := setup(t)
	ctx := context.Background()
	exec(t, in.Dir, `UPDATE settings SET seerr_url = 'http://192.0.2.10:5055' WHERE id = 1`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v, want the LAN address", rep.Unclassified)
	}
	if got := rep.Unclassified[0]; got.Host != "192.0.2.10" || got.Port != 5055 ||
		got.File != dbName || got.Where != "settings.seerr_url" {
		t.Errorf("Unclassified[0] = %+v", got)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an unclassified address")
	}
	// The left-as-stored case is the plan saying so.
	p := in.Plan
	p.Hosts = append(p.Hosts, plan.HostRule{Host: "192.0.2.10", Leave: true})
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("a leave rule did not settle it: %v", rep.Unclassified)
	}
}

// TestUnsettledPlexConnectionProposesPlex: plex_hostname/plex_port are
// Maintainerr's Plex connection by name, so an address nothing settles there can
// only be the box's Plex. That is a proposal for Settle to write as a default
// (the address is still reported, and Check still writes nothing); a URL column
// that names no app in particular proposes nothing.
func TestUnsettledPlexConnectionProposesPlex(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	exec(t, in.Dir, `UPDATE settings SET plex_hostname = '192.0.2.56', plex_port = 32400 WHERE id = 1`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := rewrite.Default{
		Host: "192.0.2.56", Port: 32400, App: "plex",
		Why: "it is Maintainerr's Plex connection, and this box runs Plex",
	}
	if len(rep.Defaults) != 1 || rep.Defaults[0] != want {
		t.Fatalf("Defaults = %+v, want %+v", rep.Defaults, want)
	}
	if len(rep.Unclassified) != 1 {
		t.Errorf("Unclassified = %v, want the address still reported", rep.Unclassified)
	}

	// A plan with no Plex has nothing to propose.
	in = setup(t)
	delete(in.Plan.Apps, "plex")
	delete(in.Targets, "plex")
	exec(t, in.Dir, `UPDATE settings SET plex_hostname = '192.0.2.56' WHERE id = 1`)
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Defaults) != 0 {
		t.Errorf("Defaults = %+v, want none without a Plex in the plan", rep.Defaults)
	}
}

func TestUnparseableURLIsRefused(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	exec(t, in.Dir, `UPDATE radarr_settings SET url = 'not a url'`)
	if _, err := (rewriter{}).Check(ctx, in); err == nil {
		t.Error("a settings value that is not a URL did not stop the run")
	}
}

func TestExternalMediaServersAreNoted(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	exec(t, in.Dir, `UPDATE settings SET jellyfin_url = 'https://jellyfin.example.com'`)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Notes) == 0 {
		t.Error("no note about the media servers box does not migrate")
	}
	if rep.Blocked() {
		t.Errorf("an external media server blocked the run: %v", rep.Unclassified)
	}
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

func readSettingsRow(t *testing.T, dir string) map[string]string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT plex_hostname, plex_port, plex_ssl, seerr_url,
        tautulli_url, download_client_url, applicationUrl FROM settings WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no settings row")
	}
	var host, port, ssl, seerrURL, tautulliURL, downloadURL, appURL string
	if err := rows.Scan(&host, &port, &ssl, &seerrURL, &tautulliURL, &downloadURL, &appURL); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"plex_hostname": host, "plex_port": port, "plex_ssl": ssl,
		"seerr_url": seerrURL, "tautulli_url": tautulliURL,
		"download_client_url": downloadURL, "applicationUrl": appURL,
	}
}

func readURL(t *testing.T, dir, table string) string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out string
	if err := db.QueryRowContext(ctx, `SELECT url FROM `+table+` ORDER BY id LIMIT 1`).Scan(&out); err != nil {
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
