package hostconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Root edits the template by hand, so its bytes are documentation and must not
// drift with a refactor.
func TestTemplateIsTheDecidedFile(t *testing.T) {
	want := `# box's settings for this host. box never rewrites this file; edit it by hand.
update:
  timer: on        # off: no update checks and no health notices
  interval: 1h     # how often the timer checks for a newer release; 15m at least
  hold-box: false  # true: take no new box, the same as --hold-box on every run
notices:
  url: ""          # box POSTs a JSON notice here after updates and on health changes
`
	if got := string(Template()); got != want {
		t.Fatalf("Template() is not the decided file:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The file box writes and the values box reads are one thing.
	cfg, err := parse("box.yaml", Template())
	if err != nil {
		t.Fatalf("the template does not parse: %v", err)
	}
	if cfg != Defaults() {
		t.Fatalf("the template parses to %+v, want the defaults %+v", cfg, Defaults())
	}
}

// --record moves both files, so a box's whole state is one directory.
func TestPathIsBesideTheRecord(t *testing.T) {
	for _, c := range []struct{ record, want string }{
		{"/etc/s5n/cluster.yaml", "/etc/s5n/box.yaml"},
		{"/tmp/run/cluster.yaml", "/tmp/run/box.yaml"},
		{"cluster.yaml", "box.yaml"},
	} {
		if got := Path(c.record); got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.record, got, c.want)
		}
	}
}

func TestLoadMissingFileIsTheDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, exists, err := Load(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if exists {
		t.Fatal("Load says a file exists that does not")
	}
	if cfg != Defaults() {
		t.Fatalf("Load = %+v, want the defaults %+v", cfg, Defaults())
	}
}

// Each of the four keys decides something, so each is read back exactly.
func TestLoadReadsEveryKey(t *testing.T) {
	file := write(t, `update:
  timer: off
  interval: 30m
  hold-box: true
notices:
  url: "https://hooks.example.test/services/T00/B00/token?x=1"
`)
	cfg, exists, err := Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !exists {
		t.Fatal("Load says no file exists")
	}
	want := Config{Timer: false, Interval: 30 * time.Minute, HoldBox: true,
		NoticeURL: "https://hooks.example.test/services/T00/B00/token?x=1"}
	if cfg != want {
		t.Fatalf("Load = %+v, want %+v", cfg, want)
	}
}

// One that sets only the address is not a file that turned the timer off.
func TestLoadAcceptsAShorterFileAndKeepsTheDefaults(t *testing.T) {
	cfg, _, err := Load(write(t, "notices:\n  url: https://hooks.example.test/x\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NoticeURL != "https://hooks.example.test/x" {
		t.Errorf("NoticeURL = %q", cfg.NoticeURL)
	}
	if !cfg.Timer || cfg.Interval != time.Hour || cfg.HoldBox {
		t.Errorf("the keys the file does not set did not keep their defaults: %+v", cfg)
	}
}

// The file is hand-written, so the shapes a person actually writes are accepted.
func TestParseAcceptsCommentsBlanksQuotesAndBothBooleanSpellings(t *testing.T) {
	cfg, err := parse("box.yaml", []byte(`
# the timer
update:
  timer: 'true'      # quoted, and spelled the other way

  interval: "90m"    # quoted
  hold-box: off
notices:
  url: ''            # nothing
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Config{Timer: true, Interval: 90 * time.Minute, HoldBox: false, NoticeURL: ""}
	if cfg != want {
		t.Fatalf("parse = %+v, want %+v", cfg, want)
	}
}

// Every message names file, line and key, so an operator finds the typo without
// reading box's source.
func TestParseRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for _, c := range []struct{ name, data, want string }{
		{
			"unknown top-level key",
			"update:\n  timer: on\nupdates:\n  timer: on\n",
			"box.yaml:3: unknown key \"updates\"",
		},
		{
			"unknown key under a section",
			"update:\n  timer: on\n  interval: 1h\n  jitter: 5m\n",
			"box.yaml:4: unknown key \"update.jitter\"",
		},
		{
			"a leaf at the top level",
			"timer: on\n",
			"box.yaml:1: unknown key \"timer\"",
		},
		{
			"a section with a value",
			"update: yes\n",
			"box.yaml:1: update takes no value",
		},
		{
			"an indented line before a section",
			"  timer: on\n",
			"box.yaml:1: unexpected indented line \"timer: on\"",
		},
		{
			"a line with no colon",
			"update:\n  timer on\n",
			"box.yaml:2: malformed line \"timer on\"",
		},
		{
			"a bad switch",
			"update:\n  timer: sometimes\n",
			"box.yaml:2: update.timer \"sometimes\" is not on or off",
		},
		{
			"a bad duration",
			"update:\n  interval: soon\n",
			"box.yaml:2: update.interval \"soon\" is not a duration such as 1h or 30m",
		},
		{
			"an interval under the floor",
			"update:\n  interval: 5m\n",
			"box.yaml:2: update.interval \"5m\" is shorter than 15m0s",
		},
		{
			"an address that is not absolute",
			"notices:\n  url: hooks.example.test/x\n",
			"box.yaml:2: notices.url \"hooks.example.test/x\" is not an http or https URL",
		},
		{
			"an address that is not http",
			"notices:\n  url: \"ftp://hooks.example.test/x\"\n",
			"is not an http or https URL",
		},
		{
			"a key set twice",
			"update:\n  timer: on\n  timer: off\n",
			"box.yaml:3: update.timer is set twice",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse("box.yaml", []byte(c.data))
			if err == nil {
				t.Fatalf("parse accepted:\n%s", c.data)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("parse error = %q, want it to contain %q", err, c.want)
			}
		})
	}
}

// The defaults go on a host that has none, and a file root edited is never
// touched again.
func TestEnsureWritesOnlyAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	wrote, err := Ensure(path)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !wrote {
		t.Fatal("Ensure did not write a missing file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600: the notice URL can carry a token", info.Mode().Perm())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(Template()) {
		t.Fatalf("the file is not the template (err %v)", err)
	}

	// Root edits it: a key, and a comment of their own.
	edited := strings.Replace(string(Template()), "interval: 1h", "interval: 6h", 1) + "# mine\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err = Ensure(path)
	if err != nil {
		t.Fatalf("Ensure on an existing file: %v", err)
	}
	if wrote {
		t.Fatal("Ensure rewrote a file that exists")
	}
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatalf("the existing file changed:\n%s", got)
	}
}

// init writes the answer into root's file, so every other byte is kept.
func TestSetNoticeURLSplicesOneLine(t *testing.T) {
	url := "https://hooks.example.test/services/T00/B00/abc"
	got, err := SetNoticeURL(Template(), url)
	if err != nil {
		t.Fatalf("SetNoticeURL: %v", err)
	}
	// The one line changed, and it kept its comment.
	wantLine := `  url: "https://hooks.example.test/services/T00/B00/abc"  # box POSTs a JSON notice here after updates and on health changes`
	if !strings.Contains(string(got), wantLine) {
		t.Fatalf("the url line is not %q:\n%s", wantLine, got)
	}
	// Every other line is byte-identical.
	before, after := strings.Split(string(Template()), "\n"), strings.Split(string(got), "\n")
	if len(before) != len(after) {
		t.Fatalf("the file gained or lost a line: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if strings.HasPrefix(strings.TrimSpace(before[i]), "url:") {
			continue
		}
		if before[i] != after[i] {
			t.Errorf("line %d changed: %q -> %q", i+1, before[i], after[i])
		}
	}
	// And the file it wrote is a file box reads back as that address.
	cfg, _, err := Load(write(t, string(got)))
	if err != nil {
		t.Fatalf("Load after SetNoticeURL: %v", err)
	}
	if cfg.NoticeURL != url {
		t.Fatalf("NoticeURL = %q, want %q", cfg.NoticeURL, url)
	}
}

func TestSetNoticeURLNeedsAURLline(t *testing.T) {
	if _, err := SetNoticeURL([]byte("update:\n  timer: on\n"), "https://hooks.example.test/x"); err == nil {
		t.Fatal("SetNoticeURL wrote an address into a file with no url line")
	}
}

// A notice address carries its own token, so only the host may be printed.
func TestRedact(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"https://hooks.example.test/services/T00/B00/secret?token=abc", "https://hooks.example.test"},
		{"http://10.0.0.2:8080/hook", "http://10.0.0.2:8080"},
		{"not a url", ""},
		{"/just/a/path", ""},
	} {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// write puts data in a temp file and returns its path.
func write(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
