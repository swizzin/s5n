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

// fixture is shaped like the config.ini Tautulli writes from
// _CONFIG_DEFINITIONS (plexpy/config.py at v2.18.2). Keys are lower case, the only
// spelling Tautulli writes: _define folds each name (:473) and Config.write()
// builds the file from those keys (:528-556).
const fixture = `[General]
# the web server
http_host = 127.0.0.1
http_port = 8181
http_root = tautulli
http_username = admin
backup_dir = /srv/apps/tautulli/backups
cache_dir = /srv/apps/tautulli/cache
export_dir = /srv/apps/tautulli/exports
log_dir = "/srv/apps/tautulli/logs"
git_path = 
https_cert = /srv/apps/tautulli/server.crt
https_cert_chain = 
https_key = /srv/apps/tautulli/server.key

[PMS]
pms_ip = localhost
pms_port = 32400
pms_ssl = 1
pms_url = https://localhost:32400
pms_logs_folder = /srv/plex/logs
pms_token = secret-token

[Newsletter]
newsletter_dir = /srv/apps/tautulli/newsletters
newsletter_custom_dir = /srv/plex/newsletters
newsletter_templates = newsletters
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
		"tautulli": {Host: "tautulli", Port: 8181, URLBase: "/tautulli"},
	}
}

func setupFile(t *testing.T, text string) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "tautulli", Dir: dir, Plan: p, Targets: targets()}
}

func setup(t *testing.T) rewrite.Input { t.Helper(); return setupFile(t, fixture) }

// rewritten runs both passes over one config.ini, returning it by section/key as
// the file spells them.
func rewritten(t *testing.T, text string) map[string]string {
	t.Helper()
	in := setupFile(t, text)
	ctx := context.Background()
	if rep, err := (rewriter{}).Check(ctx, in); err != nil {
		t.Fatal(err)
	} else if rep.Blocked() {
		t.Fatalf("Blocked: uncovered %v, unclassified %v", rep.Uncovered, rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	return read(t, in.Dir)
}

// respelled is the same config.ini with every key in the other case, as a
// hand-edited file looks. Tautulli reads only the lower-case name, but rewriting
// nothing there is the failure this package exists to avoid.
func respelled(ini string) string {
	var out []string
	for _, line := range strings.Split(ini, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			out = append(out, line)
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			out = append(out, line)
			continue
		}
		out = append(out, strings.ToUpper(name)+"="+value)
	}
	return strings.Join(out, "\n")
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
		"General/http_host":                "0.0.0.0",
		"General/http_port":                "8181",
		"General/http_root":                "/tautulli",
		"General/http_username":            "",
		"General/backup_dir":               "/config/backups",
		"General/cache_dir":                "/config/cache",
		"General/export_dir":               "/config/exports",
		"General/log_dir":                  "/config/logs",
		"General/git_path":                 "",
		"General/https_cert":               "/config/server.crt",
		"General/https_cert_chain":         "",
		"General/https_key":                "/config/server.key",
		"PMS/pms_ip":                       "plex",
		"PMS/pms_port":                     "32400",
		"PMS/pms_ssl":                      "0",
		"PMS/pms_url":                      "http://plex:32400",
		"PMS/pms_logs_folder":              "/config/plex/logs",
		"PMS/pms_token":                    "secret-token",
		"Newsletter/newsletter_templates":  "newsletters",
		"Newsletter/newsletter_dir":        "/config/newsletters",
		"Newsletter/newsletter_custom_dir": "/config/plex/newsletters",
	}
	for where, value := range want {
		if got[where] != value {
			t.Errorf("%s = %q, want %q", where, got[where], value)
		}
	}

	// Only the values changed: comments, the quote-only line and positions stand.
	after, err := os.ReadFile(filepath.Join(in.Dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{
		"# the web server",
		"log_dir = \"/config/logs\"\n",
		"https_cert_chain = \n",
		"[PMS]\n",
	} {
		if !strings.Contains(string(after), kept) {
			t.Errorf("%q is not in the rewritten file:\n%s", kept, after)
		}
	}
	// A second pass has nothing to do: pms_ip names the box's own endpoint.
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

// TestReadsEverySpellingOfAKey: the same config in either case is rewritten the
// same way, and keeps the spelling it had.
func TestReadsEverySpellingOfAKey(t *testing.T) {
	want := map[string]string{
		"General/http_host":         "0.0.0.0",
		"General/http_root":         "/tautulli",
		"General/backup_dir":        "/config/backups",
		"General/log_dir":           "/config/logs",
		"General/https_cert":        "/config/server.crt",
		"PMS/pms_ip":                "plex",
		"PMS/pms_ssl":               "0",
		"PMS/pms_url":               "http://plex:32400",
		"Newsletter/newsletter_dir": "/config/newsletters",
	}
	for _, tt := range []struct {
		name string
		text string
	}{
		{"lower case, the spelling Tautulli writes", fixture},
		{"upper case, a file a person hand-edited", respelled(fixture)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := rewritten(t, tt.text)
			for where, value := range want {
				v, ok := lookupFold(got, where)
				if !ok {
					t.Errorf("%s is not in the rewritten file: %v", where, got)
					continue
				}
				if v != value {
					t.Errorf("%s = %q, want %q", where, v, value)
				}
			}
		})
	}
}

// lookupFold asks read()'s map without caring how the file spelled the key.
func lookupFold(ini map[string]string, where string) (string, bool) {
	for k, v := range ini {
		if strings.EqualFold(k, where) {
			return v, true
		}
	}
	return "", false
}

// TestKeepsTheKeySpellingOnDisk: the bytes keep their case and gain no other.
func TestKeepsTheKeySpellingOnDisk(t *testing.T) {
	in := setupFile(t, respelled(fixture))
	ctx := context.Background()
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(in.Dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HTTP_ROOT = /tautulli\n", "HTTP_USERNAME = \"\"\n", "PMS_IP = plex\n", "LOG_DIR = \"/config/logs\"\n"} {
		if !strings.Contains(string(after), want) {
			t.Errorf("%q is not in the rewritten file:\n%s", want, after)
		}
	}
	if strings.Contains(string(after), "\nhttp_") || strings.Contains(string(after), "\npms_") {
		t.Errorf("the rewrite added a key in the other spelling:\n%s", after)
	}
}

// TestReportNamesTheSpellingTheFileUses: the plan names the line the file holds.
func TestReportNamesTheSpellingTheFileUses(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want []string
	}{
		{"lower case", fixture, []string{"General/http_host", "General/http_root", "PMS/pms_url"}},
		{"upper case", respelled(fixture), []string{"General/HTTP_HOST", "General/HTTP_ROOT", "PMS/PMS_URL"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := setupFile(t, tt.text)
			rep, err := rewriter{}.Check(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			var have []string
			for _, c := range rep.Changes {
				have = append(have, c.Where)
			}
			for _, want := range tt.want {
				found := false
				for _, where := range have {
					if where == want {
						found = true
					}
				}
				if !found {
					t.Errorf("the plan does not name %s; it names %v", want, have)
				}
			}
		})
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
	patched := strings.Replace(fixture, "cache_dir = /srv/apps/tautulli/cache", "cache_dir = /mnt/other/cache", 1)
	if err := os.WriteFile(filepath.Join(in.Dir, configName), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Key != "General/cache_dir" || rep.Uncovered[0].Path != "/mnt/other/cache" {
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
	patched := strings.Replace(fixture, "pms_ip = localhost", "pms_ip = 192.0.2.10", 1)
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
	patched = strings.Replace(fixture, "pms_ip = localhost", "pms_ip = plex.media.svc", 1)
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
	if got := read(t, in.Dir)["PMS/pms_ip"]; got != "plex" {
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

// treeHash is every file's name, mode and bytes: a pass that writes changes it.
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

// withLogin is the fixture with the source's own web login configured: a name, a
// hashed password and the two flags saying the value is a hash.
const withLogin = `[General]
http_host = 127.0.0.1
http_port = 8181
http_root = tautulli
http_username = someone
http_password = 901fd0ee12ab
http_hash_password = 1
http_hashed_password = 1
http_basic_auth = 1

[PMS]
pms_ip = localhost
pms_port = 32400
pms_ssl = 1
`

// The app's own login is switched off and the plan says so, matching what the
// chart's init container re-seals on every start (internal/ttlconf).
func TestSwitchesTheAppsOwnLoginOffAndSaysSo(t *testing.T) {
	in := setupFile(t, withLogin+"\npath_map = /srv/plex=/config/plex\n")
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var said bool
	for _, n := range rep.Notes {
		if n == "its own login is switched off; you sign in through the box" {
			said = true
		}
	}
	if !said {
		t.Fatalf("the plan does not say the app's own login is off: %v", rep.Notes)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := read(t, in.Dir)
	for where, want := range map[string]string{
		"General/http_username":        "",
		"General/http_password":        "",
		"General/http_hash_password":   "0",
		"General/http_hashed_password": "0",
		"General/http_basic_auth":      "0",
		"General/http_root":            "/tautulli",
	} {
		if got[where] != want {
			t.Errorf("%s = %q, want %q", where, got[where], want)
		}
	}
}

// A key the file does not hold is not written: the app's defaults answer for it.
func TestLoginOffLeavesAbsentKeysAlone(t *testing.T) {
	got := rewritten(t, "[General]\nhttp_port = 8181\n\n[PMS]\npms_ip = localhost\npms_port = 32400\n")
	for _, where := range []string{"General/http_username", "General/http_password", "General/http_basic_auth"} {
		if _, ok := got[where]; ok {
			t.Errorf("%s was added to a file that did not hold it: %v", where, got)
		}
	}
}

// Tautulli 2.19's two switches are the other half of "its own login is off": at
// the install defaults they mint a password for a wizard that never finished and
// refuse every request a reverse proxy forwarded.
func TestSwitchesTheTwoSwitches219AddedToKeepTheLoginOff(t *testing.T) {
	got := rewritten(t, "[General]\nhttp_port = 8181\nfirst_run_complete = 0\n"+
		"http_password = \"\"\n\n[PMS]\npms_ip = localhost\npms_port = 32400\n\n"+
		"[Advanced]\nallow_public_no_password = 0\nconfig_version = 23\n")
	for where, want := range map[string]string{
		"General/first_run_complete":        "1",
		"Advanced/allow_public_no_password": "1",
	} {
		if got[where] != want {
			t.Errorf("%s = %q, want %q", where, got[where], want)
		}
	}
	// The app's own neighbours in both sections are the app's.
	if got["Advanced/config_version"] != "23" {
		t.Errorf("Advanced/config_version = %q, want it left alone", got["Advanced/config_version"])
	}
}
