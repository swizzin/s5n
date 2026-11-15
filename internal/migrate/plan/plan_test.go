package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourcePlan is a plan for an install whose Arrs saw the library under
// /library and whose torrent client saw the same files as /downloads, which is
// the case the table exists for. Every path is invented.
const sourcePlan = `version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /downloads
        to: /data/downloads
      - from: /downloads/Series
        to: /data/media/Series
  sonarr-4k:
    source: sonarr4k
    config: /srv/apps/sonarr4k
    paths:
      - from: /library/Downloads
        to: /data/downloads
      - from: /library/media
        to: /data/media
`

func mustParse(t *testing.T, src string) *Plan {
	t.Helper()
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestRewrite(t *testing.T) {
	p := mustParse(t, sourcePlan)
	root := mustParse(t, `version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /
        to: /data
`)
	ontoRoot := mustParse(t, `version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /downloads
        to: /
`)

	tests := []struct {
		name string
		plan *Plan
		app  string
		in   string
		want string
		ok   bool
	}{
		{"exact match maps to the rule's to", p, "qbittorrent", "/downloads", "/data/downloads", true},
		{"a path below the rule keeps its remainder", p, "qbittorrent", "/downloads/Movies/a.mkv", "/data/downloads/Movies/a.mkv", true},
		{"a longer prefix wins", p, "qbittorrent", "/downloads/Series/Show/e01.mkv", "/data/media/Series/Show/e01.mkv", true},
		{"a prefix that is not a whole segment does not match", p, "qbittorrent", "/downloads2/a.mkv", "/downloads2/a.mkv", false},
		{"a longer name sharing the prefix does not match", p, "qbittorrent", "/downloads-old/a.mkv", "/downloads-old/a.mkv", false},
		{"a trailing slash survives", p, "qbittorrent", "/downloads/", "/data/downloads/", true},
		{"a trailing slash survives a longer rule", p, "qbittorrent", "/downloads/Series/", "/data/media/Series/", true},
		{"the remainder is not cleaned", p, "qbittorrent", "/downloads//Movies//a.mkv", "/data/downloads//Movies//a.mkv", true},
		{"a relative path follows its root", p, "qbittorrent", "Movies/a.mkv", "Movies/a.mkv", true},
		{"an empty path stays empty", p, "qbittorrent", "", "", true},
		{"an absolute path with no rule is a gap", p, "qbittorrent", "/mnt/other/a.mkv", "/mnt/other/a.mkv", false},
		{"an app's table is its own", p, "sonarr-4k", "/downloads/Movies/a.mkv", "/downloads/Movies/a.mkv", false},
		{"another app's rule applies to its own paths", p, "sonarr-4k", "/library/Downloads/Movies/a.mkv", "/data/downloads/Movies/a.mkv", true},
		{"an unknown app covers nothing absolute", p, "radarr", "/data/downloads/a.mkv", "/data/downloads/a.mkv", false},
		{"an unknown app passes a relative path through", p, "radarr", "a.mkv", "a.mkv", true},
		{"a rule on the filesystem root covers everything", root, "qbittorrent", "/srv/a.mkv", "/data/srv/a.mkv", true},
		{"a rule on the root maps the root itself", root, "qbittorrent", "/", "/data", true},
		{"a rule on the root keeps a trailing slash", root, "qbittorrent", "/srv/", "/data/srv/", true},
		{"a rule onto the root does not double the separator", ontoRoot, "qbittorrent", "/downloads/a.mkv", "/a.mkv", true},
		{"a rule onto the root maps the directory itself", ontoRoot, "qbittorrent", "/downloads", "/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.plan.Rewrite(tt.app, tt.in)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("Rewrite(%q, %q) = (%q, %v), want (%q, %v)", tt.app, tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseCleansPaths(t *testing.T) {
	p := mustParse(t, `version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /downloads/
        to: /data//downloads
      - from: /downloads/Series/../Series
        to: /data/media/
`)
	paths := p.Apps["qbittorrent"].Paths
	if got, want := paths[0].From, "/downloads"; got != want {
		t.Errorf("from = %q, want %q", got, want)
	}
	if got, want := paths[0].To, "/data/downloads"; got != want {
		t.Errorf("to = %q, want %q", got, want)
	}
	if got, want := paths[1].From, "/downloads/Series"; got != want {
		t.Errorf("from = %q, want %q", got, want)
	}
	if got, want := paths[1].To, "/data/media"; got != want {
		t.Errorf("to = %q, want %q", got, want)
	}
}

func TestParseReadsTheDocumentedShape(t *testing.T) {
	p := mustParse(t, sourcePlan)
	if p.Version != Version {
		t.Errorf("version = %d, want %d", p.Version, Version)
	}
	app, ok := p.Apps["sonarr-4k"]
	if !ok {
		t.Fatalf("apps = %v, want an entry for sonarr-4k", p.appNames())
	}
	if app.Source != "sonarr4k" || app.Config != "/srv/apps/sonarr4k" {
		t.Errorf("app = %+v, want the source name and config directory from the file", app)
	}
	if len(app.Paths) != 2 {
		t.Errorf("paths = %v, want the two rules from the file", app.Paths)
	}
}

func TestParseRefusals(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "an unknown version",
			src:  "version: 2\napps: {}\n",
			want: "version 2: this box reads version 1",
		},
		{
			name: "a missing version",
			src:  "apps: {}\n",
			want: "version 0: this box reads version 1",
		},
		{
			name: "an unknown key at the top",
			src:  "version: 1\napps: {}\nports: {}\n",
			want: "ports",
		},
		{
			name: "an unknown key in an app",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    path: []\n",
			want: "path",
		},
		{
			name: "an unknown key in a rule",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: /a\n        to: /b\n        prefix: true\n",
			want: "prefix",
		},
		{
			name: "a relative from",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: downloads\n        to: /data/downloads\n",
			want: `apps.q: from "downloads" is not an absolute path`,
		},
		{
			name: "a relative to",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: /downloads\n        to: data/downloads\n",
			want: `apps.q: to "data/downloads" is not an absolute path`,
		},
		{
			name: "an empty from",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: \"\"\n        to: /data/downloads\n",
			want: `apps.q: from "" is not an absolute path`,
		},
		{
			name: "the same from twice",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: /downloads\n        to: /a\n      - from: /downloads\n        to: /b\n",
			want: `apps.q: from "/downloads" appears twice`,
		},
		{
			name: "the same directory spelled twice",
			src:  "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n    paths:\n      - from: /downloads\n        to: /a\n      - from: /downloads/\n        to: /b\n",
			want: `apps.q: from "/downloads" appears twice`,
		},
		{
			name: "an empty file",
			src:  "",
			want: "is empty",
		},
		{
			name: "a second document",
			src:  "version: 1\napps: {}\n---\nversion: 1\n",
			want: "more than one YAML document",
		},
		{
			name: "apps that are not a map",
			src:  "version: 1\napps: []\n",
			want: "cannot unmarshal !!seq",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("Parse(%q) accepted a plan it should refuse", tt.src)
			}
			var perr *Error
			if !errors.As(err, &perr) {
				t.Fatalf("error is %T, want *plan.Error: %v", err, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestParseNamesEveryProblemAtOnce(t *testing.T) {
	_, err := Parse([]byte(`version: 2
apps:
  q:
    source: q
    config: /srv/q
    paths:
      - from: downloads
        to: data/downloads
`))
	if err == nil {
		t.Fatal("Parse accepted a plan with three problems")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("error is %T, want *plan.Error: %v", err, err)
	}
	if len(perr.Problems) != 3 {
		t.Fatalf("problems = %v, want the version and both relative paths", perr.Problems)
	}
	for _, want := range []string{"version 2", `from "downloads"`, `to "data/downloads"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(name, []byte("version: 3\napps: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(name)
	if err == nil {
		t.Fatal("Load accepted version 3")
	}
	if !strings.Contains(err.Error(), "plan "+name+": version 3") {
		t.Fatalf("error = %q, want it to name the file and the version", err)
	}
}

func TestLoadReadsAFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(name, []byte(sourcePlan), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := p.Apps["qbittorrent"]; !ok {
		t.Errorf("apps = %v, want qbittorrent", p.appNames())
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read plan") {
		t.Fatalf("error = %v, want a read failure", err)
	}
}
