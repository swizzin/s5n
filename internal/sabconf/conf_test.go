package sabconf

import (
	"strings"
	"testing"
)

// testSettings is the chart's shape: /sabnzbd on the box's own name, downloads
// on the data claim, the release Secret's key, and the Gateway trusted.
var testSettings = Settings{
	URLBase:     "/sabnzbd",
	CompleteDir: "/data/downloads",
	DownloadDir: "/data/downloads/incomplete",
	APIKey:      "0123456789abcdef0123456789abcdef",
	VerifyXFF:   false,
}

// imageDefaults is the shape the pinned image copies out of /defaults on a
// first start: top-level ConfigObj keys, [misc] with its settings, then the
// notification sections. Only the keys this package owns matter to the test;
// the rest are here so a rewrite can be proven to leave them alone.
const imageDefaults = `__version__ = 19
__encoding__ = utf-8
[misc]
host = 0.0.0.0
port = 8080
username = ""
password = ""
inet_exposure = 0
local_ranges = ,
api_key = ""
nzb_key = ""
download_dir = Downloads/incomplete
complete_dir = Downloads/complete
url_base = /sabnzbd
host_whitelist = buildhost,
[logging]
log_level = 1
max_log_size = 5242880
`

func TestSealWritesEveryOwnedKeyIntoAnEmptyFile(t *testing.T) {
	out, changed, err := Seal(nil, testSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed on an absent file")
	}
	f, err := Parse(out)
	if err != nil {
		t.Fatalf("the result is not an INI file: %v\n%s", err, out)
	}
	for key, want := range map[string]string{
		"url_base":          "/sabnzbd",
		"complete_dir":      "/data/downloads",
		"download_dir":      "/data/downloads/incomplete",
		"api_key":           testSettings.APIKey,
		"verify_xff_header": "0",
	} {
		if got, ok := f.Get("misc", key); !ok || got != want {
			t.Errorf("misc.%s = %q (present %v), want %q", key, got, ok, want)
		}
	}
}

// TestSealIsANoOpOnAFileTheAppWrote is the case that matters on every start
// after the first: the app rewrote the file from its own memory and the init
// container must not touch it again.
func TestSealIsANoOpOnAFileTheAppWrote(t *testing.T) {
	app := strings.NewReplacer(
		`api_key = ""`, `api_key = `+testSettings.APIKey,
		`download_dir = Downloads/incomplete`, `download_dir = /data/downloads/incomplete`,
		`complete_dir = Downloads/complete`, `complete_dir = /data/downloads`,
		`inet_exposure = 0`, "inet_exposure = 0\nverify_xff_header = 0",
	).Replace(imageDefaults)
	out, changed, err := Seal([]byte(app), testSettings)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("a file that already holds the settings was rewritten:\n%s", out)
	}
	if string(out) != app {
		t.Fatal("a no-op changed the bytes")
	}
}

// TestSealRepairsEveryDriftedValue: a source's file or an older image's defaults
// are the download paths on the config claim and XFF verification on, which is
// a copy instead of a hardlink and a 403 through the Gateway.
func TestSealRepairsEveryDriftedValue(t *testing.T) {
	out, changed, err := Seal([]byte(imageDefaults), testSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed on the image's defaults")
	}
	f, err := Parse(out)
	if err != nil {
		t.Fatalf("the result is not an INI file: %v\n%s", err, out)
	}
	for key, want := range map[string]string{
		"download_dir":      "/data/downloads/incomplete",
		"complete_dir":      "/data/downloads",
		"api_key":           testSettings.APIKey,
		"verify_xff_header": "0",
	} {
		if got, ok := f.Get("misc", key); !ok || got != want {
			t.Errorf("misc.%s = %q (present %v), want %q", key, got, ok, want)
		}
	}
	// Every key this package does not own is the byte it was.
	for _, keep := range []string{"host = 0.0.0.0", "port = 8080", "inet_exposure = 0", "nzb_key = \"\"", "host_whitelist = buildhost,"} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("the rewrite lost %q:\n%s", keep, out)
		}
	}
	if !strings.Contains(string(out), "[logging]\nlog_level = 1\nmax_log_size = 5242880\n") {
		t.Errorf("the section after [misc] was changed:\n%s", out)
	}
}

// TestSealAddsAMissingKeyInsideItsOwnSection: verify_xff_header is not in the
// image's defaults at all, so a first start has to gain the key inside [misc].
// Appending it at the end of the file would put it in the last section, where
// SABnzbd reads it as a notification setting and ignores it.
func TestSealAddsAMissingKeyInsideItsOwnSection(t *testing.T) {
	out, _, err := Seal([]byte(imageDefaults), testSettings)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := f.Get("misc", "verify_xff_header"); !ok || got != "0" {
		t.Fatalf("misc.verify_xff_header = %q (present %v), want 0", got, ok)
	}
	if got, ok := f.Get("logging", "verify_xff_header"); ok {
		t.Fatalf("verify_xff_header was written into [logging] as %q", got)
	}
	// The section header still directly precedes its first key.
	if !strings.Contains(string(out), "[misc]\nhost = 0.0.0.0") {
		t.Fatalf("the [misc] section was disturbed:\n%s", out)
	}
}

func TestSealRefusesASectionDeclaredTwice(t *testing.T) {
	file := "[misc]\nurl_base = /sabnzbd\n[misc]\nurl_base = /other\n"
	_, _, err := Seal([]byte(file), testSettings)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("err = %v, want a refusal naming the duplicate section", err)
	}
}

func TestSealRefusesAKeySetTwice(t *testing.T) {
	file := "[misc]\nurl_base = /sabnzbd\nurl_base = /other\n"
	_, _, err := Seal([]byte(file), testSettings)
	if err == nil || !strings.Contains(err.Error(), "set twice") {
		t.Fatalf("err = %v, want a refusal naming the duplicate key", err)
	}
}

func TestSealRefusesAFileThatIsNotSabnzbd(t *testing.T) {
	_, _, err := Seal([]byte("hello\n"), testSettings)
	if err == nil {
		t.Fatal("want a refusal for a file with no keys and no [misc]")
	}
}

func TestFileKeepsTheSpellingAndSpacingOfALineItEdits(t *testing.T) {
	file := "[misc]\nurl_base   =   /old\n"
	f, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set("misc", "url_base", "/sabnzbd"); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Bytes()); got != "[misc]\nurl_base   =   /sabnzbd\n" {
		t.Fatalf("the line was reformatted: %q", got)
	}
}

// TestNestedSectionsAreReadAndLeftAlone: SABnzbd writes every category and
// server as a ConfigObj subsection ([[tv]] under [categories]), and the sealer
// must not mistake one for a top-level section or write into it.
func TestNestedSectionsAreReadAndLeftAlone(t *testing.T) {
	file := `[misc]
complete_dir = Downloads/complete
[categories]
[[*]]
order = 0
[[4k/movies]]
order = 1
dir = 4K/Movies
[servers]
[[news]]
host = news.example.com
port = 563
`
	f, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Children("categories"); len(got) != 2 || got[0] != "*" || got[1] != "4k/movies" {
		t.Fatalf("categories = %v, want the two subsections in order", got)
	}
	if got, ok := f.Get("categories,4k/movies", "dir"); !ok || got != "4K/Movies" {
		t.Fatalf("categories,4k/movies.dir = %q (present %v)", got, ok)
	}
	out, changed, err := Seal([]byte(file), testSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want the misc paths sealed")
	}
	if !strings.Contains(string(out), "[[4k/movies]]\norder = 1\ndir = 4K/Movies\n") {
		t.Fatalf("the nested sections were disturbed:\n%s", out)
	}
	if got, _ := Parse(out); func() string { v, _ := got.Get("servers,news", "host"); return v }() != "news.example.com" {
		t.Fatalf("the server subsection was lost:\n%s", out)
	}
}
