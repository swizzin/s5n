package tautulli

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
)

// fixture is a config.ini shaped like the one Tautulli writes from
// _CONFIG_DEFINITIONS (plexpy/config.py at v2.18.2): one section per group of
// settings, `key = value`, comments between them, and a quoted value holding a
// space.
const fixture = `[General]
# the web server
HTTP_HOST = 127.0.0.1
HTTP_PORT = 8181
HTTP_ROOT = /tautulli
HTTP_USERNAME = admin
BACKUP_DIR = /srv/apps/tautulli/backups
CACHE_DIR = /srv/apps/tautulli/cache
EXPORT_DIR = /srv/apps/tautulli/exports
LOG_DIR = "/srv/apps/tautulli/logs"
GIT_PATH = 
HTTPS_CERT = /srv/apps/tautulli/server.crt
HTTPS_CERT_CHAIN = 
HTTPS_KEY = /srv/apps/tautulli/server.key

[PMS]
PMS_IP = localhost
PMS_PORT = 32400
PMS_SSL = 1
PMS_URL = https://localhost:32400
PMS_LOGS_FOLDER = /srv/plex/logs
PMS_TOKEN = secret-token

[Newsletter]
NEWSLETTER_DIR = /srv/apps/tautulli/newsletters
NEWSLETTER_CUSTOM_DIR = /srv/plex/newsletters
NEWSLETTER_TEMPLATES = newsletters
`

const planYAML = `
version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths:
      - from: /srv/plex
        to: /config/plex
  tautulli:
    kind: kubernetes
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths:
      - from: /srv/apps/tautulli
        to: /config
      - from: /srv/plex
        to: /config/plex
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"plex":     {Host: "plex", Port: 32400},
		"tautulli": {Host: "tautulli", Port: 8181},
	}
}

func setup(t *testing.T) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "tautulli", Dir: dir, Plan: p, Targets: targets()}
}

func TestRewritesPathsAddressesAndListenSettings(t *testing.T) {
	in := setup(t)
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
	got := read(t, in.Dir)

	want := map[string]string{
		"General/HTTP_HOST":                "0.0.0.0",
		"General/HTTP_PORT":                "8181",
		"General/HTTP_ROOT":                "",
		"General/BACKUP_DIR":               "/config/backups",
		"General/CACHE_DIR":                "/config/cache",
		"General/EXPORT_DIR":               "/config/exports",
		"General/LOG_DIR":                  "/config/logs",
		"General/GIT_PATH":                 "",
		"General/HTTPS_CERT":               "/config/server.crt",
		"General/HTTPS_CERT_CHAIN":         "",
		"General/HTTPS_KEY":                "/config/server.key",
		"PMS/PMS_IP":                       "plex",
		"PMS/PMS_PORT":                     "32400",
		"PMS/PMS_SSL":                      "0",
		"PMS/PMS_URL":                      "http://plex:32400",
		"PMS/PMS_LOGS_FOLDER":              "/config/plex/logs",
		"PMS/PMS_TOKEN":                    "secret-token",
		"Newsletter/NEWSLETTER_TEMPLATES":  "newsletters",
		"Newsletter/NEWSLETTER_DIR":        "/config/newsletters",
		"Newsletter/NEWSLETTER_CUSTOM_DIR": "/config/plex/newsletters",
	}
	for where, value := range want {
		if got[where] != value {
			t.Errorf("%s = %q, want %q", where, got[where], value)
		}
	}

	// Only the values changed: comments, the blank quote-only line and every
	// line's position are as they were.
	after, err := os.ReadFile(filepath.Join(in.Dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{
		"# the web server",
		"LOG_DIR = \"/config/logs\"\n",
		"HTTPS_CERT_CHAIN = \n",
		"HTTP_USERNAME = admin\n",
		"[PMS]\n",
	} {
		if !strings.Contains(string(after), kept) {
			t.Errorf("%q is not in the rewritten file:\n%s", kept, after)
		}
	}
	// A second pass has nothing left to do, and no address is unclassified:
	// PMS_IP now names the box, and in.Resolve knows the box's own endpoints.
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

func TestUncoveredPathBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	// A directory outside every rule.
	patched := strings.Replace(fixture, "CACHE_DIR = /srv/apps/tautulli/cache", "CACHE_DIR = /mnt/other/cache", 1)
	if err := os.WriteFile(filepath.Join(in.Dir, configName), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "General/CACHE_DIR" || rep.Uncovered[0].Path != "/mnt/other/cache" {
		t.Fatalf("Uncovered = %v", rep.Uncovered)
	}
	if !rep.Blocked() {
		t.Error("an uncovered path did not block")
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an uncovered path")
	}
}

func TestUnclassifiedAddressBlocks(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	patched := strings.Replace(fixture, "PMS_IP = localhost", "PMS_IP = 192.0.2.10", 1)
	if err := os.WriteFile(filepath.Join(in.Dir, configName), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v, want the LAN address", rep.Unclassified)
	}
	if got := rep.Unclassified[0]; got.Host != "192.0.2.10" || got.Port != 32400 {
		t.Errorf("Unclassified[0] = %+v", got)
	}
	// The plan can keep it as stored.
	in.Plan.Hosts = append(in.Plan.Hosts, plan.HostRule{Host: "192.0.2.10", Leave: true})
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("a leave rule did not settle it: %v", rep.Unclassified)
	}
	// A Service name resolves through the plan's host rules.
	in.Plan.Hosts = []plan.HostRule{{Service: "plex.media", App: "plex"}}
	patched = strings.Replace(fixture, "PMS_IP = localhost", "PMS_IP = plex.media.svc", 1)
	if err := os.WriteFile(filepath.Join(in.Dir, configName), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a discovered Service was not settled: %v", rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := read(t, in.Dir)["PMS/PMS_IP"]; got != "plex" {
		t.Errorf("PMS_IP = %q, want plex", got)
	}
}

func read(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseINI(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range f.lines {
		out[l.section+"/"+l.key] = l.value
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
