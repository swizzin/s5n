package seedbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockDiff trims the unchanged prefix and suffix, so an assertion is about the
// only region that moved: that is what "box changes one key" has to mean.
func blockDiff(before, after []byte) (oldBlock, newBlock []string) {
	a := strings.Split(string(before), "\n")
	b := strings.Split(string(after), "\n")
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	j := 0
	for j < len(a)-i && j < len(b)-i && a[len(a)-1-j] == b[len(b)-1-j] {
		j++
	}
	return a[i : len(a)-j], b[i : len(b)-j]
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	for _, name := range []string{"values.yaml", "values-terse.yaml", "grant.yaml"} {
		t.Run(name, func(t *testing.T) {
			path := copyFixture(t, name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			s, err := Load(fixture(t, "grant.yaml"), path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var doc *Doc
			if name == "grant.yaml" {
				doc = s.Grant
			} else {
				doc = s.Values
			}
			e := doc.Editor()
			if err := e.Save(); err != nil {
				t.Fatalf("Save: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("an unedited document changed on disk:\n--- before\n%s\n--- after\n%s", before, after)
			}
			if string(e.Bytes()) != string(before) {
				t.Error("the editor's bytes differ from the file it read")
			}
			// And the writer itself adds nothing: the same bytes through the
			// atomic path come back byte for byte.
			out := filepath.Join(t.TempDir(), "copy.yaml")
			if err := writeAtomic(out, before, 0o600, os.Rename); err != nil {
				t.Fatalf("writeAtomic: %v", err)
			}
			if got, err := os.ReadFile(out); err != nil || string(got) != string(before) {
				t.Errorf("the atomic write changed the bytes (%v)", err)
			}
		})
	}
}

func TestSaveWithoutEditsDoesNotRewrite(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	old := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Values.Editor().Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Error("an edit-free Save rewrote the file, so its mtime no longer means the user changed it")
	}
}

func TestSetChangesOnlyItsLine(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		path    string
		value   any
		want    string
	}{
		{
			name:    "a quoted string keeps its quotes and its comment",
			fixture: "values-terse.yaml",
			path:    "global.tz",
			value:   "Etc/GMT",
			want:    "tz: 'Etc/GMT'    # single quotes matter",
		},
		{
			name:    "a plain string stays plain",
			fixture: "values-terse.yaml",
			path:    "apps.sonarr.env.SONARR__LOG__LEVEL",
			value:   "info",
			want:    "SONARR__LOG__LEVEL: info",
		},
		{
			name:    "an integer",
			fixture: "values.yaml",
			path:    "wiring.timeoutSeconds",
			value:   900,
			want:    "timeoutSeconds: 900",
		},
		{
			name:    "a boolean",
			fixture: "values.yaml",
			path:    "wiring.enabled",
			value:   false,
			want:    "enabled: false",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := copyFixture(t, c.fixture)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			s, err := Load(fixture(t, "grant.yaml"), path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := s.SetValue(c.path, c.value); err != nil {
				t.Fatalf("SetValue: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			oldBlock, newBlock := blockDiff(before, after)
			if len(oldBlock) != 1 || len(newBlock) != 1 {
				t.Fatalf("changed %d lines into %d, want exactly one line replaced:\n%s", len(oldBlock), len(newBlock), after)
			}
			if strings.TrimSpace(newBlock[0]) != c.want {
				t.Errorf("new line = %q, want %q", newBlock[0], c.want)
			}
		})
	}
}

func TestSetChangesAScalarInsideAFlowMapping(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("apps.sonarr.resources.limits.memory", "3Gi"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(oldBlock) != 1 || len(newBlock) != 1 {
		t.Fatalf("changed %d lines into %d, want one:\n%s", len(oldBlock), len(newBlock), after)
	}
	if strings.TrimSpace(newBlock[0]) != "resources: {limits: {memory: 3Gi}}" {
		t.Errorf("new line = %q", newBlock[0])
	}
}

func TestSetAppendsAtTheEndOfAMapping(t *testing.T) {
	path := copyFixture(t, "values-terse.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("apps.plex.rewritePath", true); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(oldBlock) != 0 {
		t.Fatalf("an append replaced %v", oldBlock)
	}
	if len(newBlock) != 1 || newBlock[0] != "    rewritePath: true" {
		t.Fatalf("added %q, want one line under apps.plex at its own indent", newBlock)
	}
}

func TestSetCreatesNestedKeys(t *testing.T) {
	path := copyFixture(t, "values-terse.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("apps.plex.env.PLEX_UID", "1001"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	after, _ := os.ReadFile(path)
	_, newBlock := blockDiff(before, after)
	want := []string{"    env:", "      PLEX_UID: \"1001\""}
	if strings.Join(newBlock, "\n") != strings.Join(want, "\n") {
		t.Fatalf("added:\n%s\nwant:\n%s", strings.Join(newBlock, "\n"), strings.Join(want, "\n"))
	}
	// The file must still parse, and the new key must be where the path says.
	s2, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	n, ok, _ := s2.Values.lookup("apps.plex.env.PLEX_UID")
	if !ok || n.Value != "1001" {
		t.Errorf("apps.plex.env.PLEX_UID = %v, want 1001", n)
	}
}

func TestSetUnderAKeyWithNothingBeneathIt(t *testing.T) {
	path := writeTemp(t, "values.yaml", "apps:\n  plex:\n  sonarr:\n    env: {}\n")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("apps.plex.env.PLEX_UID", "1001"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "apps:\n  plex:\n    env:\n      PLEX_UID: \"1001\"\n  sonarr:\n    env: {}\n"
	if string(got) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetOneListEntry(t *testing.T) {
	path := copyFixture(t, "grant.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(path, fixture(t, "values.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetGrant("apps.sonarr.instances[1].enabled", false); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(oldBlock) != 1 || len(newBlock) != 1 {
		t.Fatalf("changed %d -> %d lines, want one", len(oldBlock), len(newBlock))
	}
	if strings.TrimSpace(oldBlock[0]) != "enabled: true" || strings.TrimSpace(newBlock[0]) != "enabled: false" {
		t.Errorf("changed %q to %q", oldBlock[0], newBlock[0])
	}
	merged, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	inst := merged["apps"].(map[string]any)["sonarr"].(map[string]any)["instances"].([]any)
	if inst[1].(map[string]any)["enabled"] != false {
		t.Errorf("instances[1] = %v, want enabled false", inst[1])
	}
}

func TestSetAppendsAListEntry(t *testing.T) {
	path := copyFixture(t, "grant.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(path, fixture(t, "values.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry := map[string]any{"suffix": "1080", "category": "movies-1080", "enabled": true}
	if err := s.SetGrant("apps.radarr.instances[2]", entry); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(oldBlock) != 0 || len(newBlock) != 3 {
		t.Fatalf("added %q (replacing %q), want one three-line entry", newBlock, oldBlock)
	}
	// Keys come out in sorted order: the value is a Go map, which has none.
	want := []string{"      - category: movies-1080", "        enabled: true", "        suffix: \"1080\""}
	if strings.Join(newBlock, "\n") != strings.Join(want, "\n") {
		t.Fatalf("added:\n%s\nwant:\n%s", strings.Join(newBlock, "\n"), strings.Join(want, "\n"))
	}
	merged, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	inst := merged["apps"].(map[string]any)["radarr"].(map[string]any)["instances"].([]any)
	if len(inst) != 3 {
		t.Fatalf("instances = %v, want three", inst)
	}
	if inst[2].(map[string]any)["category"] != "movies-1080" {
		t.Errorf("instances[2] = %v", inst[2])
	}
}

func TestDelete(t *testing.T) {
	path := copyFixture(t, "values-terse.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.DeleteValue("apps.sonarr.env"); err != nil {
		t.Fatalf("DeleteValue: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(newBlock) != 0 {
		t.Errorf("delete added %q", newBlock)
	}
	if len(oldBlock) != 2 {
		t.Errorf("removed %q, want the key and its one child", oldBlock)
	}
	if strings.Contains(string(after), "SONARR__LOG__LEVEL") {
		t.Error("the child survived the delete")
	}
	if !strings.Contains(string(after), "resources:") {
		t.Error("the delete took a neighbour with it")
	}
}

func TestDeleteTakesItsComment(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.DeleteValue("apps.qbittorrent.image.tag"); err != nil {
		t.Fatalf("DeleteValue: %v", err)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "A pin is held") {
		t.Errorf("the key's comment was left behind:\n%s", after)
	}
	if !strings.Contains(string(after), "variant: default") {
		t.Errorf("the sibling key went too:\n%s", after)
	}
}

func TestSetRefusals(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		doc     string
		path    string
		value   any
		want    string
	}{
		{name: "a mapping in place", fixture: "values.yaml", path: "apps.sonarr.resources", value: map[string]any{}, want: "is a mapping"},
		{
			name:  "a list in place",
			doc:   "apps:\n  plex:\n    noAuthNetworks:\n      - 10.0.0.0/8\n",
			path:  "apps.plex.noAuthNetworks",
			value: []any{},
			want:  "is a list",
		},
		{name: "a key inside a flow mapping", fixture: "values.yaml", path: "apps.radarr.env.X", value: "y", want: "on one line"},
		{name: "into a scalar", fixture: "values.yaml", path: "apps.qbittorrent.image.tag.deeper", value: "x", want: "not a mapping"},
		{name: "a delete that is not set", fixture: "values.yaml", path: "apps.tautulli.enabled", value: nil, want: "not set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := ""
			if c.doc != "" {
				path = writeTemp(t, "values.yaml", c.doc)
			} else {
				path = copyFixture(t, c.fixture)
			}
			before, _ := os.ReadFile(path)
			s, err := Load(fixture(t, "grant.yaml"), path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var editErr error
			if c.value == nil {
				editErr = s.DeleteValue(c.path)
			} else {
				editErr = s.SetValue(c.path, c.value)
			}
			if editErr == nil {
				t.Fatal("the edit was accepted")
			}
			if !strings.Contains(editErr.Error(), c.want) {
				t.Errorf("error = %q, want %q", editErr, c.want)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Error("a refused edit changed the file")
			}
		})
	}
}

func TestDeleteTheOnlyKeyIsRefused(t *testing.T) {
	path := copyFixture(t, "values-terse.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.DeleteValue("apps.sonarr.env"); err != nil {
		t.Fatalf("DeleteValue: %v", err)
	}
	// resources is now the only key under apps.sonarr; removing it would leave
	// an empty value where the chart expects a mapping.
	err = s.DeleteValue("apps.sonarr.resources")
	if err == nil {
		t.Fatal("the last key under apps.sonarr was removed")
	}
	if !strings.Contains(err.Error(), "only key left") {
		t.Errorf("error = %q, want it to say why", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("stat: %v", statErr)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "resources:") == false {
		t.Errorf("the refused delete removed a key:\n%s", after)
	}
	if string(before) == string(after) {
		t.Error("the first delete did not happen")
	}
}

func TestCRLFFileIsRefused(t *testing.T) {
	path := writeTemp(t, "values.yaml", "global:\r\n  tz: UTC\r\n")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = s.SetValue("global.tz", "Etc/UTC")
	if err == nil || !strings.Contains(err.Error(), "Windows line endings") {
		t.Fatalf("error = %v, want a line-ending refusal", err)
	}
}

func TestSaveKeepsTheFileMode(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("wiring.timeoutSeconds", 900); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %o, want 640", got)
	}
}

func TestFailedWriteLeavesTheOriginal(t *testing.T) {
	path := copyFixture(t, "values.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := s.Values.Editor()
	if err := e.Set("wiring.timeoutSeconds", 900); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.rename = func(_, _ string) error { return errors.New("simulated rename failure") }
	if err := e.Save(); err == nil {
		t.Fatal("Save reported success after the rename failed")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("the original file changed after a failed write")
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
	if s.Values.Src != nil && string(s.Values.Src) != string(before) {
		t.Error("the in-memory document moved even though the write failed")
	}
}

func TestSaveCreatesAMissingValuesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("wiring.timeoutSeconds", 60); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "wiring:\n  timeoutSeconds: 60\n" {
		t.Errorf("file = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600: a values file can hold a Plex token", info.Mode().Perm())
	}
}

func TestEditsAccumulateInOneWrite(t *testing.T) {
	path := copyFixture(t, "values-terse.yaml")
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := s.Values.Editor()
	if err := e.Set("global.tz", "Etc/GMT"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := e.Set("apps.plex.rewritePath", true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !e.Changed() {
		t.Fatal("Changed() = false after two edits")
	}
	if err := e.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := e.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	merged, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged["global"].(map[string]any)["tz"] != "Etc/GMT" {
		t.Error("the first edit did not survive the second")
	}
	if merged["apps"].(map[string]any)["plex"].(map[string]any)["rewritePath"] != true {
		t.Error("the second edit is not in the merged values")
	}
}

func TestSetAfterABlockScalar(t *testing.T) {
	doc := "apps:\n  plex:\n    env:\n      NOTE: |\n        line one\n        line two\n"
	path := writeTemp(t, "values.yaml", doc)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("apps.plex.rewritePath", true); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := doc + "    rewritePath: true\n"
	if string(got) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetRefusesABlockScalar(t *testing.T) {
	doc := "apps:\n  plex:\n    env:\n      NOTE: |\n        line one\n"
	path := writeTemp(t, "values.yaml", doc)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = s.SetValue("apps.plex.env.NOTE", "one line")
	if err == nil || !strings.Contains(err.Error(), "multi-line block") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != doc {
		t.Error("a refused edit changed the file")
	}
}

func TestSetKeepsTheTypeTheCallerMeant(t *testing.T) {
	doc := "wiring:\n  timeoutSeconds: 600\n"
	path := writeTemp(t, "values.yaml", doc)
	s, err := Load(fixture(t, "grant.yaml"), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.SetValue("wiring.timeoutSeconds", "600"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "wiring:\n  timeoutSeconds: \"600\"\n" {
		t.Fatalf("file = %q, want the string quoted so it stays a string", got)
	}
	if err := s.SetValue("wiring.timeoutSeconds", 600); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != doc {
		t.Fatalf("file = %q, want the number unquoted", got)
	}
}

func TestDeleteAListEntry(t *testing.T) {
	path := copyFixture(t, "grant.yaml")
	before, _ := os.ReadFile(path)
	s, err := Load(path, fixture(t, "values.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.DeleteGrant("apps.radarr.instances[1]"); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	after, _ := os.ReadFile(path)
	oldBlock, newBlock := blockDiff(before, after)
	if len(newBlock) != 0 || len(oldBlock) != 3 {
		t.Fatalf("removed %q, added %q; want the three-line entry gone", oldBlock, newBlock)
	}
	merged, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	inst := merged["apps"].(map[string]any)["radarr"].(map[string]any)["instances"].([]any)
	if len(inst) != 1 {
		t.Errorf("instances = %v, want one", inst)
	}
}
