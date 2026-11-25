package bazconf

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const testKey = "0123456789abcdef0123456789abcdef"

// realFile is what the pinned image writes after a first start, with the two
// keys this package owns changed to the test values. Only the shape matters:
// two-space indentation, one level of mapping per section, keys sorted.
const realFile = `---
auth:
  apikey: ` + testKey + `
  password: ''
  type: null
  username: ''
general:
  base_url: /bazarr
  branch: master
  instance_name: Bazarr
  port: 6767
  use_radarr: false
  use_sonarr: false
sonarr:
  apikey: something
  base_url: /
  ip: 127.0.0.1
  port: 8989
`

// decoded reads the document back the way the app's own loader does, so a test
// proves values rather than bytes.
func decoded(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal(doc, &out); err != nil {
		t.Fatalf("the result is not YAML: %v\n%s", err, doc)
	}
	return out
}

func sect(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	sec, ok := doc[name].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping in %v", name, doc)
	}
	return sec
}

func TestSealWritesBothKeysIntoAnEmptyFile(t *testing.T) {
	out, changed, err := Seal(nil, testKey, "/bazarr")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed on an absent file")
	}
	doc := decoded(t, out)
	if got := sect(t, doc, "general")["base_url"]; got != "/bazarr" {
		t.Fatalf("general.base_url = %v, want the chart's prefix", got)
	}
	if got := sect(t, doc, "auth")["apikey"]; got != testKey {
		t.Fatalf("auth.apikey = %v, want the seeded key", got)
	}
}

// TestSealIsANoOpOnAFileTheAppWrote is the case that matters on every start
// after the first: the app rewrote the whole file from its own memory, and the
// init container must not touch it again. The file starts with a YAML document
// marker, which is what the app's own writer emits.
func TestSealIsANoOpOnAFileTheAppWrote(t *testing.T) {
	out, changed, err := Seal([]byte(realFile), testKey, "/bazarr")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("re-sealing an already sealed file reported a change:\n%s", out)
	}
	if string(out) != realFile {
		t.Fatal("an unchanged seal must return the original bytes untouched")
	}
}

// TestSealRepairsWhatTheAppOrAPersonChanged: base_url is not on a settings page
// but a restored or hand-edited config can carry another one, and the API key is
// box's, so both are put back the way the Job and the Gateway expect.
func TestSealRepairsBothKeysAndKeepsEveryOtherSetting(t *testing.T) {
	existing := strings.NewReplacer(
		"  apikey: "+testKey, "  apikey: someone-elses-key",
		"  base_url: /bazarr", "  base_url: /",
	).Replace(realFile)
	out, changed, err := Seal([]byte(existing), testKey, "/bazarr")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed: both owned keys differ")
	}
	doc := decoded(t, out)
	if got := sect(t, doc, "general")["base_url"]; got != "/bazarr" {
		t.Fatalf("general.base_url = %v", got)
	}
	if got := sect(t, doc, "auth")["apikey"]; got != testKey {
		t.Fatalf("auth.apikey = %v", got)
	}
	// Everything else is the app's: a value it wrote, a key it wrote, and a
	// section this package does not own.
	if got := sect(t, doc, "auth")["type"]; got != nil {
		t.Fatalf("auth.type = %v, want the app's null left alone", got)
	}
	if got := sect(t, doc, "general")["instance_name"]; got != "Bazarr" {
		t.Fatalf("general.instance_name = %v", got)
	}
	if got := sect(t, doc, "sonarr")["ip"]; got != "127.0.0.1" {
		t.Fatalf("sonarr.ip = %v", got)
	}
	// ... and only the two owned lines changed: the rest is byte-identical.
	if before, after := strings.Split(existing, "\n"), strings.Split(string(out), "\n"); len(before) != len(after) {
		t.Fatalf("line count changed: %d -> %d", len(before), len(after))
	}
}

// TestSealKeepsTheDocumentMarkerTheAppWrote: a restart must not fail on the file
// it wrote last time. The marker is kept, not stripped: the app's own loader
// writes one, and a file the init container trimmed would be the one difference
// between its output and the app's.
func TestSealKeepsTheDocumentMarker(t *testing.T) {
	existing := []byte(strings.ReplaceAll(realFile, "  base_url: /bazarr", "  base_url: /somewhere-else"))
	out, changed, err := Seal(existing, testKey, "/bazarr")
	if err != nil {
		t.Fatalf("a file the app wrote was refused: %v", err)
	}
	if !changed {
		t.Fatal("want changed: base_url differs")
	}
	if !strings.HasPrefix(string(out), "---\n") {
		t.Fatalf("the document marker was dropped:\n%s", out)
	}
	if got := sect(t, decoded(t, out), "general")["base_url"]; got != "/bazarr" {
		t.Fatalf("general.base_url = %v", got)
	}
}

// TestSealRefusesASecondDocument: one marker starts the document, a second one
// starts another file's worth of settings, and the app would read only one.
func TestSealRefusesASecondDocument(t *testing.T) {
	existing := []byte(realFile + "---\nsonarr:\n  ip: other\n")
	if _, _, err := Seal(existing, testKey, "/bazarr"); err == nil {
		t.Fatal("want a refusal for a second YAML document")
	}
}

// TestSealAddsAMissingSection: a config the app wrote before a key existed, or
// a hand-written two-line file, still gets the section it needs.
func TestSealAddsAMissingSection(t *testing.T) {
	existing := []byte("general:\n  branch: master\n")
	out, changed, err := Seal(existing, testKey, "/bazarr")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed: auth is absent")
	}
	doc := decoded(t, out)
	if got := sect(t, doc, "auth")["apikey"]; got != testKey {
		t.Fatalf("auth.apikey = %v", got)
	}
	if got := sect(t, doc, "general")["base_url"]; got != "/bazarr" {
		t.Fatalf("general.base_url = %v", got)
	}
	if got := sect(t, doc, "general")["branch"]; got != "master" {
		t.Fatalf("general.branch = %v, want the existing key kept", got)
	}
}

// TestSealQuotesAValueYAMLWouldOtherwiseRead: a prefix with a leading symbol is
// a comment or a tag to a YAML reader, so the value is always quoted.
func TestSealQuotesAValueYAMLWouldOtherwiseRead(t *testing.T) {
	out, _, err := Seal(nil, "key: with # markers", "/bazarr")
	if err != nil {
		t.Fatal(err)
	}
	doc := decoded(t, out)
	if got := sect(t, doc, "auth")["apikey"]; got != "key: with # markers" {
		t.Fatalf("auth.apikey = %q", got)
	}
}
