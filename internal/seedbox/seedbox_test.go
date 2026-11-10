package seedbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture is a file under testdata. Testdata is read-only: anything that saves
// works on a copy.
func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

// copyFixture writes a fixture into a temporary directory, with a mode that is
// not the one box would create, so a write that forgets the mode is visible.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(fixture(t, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func loadFixture(t *testing.T, values string) *Seedbox {
	t.Helper()
	s, err := Load(fixture(t, "grant.yaml"), fixture(t, values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestLoad(t *testing.T) {
	t.Run("missing values file is an empty document", func(t *testing.T) {
		s, err := Load(fixture(t, "grant.yaml"), filepath.Join(t.TempDir(), "absent.yaml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if s.Values.Exists {
			t.Error("Values.Exists = true for a file that is not there")
		}
		if len(s.Values.data) != 0 {
			t.Errorf("values = %v, want empty", s.Values.data)
		}
		if got := s.Grant.data["namespace"]; got != "alice" {
			t.Errorf("namespace = %v, want alice", got)
		}
	})

	t.Run("missing grant is an error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "alice.yaml"), "")
		if err == nil {
			t.Fatal("Load succeeded with no grant")
		}
		if !strings.Contains(err.Error(), "grant") {
			t.Errorf("error = %q, want it to name the grant", err)
		}
	})

	t.Run("empty grant path is an error", func(t *testing.T) {
		if _, err := Load("", fixture(t, "values.yaml")); err == nil {
			t.Fatal("Load succeeded with no grant path")
		}
	})

	t.Run("unparsable values names the file", func(t *testing.T) {
		path := writeTemp(t, "values.yaml", "global:\n  tz: [\n")
		_, err := Load(fixture(t, "grant.yaml"), path)
		if err == nil {
			t.Fatal("Load accepted broken YAML")
		}
		if !strings.Contains(err.Error(), "values.yaml") {
			t.Errorf("error = %q, want the path", err)
		}
	})

	t.Run("a duplicate key is refused", func(t *testing.T) {
		path := writeTemp(t, "values.yaml", "global:\n  tz: a\n  tz: b\n")
		_, err := Load(fixture(t, "grant.yaml"), path)
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("error = %v, want a duplicate-key error", err)
		}
	})

	t.Run("an alias is refused rather than followed", func(t *testing.T) {
		path := writeTemp(t, "values.yaml", "global: &g\n  tz: a\nother: *g\n")
		_, err := Load(fixture(t, "grant.yaml"), path)
		if err == nil || !strings.Contains(err.Error(), "anchors and aliases") {
			t.Fatalf("error = %v, want an alias error", err)
		}
	})

	t.Run("a timestamp stays the text that was written", func(t *testing.T) {
		path := writeTemp(t, "values.yaml", "apps:\n  plex:\n    env:\n      RELEASE: 2020-01-01\n")
		s, err := Load(fixture(t, "grant.yaml"), path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := s.Values.data["apps"].(map[string]any)["plex"].(map[string]any)["env"].(map[string]any)["RELEASE"]
		if got != "2020-01-01" {
			t.Errorf("RELEASE = %#v, want the string 2020-01-01", got)
		}
	})

	t.Run("top level must be a mapping", func(t *testing.T) {
		path := writeTemp(t, "values.yaml", "- a\n- b\n")
		_, err := Load(fixture(t, "grant.yaml"), path)
		if err == nil || !strings.Contains(err.Error(), "mapping") {
			t.Fatalf("error = %v, want a mapping error", err)
		}
	})
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  []string
		index []int
	}{
		{name: "one key", in: "apps", want: []string{"apps"}},
		{name: "nested", in: "apps.sonarr.enabled", want: []string{"apps", "sonarr", "enabled"}},
		{name: "one index", in: "apps.sonarr.instances[1].enabled", want: []string{"apps", "sonarr", "instances", "enabled"}, index: []int{1}},
		{name: "two indices", in: "a[0][2].b", want: []string{"a", "b"}, index: []int{0, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParsePath(c.in)
			if err != nil {
				t.Fatalf("ParsePath(%q): %v", c.in, err)
			}
			if got := p.Keys(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("keys = %v, want %v", got, c.want)
			}
			var gotIdx []int
			for _, st := range p {
				if st.IsIndex {
					gotIdx = append(gotIdx, st.Index)
				}
			}
			if !reflect.DeepEqual(gotIdx, c.index) {
				t.Errorf("indices = %v, want %v", gotIdx, c.index)
			}
			if back, err := ParsePath(p.String()); err != nil || back.String() != p.String() {
				t.Errorf("round trip = %q (%v), want %q", back.String(), err, p.String())
			}
		})
	}

	for _, bad := range []string{"", ".a", "a..b", "a[x]", "a[", "a[-1]"} {
		if _, err := ParsePath(bad); err == nil {
			t.Errorf("ParsePath(%q) succeeded, want an error", bad)
		}
	}
}

func TestDocMapIsACopy(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	m := s.Values.Map()
	m["global"].(map[string]any)["tz"] = "mutated"
	if s.Values.data["global"].(map[string]any)["tz"] != "Europe/Berlin" {
		t.Error("Map returned the document's own map")
	}
}

// LoadValues exists for the one caller that has a values file and no grant
// yet: box init on a host root has decided nothing about. The file is read,
// not assumed empty.
func TestLoadValues(t *testing.T) {
	doc, err := LoadValues(fixture(t, "values.yaml"))
	if err != nil {
		t.Fatalf("LoadValues: %v", err)
	}
	if !doc.Exists {
		t.Fatal("Values.Exists = false for a file that is there")
	}
	if len(doc.data) == 0 {
		t.Fatal("a values file with content loaded empty")
	}
	absent, err := LoadValues(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadValues(absent): %v", err)
	}
	if absent.Exists || len(absent.data) != 0 {
		t.Fatalf("an absent values file loaded as %+v", absent.data)
	}
}
