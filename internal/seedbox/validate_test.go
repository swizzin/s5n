package seedbox

import (
	"encoding/json"
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

// chartAt reads a chart release out of testdata: a release's values.yaml and
// values.schema.json are the whole of what box takes from one, so a pair of
// files is a release as far as this package is concerned.
func chartAt(t *testing.T, valuesName, schemaName string) Chart {
	t.Helper()
	values, err := os.ReadFile("testdata/" + valuesName)
	if err != nil {
		t.Fatalf("read %s: %v", valuesName, err)
	}
	schema, err := os.ReadFile("testdata/" + schemaName)
	if err != nil {
		t.Fatalf("read %s: %v", schemaName, err)
	}
	return Chart{Values: values, Schema: schema}
}

// TestEffectiveWritesAuthEdgeWhereTheChartDeclaresIt: the chart ships no login
// and refuses to render without auth.edge, so box's values carry it -- but only
// for a release whose schema says the key exists. It is written after the
// chart's defaults and after both files, so neither can shadow it.
func TestEffectiveWritesAuthEdgeWhereTheChartDeclaresIt(t *testing.T) {
	t.Run("the chart main ships", func(t *testing.T) {
		s := loadFixture(t, "values.yaml")
		got, err := s.Effective(realChart(t))
		if err != nil {
			t.Fatalf("Effective: %v", err)
		}
		auth, ok := got["auth"].(map[string]any)
		if !ok || auth["edge"] != true {
			t.Fatalf("auth = %#v, want edge true: the chart refuses to render without it", got["auth"])
		}
	})

	t.Run("a chart whose values default it to false", func(t *testing.T) {
		c := Chart{
			Values: []byte("apps: {}\nauth:\n  edge: false\n"),
			Schema: []byte(`{"type": "object", "properties": {"auth": {"type": "object", "properties": {"edge": {"type": "boolean"}}}}}`),
		}
		s := loadFixture(t, "values.yaml")
		got, err := s.Effective(c)
		if err != nil {
			t.Fatalf("Effective: %v", err)
		}
		auth, ok := got["auth"].(map[string]any)
		if !ok || auth["edge"] != true {
			t.Fatalf("auth = %#v, want edge true over the chart's default false", got["auth"])
		}
	})
}

// TestEffectiveOmitsAuthEdgeForChart020 is the compatibility claim: a box that
// knows auth.edge still applies to the release that predates it. The pair below
// is copied verbatim from the published tag v0.2.0 (`git show
// v0.2.0:charts/s5n/values.yaml` and the schema beside it). That schema sets
// additionalProperties: false and has no auth section, so writing the key is
// exactly the refusal -- "auth: is not a key the chart knows" -- an older
// install must not get from a newer box.
func TestEffectiveOmitsAuthEdgeForChart020(t *testing.T) {
	c := chartAt(t, "chart-0.2.0.values.yaml", "chart-0.2.0.schema.json")

	var doc map[string]any
	if err := json.Unmarshal(c.Schema, &doc); err != nil {
		t.Fatalf("parse the 0.2.0 schema: %v", err)
	}
	props, _ := doc["properties"].(map[string]any)
	if _, ok := props["auth"]; ok {
		t.Fatal("the 0.2.0 fixture declares auth: it is not the release this test is about any more")
	}

	s := loadFixture(t, "values.yaml")
	got, err := s.Effective(c)
	if err != nil {
		t.Fatalf("Effective against the 0.2.0 chart: %v", err)
	}
	if auth, ok := got["auth"]; ok {
		t.Errorf("auth = %#v, want no auth key: the 0.2.0 schema has none", auth)
	}
	if err := Validate(c.Schema, got); err != nil {
		t.Errorf("Validate against the 0.2.0 schema: %v", err)
	}
}

// TestEffectiveOmitsAuthEdgeWhenOnlyTheBranchIsDeclared: presence of
// properties.auth.properties.edge is the question, not presence of an auth
// branch. A release that describes some other auth key would still reject
// auth.edge, so box writes nothing.
func TestEffectiveOmitsAuthEdgeWhenOnlyTheBranchIsDeclared(t *testing.T) {
	c := Chart{
		Values: []byte("apps: {}\n"),
		Schema: []byte(`{"type": "object", "properties": {"auth": {"type": "object", "properties": {"other": {"type": "boolean"}}}}}`),
	}
	s := loadFixture(t, "values.yaml")
	got, err := s.Effective(c)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if auth, ok := got["auth"]; ok {
		t.Errorf("auth = %#v, want no auth key: the schema does not declare auth.edge", auth)
	}
}

// TestEffectiveRefusesAuthInEitherChart: moving the write out of Merge must not
// move the refusal. A file that sets auth.edge is refused whatever release is
// being applied, old or new, because the refusal is about the file: box writes
// the key, so saying it in either file is a mistake in that file.
func TestEffectiveRefusesAuthInEitherChart(t *testing.T) {
	charts := map[string]func(*testing.T) Chart{
		"chart 0.2.0": func(t *testing.T) Chart {
			return chartAt(t, "chart-0.2.0.values.yaml", "chart-0.2.0.schema.json")
		},
		"the chart main ships": realChart,
	}
	for name, chart := range charts {
		t.Run(name, func(t *testing.T) {
			for _, edge := range []string{"true", "false"} {
				t.Run("values "+edge, func(t *testing.T) {
					s, err := Load(fixture(t, "grant.yaml"), writeTemp(t, "values.yaml", "auth:\n  edge: "+edge+"\n"))
					if err != nil {
						t.Fatalf("Load: %v", err)
					}
					_, err = s.Effective(chart(t))
					if err == nil {
						t.Fatal("Effective accepted a values file that sets auth.edge")
					}
					if !strings.Contains(err.Error(), "values.yaml:2: auth.edge is set by box") {
						t.Errorf("error = %v, want it to name the key, the line and box", err)
					}
				})
			}

			t.Run("grant", func(t *testing.T) {
				grant := writeTemp(t, "grant.yaml", "namespace: alice\ngateway:\n  host: alice.example.com\nauth:\n  edge: true\n")
				s, err := Load(grant, "")
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				_, err = s.Effective(chart(t))
				if err == nil {
					t.Fatal("Effective accepted a grant that sets auth.edge")
				}
				if !strings.Contains(err.Error(), "grant.yaml:5: auth.edge is box's; box sets it itself") {
					t.Errorf("error = %v, want it to say box sets it itself", err)
				}
			})
		})
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
