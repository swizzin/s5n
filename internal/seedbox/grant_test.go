package seedbox

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	chartSchemaPath = "../../charts/s5n/values.schema.json"
	chartValuesPath = "../../charts/s5n/values.yaml"
)

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func readYAMLFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// TestGrantKeysAreRealKeys is the guard against a grant rule that names a key
// the chart does not have: a typo there would leave the key user-writable and
// silently unprotected.
func TestGrantKeysAreRealKeys(t *testing.T) {
	schema := readJSONFile(t, chartSchemaPath)
	values := readYAMLFile(t, chartValuesPath)
	apps := sortedKeys(values["apps"].(map[string]any))

	for _, rule := range GrantKeys {
		if rule.BoxOnly {
			continue // box's own key, not a chart value
		}
		if !strings.Contains(rule.Path, "*") {
			if _, ok := schemaAt(schema, strings.Split(rule.Path, ".")); !ok {
				t.Errorf("grant rule %q: the chart schema has no such key, so the rule protects nothing", rule.Path)
			}
			continue
		}
		// A wildcard rule ("every app") has to exist for at least one app: the
		// wildcard also covers apps that legitimately have no such key.
		found := 0
		for _, app := range apps {
			path := strings.Replace(rule.Path, "*", app, 1)
			if _, ok := schemaAt(schema, strings.Split(path, ".")); ok {
				found++
			}
		}
		if found == 0 {
			t.Errorf("grant rule %q matches no app in the chart schema", rule.Path)
		}
	}
}

// TestEveryAppIsGrantable: an app the chart ships but no rule covers would be an
// app a user's values could switch on. The wildcard should make this true by
// construction; the test proves the wildcard actually reaches every app.
func TestEveryAppIsGrantable(t *testing.T) {
	values := readYAMLFile(t, chartValuesPath)
	apps := values["apps"].(map[string]any)
	for _, app := range sortedKeys(apps) {
		if _, ok := GrantRule([]string{"apps", app, "enabled"}); !ok {
			t.Errorf("apps.%s.enabled is not a grant key, so a user could decide it", app)
		}
	}
}

// TestEveryInstanceListIsGranted: only the Arrs take instances, but the rule is
// a wildcard, so this asserts the schema agrees about which apps have them.
func TestEveryInstanceListIsGranted(t *testing.T) {
	schema := readJSONFile(t, chartSchemaPath)
	values := readYAMLFile(t, chartValuesPath)
	for _, app := range sortedKeys(values["apps"].(map[string]any)) {
		node, ok := schemaAt(schema, []string{"apps", app})
		if !ok {
			t.Fatalf("schema has no apps.%s", app)
		}
		props, _ := node["properties"].(map[string]any)
		if _, has := props["instances"]; !has {
			continue
		}
		if _, ok := GrantRule([]string{"apps", app, "instances", "0", "enabled"}); !ok {
			t.Errorf("apps.%s.instances is not granted, so a user could invent an instance", app)
		}
	}
}

func TestGrantRule(t *testing.T) {
	cases := []struct {
		keys []string
		want bool
	}{
		{keys: []string{"gateway", "host"}, want: true},
		{keys: []string{"gateway", "host", "extra"}, want: false},
		{keys: []string{"apps", "sonarr", "enabled"}, want: true},
		{keys: []string{"apps", "sonarr", "instances"}, want: true},
		{keys: []string{"apps", "sonarr", "instances", "enabled"}, want: true},
		{keys: []string{"apps", "sonarr", "image", "tag"}, want: false},
		{keys: []string{"apps", "sonarr"}, want: false},
		{keys: []string{"global", "tz"}, want: false},
		{keys: []string{"global", "puid"}, want: true},
		{keys: []string{"networkPolicy", "enabled"}, want: true},
		{keys: []string{"networkPolicy", "extraHttpFrom"}, want: false},
		{keys: []string{"storage", "data", "size"}, want: false},
		{keys: []string{"storage", "data", "hostPath"}, want: true},
	}
	for _, c := range cases {
		if _, ok := GrantRule(c.keys); ok != c.want {
			t.Errorf("GrantRule(%v) = %v, want %v", c.keys, ok, c.want)
		}
	}
}

func TestNoDuplicateRules(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range GrantKeys {
		if seen[r.Path] {
			t.Errorf("rule %q is listed twice", r.Path)
		}
		seen[r.Path] = true
	}
}

func TestSetGrantRefusesAValueKey(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	grant := copyFixture(t, "grant.yaml")
	s.Grant.Path = grant
	s.Grant.Src, _ = os.ReadFile(grant)

	err := s.SetGrant("apps.sonarr.image.tag", "6.0.0")
	if err == nil {
		t.Fatal("SetGrant accepted an image tag")
	}
	if !strings.Contains(err.Error(), "not a grant key") {
		t.Errorf("error = %q, want it to say the key is not the grant's", err)
	}
	if got, _ := os.ReadFile(grant); string(got) != string(s.Grant.Src) {
		t.Error("the grant changed on a refused edit")
	}
}

func TestSetValueRefusesAGrantKey(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	before := string(s.Values.Src)

	for _, path := range []string{"gateway.host", "apps.sonarr.instances[1].enabled", "global.puid"} {
		err := s.SetValue(path, "x")
		if err == nil {
			t.Fatalf("SetValue(%q) succeeded", path)
		}
		if !strings.Contains(err.Error(), "grant") {
			t.Errorf("SetValue(%q) error = %q, want it to name the grant", path, err)
		}
	}
	if string(s.Values.Src) != before {
		t.Error("the values file changed on a refused edit")
	}
}

// schemaAt resolves a dotted path through the schema's properties, following a
// local $ref at every level.
func schemaAt(schema map[string]any, keys []string) (map[string]any, bool) {
	cur := schema
	for _, k := range keys {
		cur = deref(schema, cur)
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := props[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return deref(schema, cur), true
}

func deref(root, node map[string]any) map[string]any {
	for i := 0; i < 8; i++ {
		ref, ok := node["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return node
		}
		cur := root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur[part].(map[string]any)
			if !ok {
				return node
			}
			cur = m
		}
		node = cur
	}
	return node
}
