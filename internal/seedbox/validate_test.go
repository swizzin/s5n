package seedbox

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func realChart(t *testing.T) Chart {
	t.Helper()
	values, err := os.ReadFile(chartValuesPath)
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	schema, err := os.ReadFile(chartSchemaPath)
	if err != nil {
		t.Fatalf("read chart schema: %v", err)
	}
	return Chart{Values: values, Schema: schema}
}

// TestEffectiveAgainstTheRealChart is the end-to-end claim: this fixture's grant
// and values, merged the way box merges them, are values the shipped chart would
// accept.
func TestEffectiveAgainstTheRealChart(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	got, err := s.Effective(realChart(t))
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	apps := got["apps"].(map[string]any)
	want := map[string]bool{
		"qbittorrent": true,
		"prowlarr":    true,
		"sonarr":      true,
		"radarr":      true,
		"plex":        false, // the chart defaults it on; the grant never gave it
		"tautulli":    false,
		"seerr":       false,
	}
	for name, enabled := range want {
		app, ok := apps[name].(map[string]any)
		if !ok {
			t.Fatalf("apps.%s is missing from the effective values", name)
		}
		if app["enabled"] != enabled {
			t.Errorf("apps.%s.enabled = %v, want %v (the grant decides)", name, app["enabled"], enabled)
		}
	}

	sonarr := apps["sonarr"].(map[string]any)
	instances, _ := sonarr["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("apps.sonarr.instances = %v, want the grant's two", sonarr["instances"])
	}
	if instances[1].(map[string]any)["enabled"] != true {
		t.Errorf("instances[1] = %v, want the grant's enabled 4k", instances[1])
	}
	if sonarr["image"].(map[string]any)["variant"] != "default" {
		t.Errorf("apps.sonarr.image = %v, want the chart's default variant", sonarr["image"])
	}
	if _, ok := sonarr["image"].(map[string]any)["repository"]; ok {
		t.Errorf("apps.sonarr.image carries a repository: %v", sonarr["image"])
	}

	qbt := apps["qbittorrent"].(map[string]any)
	if qbt["image"].(map[string]any)["variant"] != "default" {
		t.Errorf("qbt image = %v, want the user's chosen variant", qbt["image"])
	}
	if port := qbt["torrent"].(map[string]any)["port"]; port != 31111 {
		t.Errorf("torrent port = %v, want the chart default", port)
	}

	global := got["global"].(map[string]any)
	if global["puid"] != 1001 || global["tz"] != "Europe/Berlin" {
		t.Errorf("global = %v, want the grant's puid and the user's tz", global)
	}
	if _, ok := got["namespace"]; ok {
		t.Error("namespace reached the effective values")
	}
}

func TestValidateNamesTheKeyPath(t *testing.T) {
	c := realChart(t)
	s := loadFixture(t, "values.yaml")
	base, err := s.Effective(c)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string // path the error must name
		msg    string // and the words it must use
	}{
		{
			name: "a string where an integer belongs",
			mutate: func(m map[string]any) {
				m["global"].(map[string]any)["puid"] = "1001"
			},
			want: "global.puid",
			msg:  "expected integer",
		},
		{
			name: "a floating tag",
			mutate: func(m map[string]any) {
				m["apps"].(map[string]any)["sonarr"].(map[string]any)["image"].(map[string]any)["tag"] = "latest"
			},
			want: "apps.sonarr.image.tag",
			msg:  "not an allowed value",
		},
		{
			name: "a repository where only a variant belongs",
			mutate: func(m map[string]any) {
				m["apps"].(map[string]any)["radarr"].(map[string]any)["image"].(map[string]any)["repository"] = "ghcr.io/example/radarr"
			},
			want: "apps.radarr.image.repository",
			msg:  "not a key the chart knows",
		},
		{
			name: "an app the chart does not have",
			mutate: func(m map[string]any) {
				m["apps"].(map[string]any)["plexx"] = map[string]any{"enabled": false}
			},
			want: "apps.plexx",
			msg:  "not a key the chart knows",
		},
		{
			name: "a required key removed",
			mutate: func(m map[string]any) {
				delete(m["apps"].(map[string]any)["qbittorrent"].(map[string]any), "image")
			},
			want: "apps.qbittorrent.image",
			msg:  "is required",
		},
		{
			name: "a required section removed",
			mutate: func(m map[string]any) {
				delete(m, "storage")
			},
			want: "storage",
			msg:  "is required",
		},
		{
			name: "an instance missing its category",
			mutate: func(m map[string]any) {
				inst := m["apps"].(map[string]any)["sonarr"].(map[string]any)["instances"].([]any)
				delete(inst[0].(map[string]any), "category")
			},
			want: "apps.sonarr.instances[0].category",
			msg:  "is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := cloneMap(base)
			tc.mutate(m)
			err := Validate(c.Schema, m)
			if err == nil {
				t.Fatal("Validate accepted an invalid document")
			}
			var se *SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("error is %T, want *SchemaError: %v", err, err)
			}
			var found *Problem
			for i := range se.Problems {
				if se.Problems[i].Path == tc.want {
					found = &se.Problems[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no problem names %q; got:\n%s", tc.want, err)
			}
			if !strings.Contains(found.Message, tc.msg) {
				t.Errorf("%s: message = %q, want %q", tc.want, found.Message, tc.msg)
			}
		})
	}
}

func TestValidateWithoutASchema(t *testing.T) {
	if err := Validate(nil, map[string]any{}); err == nil {
		t.Fatal("Validate accepted a document with no schema to check it")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := realChart(t)
	s := loadFixture(t, "values.yaml")
	m, err := s.Effective(c)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	m["global"].(map[string]any)["pgid"] = "nope"
	m["global"].(map[string]any)["tz"] = ""

	err = Validate(c.Schema, m)
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *SchemaError", err)
	}
	if len(se.Problems) != 2 {
		t.Fatalf("problems = %v, want both pgid and tz", se.Problems)
	}
}

// The variant list is the chart's, not the user's: a repository in the user's
// values file must fail as a key the chart does not have, naming the path, not
// as a value that was quietly overridden.
func TestEffectiveRefusesARepositoryInValues(t *testing.T) {
	values := writeTemp(t, "values.yaml", `apps:
  radarr:
    image:
      repository: ghcr.io/example/radarr
`)
	s, err := Load(fixture(t, "grant.yaml"), values)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = s.Effective(realChart(t))
	if err == nil {
		t.Fatal("Effective accepted a user-chosen repository")
	}
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want *SchemaError: %v", err, err)
	}
	for _, p := range se.Problems {
		if p.Path == "apps.radarr.image.repository" {
			return
		}
	}
	t.Fatalf("no problem names apps.radarr.image.repository; got:\n%s", err)
}

// A chart schema is untrusted input to a root process: a $ref to a local file
// must fail to compile rather than be read.
func TestValidateRefusesExternalRefs(t *testing.T) {
	schema := []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"x":{"$ref":"file:///etc/hostname"}}}`)
	err := Validate(schema, map[string]any{"x": 1})
	if err == nil || !strings.Contains(err.Error(), "outside the chart schema") {
		t.Fatalf("external $ref was not refused: %v", err)
	}
}

// A category names one qBittorrent category and both /data trees, and an
// existing seedbox keeps the names it already used (docs/box.md, migrate): so
// upper case and one subcategory level are legal, while anything that could
// escape a tree, split differently in the two apps, or that qBittorrent itself
// refuses is not.
func TestCategoryPattern(t *testing.T) {
	c := realChart(t)
	base, err := Base(c)
	if err != nil {
		t.Fatalf("chart base: %v", err)
	}
	apps, _ := base["apps"].(map[string]any)
	radarr, _ := apps["radarr"].(map[string]any)
	instances, _ := radarr["instances"].([]any)
	if len(instances) == 0 {
		t.Fatal("chart values have no radarr instance to carry a category")
	}
	inst, _ := instances[0].(map[string]any)

	accept := []string{
		"movies", "Movies", "tv-4k", "TV",
		"4K/Movies", "4K/TV", "Movies.2024", "a_b",
	}
	for _, name := range accept {
		inst["category"] = name
		if err := Validate(c.Schema, base); err != nil {
			t.Errorf("category %q was rejected: %v", name, err)
		}
	}

	reject := []string{
		"",      // a category is required
		"/x",    // no leading separator
		"x/",    // no trailing separator
		"a//b",  // no empty segment
		"a/b/c", // one level only: both trees stay two deep
		"../x",  // no parent traversal
		"x/..",  // the second segment may not be ..
		".",     // not a directory name
		"..",    // nor the parent
		"a b",   // no spaces
		`a\b`,   // no backslash
	}
	for _, name := range reject {
		inst["category"] = name
		if err := Validate(c.Schema, base); err == nil {
			t.Errorf("category %q was accepted", name)
		}
	}
}
