package sabnzbd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/sabconf"
)

// sourcePlan: downloads and scripts under /downloads and /library, box's tree
// /data and /config. Every path is invented.
const sourcePlan = `version: 2
apps:
  sabnzbd:
    kind: kubernetes
    source: sabnzbd
    config: /srv/apps/sabnzbd
    paths:
      - from: /downloads
        to: /data/downloads
      - from: /library/scripts
        to: /config/scripts
      - from: /library/backup
        to: /config/backup
`

// sourceINI is the shape SABnzbd 5 writes, with its own login and an already
// obfuscated server password.
const sourceINI = `__version__ = 19
__encoding__ = utf-8
[misc]
host = 0.0.0.0
port = 8080
username = admin
password = hunter2
api_key = 0123456789abcdef0123456789abcdef
nzb_key = abcdef
download_dir = /downloads/incomplete
complete_dir = /downloads/complete
script_dir = /library/scripts
nzb_backup_dir = /library/backup
url_base = /
[categories]
[[*]]
order = 0
[[tv]]
order = 1
dir = /downloads/complete/tv
[[4k/movies]]
order = 2
dir = /downloads/complete/4K/Movies
[servers]
[[news]]
host = news.example.com
port = 563
username = person
password = !!!encoded!!!616263
`

func input(t *testing.T, doc string) rewrite.Input {
	t.Helper()
	p, err := plan.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse plan: %v", err)
	}
	return rewrite.Input{App: "sabnzbd", Dir: writeFile(t, sourceINI), Plan: p}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, dir string) *sabconf.File {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, configName))
	if err != nil {
		t.Fatal(err)
	}
	f, err := sabconf.Parse(raw)
	if err != nil {
		t.Fatalf("the rewritten file is not an INI: %v\n%s", err, raw)
	}
	return f
}

// TestRewritesThePathsAndSwitchesTheLoginOff is the whole job: the API key and
// the obfuscated server password are the app's own and stay as they are.
func TestRewritesThePathsAndSwitchesTheLoginOff(t *testing.T) {
	in := input(t, sourcePlan)
	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("the plan blocked the run: %v", rep.Uncovered)
	}
	want := map[string]string{
		"complete_dir":   "/data/downloads/complete",
		"download_dir":   "/data/downloads/incomplete",
		"script_dir":     "/config/scripts",
		"nzb_backup_dir": "/config/backup",
		"username":       "",
		"password":       "",
		"api_key":        "0123456789abcdef0123456789abcdef",
	}
	// Check wrote nothing: the file is still the source's.
	before, _ := os.ReadFile(filepath.Join(in.Dir, configName))
	if string(before) != sourceINI {
		t.Fatalf("Check wrote to the file:\n%s", before)
	}

	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f := read(t, in.Dir)
	for key, value := range want {
		if got, ok := f.Get("misc", key); !ok || got != value {
			t.Errorf("misc.%s = %q (present %v), want %q", key, got, ok, value)
		}
	}
	for _, c := range []struct{ section, want string }{
		{"categories,tv", "/data/downloads/complete/tv"},
		{"categories,4k/movies", "/data/downloads/complete/4K/Movies"},
	} {
		if got, ok := f.Get(c.section, "dir"); !ok || got != c.want {
			t.Errorf("%s/dir = %q (present %v), want %q", c.section, got, ok, c.want)
		}
	}
	if got, _ := f.Get("servers,news", "password"); got != "!!!encoded!!!616263" {
		t.Errorf("the obfuscated server password was rewritten: %q", got)
	}
	if got, _ := f.Get("servers,news", "username"); got != "person" {
		t.Errorf("the server username was rewritten: %q", got)
	}
	if !hasNote(rep.Notes, "its own login is switched off") {
		t.Errorf("notes = %v, want the login decision called out", rep.Notes)
	}
}

// TestAPathNoRuleCoversBlocks: a path the plan does not settle is a gap, not a
// guess, and nothing is written.
func TestAPathNoRuleCoversBlocks(t *testing.T) {
	ini := strings.Replace(sourceINI, "/downloads/complete\n", "/srv/elsewhere/complete\n", 1)
	in := input(t, sourcePlan)
	if err := os.WriteFile(filepath.Join(in.Dir, configName), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Blocked() {
		t.Fatalf("uncovered = %v, want the path reported", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err == nil {
		t.Fatal("Write succeeded with an uncovered path")
	}
	if got, _ := read(t, in.Dir).Get("misc", "complete_dir"); got != "/srv/elsewhere/complete" {
		t.Fatalf("a blocked run wrote %q", got)
	}
}

// TestTheFileIsOnlyTouchedWhereItMust: a second run is a no-op, and every line
// this package does not own is unchanged.
func TestTheFileIsOnlyTouchedWhereItMust(t *testing.T) {
	in := input(t, sourcePlan)
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(in.Dir, configName))
	for _, keep := range []string{
		"host = 0.0.0.0",
		"port = 8080",
		"nzb_key = abcdef",
		"[[4k/movies]]\norder = 2\n",
		"[[news]]\nhost = news.example.com\nport = 563\nusername = person\npassword = !!!encoded!!!616263\n",
	} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("the rewrite lost %q:\n%s", keep, raw)
		}
	}
	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Changes) != 0 {
		t.Fatalf("a second check reports changes: %+v", rep.Changes)
	}
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}
