package seedbox

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergeGrantWins(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	got, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if host := got["gateway"].(map[string]any)["host"]; host != "alice.example.com" {
		t.Errorf("gateway.host = %v, want the grant's alice.example.com", host)
	}
	global := got["global"].(map[string]any)
	if global["puid"] != 1001 {
		t.Errorf("global.puid = %#v, want the grant's 1001", global["puid"])
	}
	if global["tz"] != "Europe/Berlin" {
		t.Errorf("global.tz = %#v, want the user's Europe/Berlin", global["tz"])
	}
	if _, ok := got["namespace"]; ok {
		t.Error("namespace reached the values handed to Helm")
	}
	if sonarr := got["apps"].(map[string]any)["sonarr"].(map[string]any); sonarr["enabled"] != true {
		t.Errorf("apps.sonarr.enabled = %#v, want the grant's true", sonarr["enabled"])
	}
}

// TestOverlayPrecedence proves the rule itself, without the conflict check in
// front of it: whatever the user's document holds, the grant's value is what
// comes out, and a list is replaced whole rather than merged entry by entry.
func TestOverlayPrecedence(t *testing.T) {
	values := map[string]any{
		"gateway": map[string]any{"host": "values.example.com"},
		"apps": map[string]any{"sonarr": map[string]any{
			"enabled":   true,
			"instances": []any{map[string]any{"suffix": "x"}},
		}},
	}
	grant := map[string]any{
		"namespace": "alice",
		"gateway":   map[string]any{"host": "grant.example.com"},
		"apps": map[string]any{"sonarr": map[string]any{
			"enabled":   false,
			"instances": []any{map[string]any{"suffix": "4k"}, map[string]any{"suffix": "1080"}},
		}},
	}
	overlay(values, grant, nil)

	if host := values["gateway"].(map[string]any)["host"]; host != "grant.example.com" {
		t.Errorf("gateway.host = %v, want the grant's", host)
	}
	sonarr := values["apps"].(map[string]any)["sonarr"].(map[string]any)
	if sonarr["enabled"] != false {
		t.Errorf("apps.sonarr.enabled = %v, want the grant's false", sonarr["enabled"])
	}
	instances := sonarr["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("instances = %v, want the grant's two entries, replaced whole", instances)
	}
	if instances[1].(map[string]any)["suffix"] != "1080" {
		t.Errorf("instances[1] = %v, want the grant's", instances[1])
	}
	if _, ok := values["namespace"]; ok {
		t.Error("namespace was overlaid into the chart values")
	}
}

func TestMergeRefusesGrantKeysInValues(t *testing.T) {
	s := loadFixture(t, "values-grant-keys.yaml")
	_, err := s.Merge()
	if err == nil {
		t.Fatal("Merge accepted a values file that sets grant keys")
	}
	msg := err.Error()
	for _, want := range []string{
		"values-grant-keys.yaml:2: namespace",
		"values-grant-keys.yaml:5: gateway.host",
		"values-grant-keys.yaml:9: apps.sonarr.instances",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
}

func TestMergeMissingValuesFile(t *testing.T) {
	s, err := Load(fixture(t, "grant.yaml"), filepath.Join(t.TempDir(), "values.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if host := got["gateway"].(map[string]any)["host"]; host != "alice.example.com" {
		t.Errorf("gateway.host = %v, want the grant's", host)
	}
}

func TestMergeGrantProblems(t *testing.T) {
	cases := []struct {
		name  string
		grant string
		want  string
	}{
		{
			name: "an app without enabled",
			grant: `namespace: alice
gateway:
  host: alice.example.com
apps:
  sonarr:
    instances:
      - {suffix: "", category: tv, enabled: true}
`,
			want: "apps.sonarr.enabled is missing",
		},
		{
			name: "a key the grant does not own",
			grant: `namespace: alice
gateway:
  host: alice.example.com
apps:
  sonarr:
    enabled: true
    image:
      tag: "6.0.0"
`,
			want: "apps.sonarr.image.tag is not a grant key",
		},
		{
			name: "no domain",
			grant: `namespace: alice
global:
  puid: 1001
`,
			want: "gateway.host is missing",
		},
		{
			name: "no namespace",
			grant: `gateway:
  host: alice.example.com
`,
			want: "namespace is missing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			grant := writeTemp(t, "grant.yaml", c.grant)
			values := writeTemp(t, "values.yaml", "wiring:\n  timeoutSeconds: 60\n")
			s, err := Load(grant, values)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, err = s.Merge()
			if err == nil {
				t.Fatal("Merge accepted a broken grant")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "grant.yaml:") {
				t.Errorf("error = %q, want a file:line", err)
			}
		})
	}
}

func TestMergeWithoutApps(t *testing.T) {
	grant := writeTemp(t, "grant.yaml", "namespace: alice\ngateway:\n  host: alice.example.com\n")
	s, err := Load(grant, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	apps, ok := got["apps"].(map[string]any)
	if !ok || len(apps) != 0 {
		t.Errorf("apps = %#v, want an empty mapping so the schema sees one", got["apps"])
	}
}

func TestMergeDoesNotMutateDocuments(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	before := s.Values.Map()
	got, err := s.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got["global"].(map[string]any)["tz"] = "mutated"
	if !reflect.DeepEqual(s.Values.Map(), before) {
		t.Error("Merge returned the document's own map")
	}
}

// TestMergeForcesAuthEdge: the chart ships no login and refuses to render
// without auth.edge, so box's own values say it. Neither an absent key nor an
// empty branch can leave it out, and the effective values (what Helm receives)
// carry it too.
func TestMergeForcesAuthEdge(t *testing.T) {
	cases := []struct {
		name   string
		values string
	}{
		{"omitted", "wiring:\n  timeoutSeconds: 60\n"},
		{"empty auth branch", "auth: {}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Load(fixture(t, "grant.yaml"), writeTemp(t, "values.yaml", c.values))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := s.Merge()
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			auth, ok := got["auth"].(map[string]any)
			if !ok || auth["edge"] != true {
				t.Fatalf("auth = %#v, want edge true: the chart refuses to render without it", got["auth"])
			}
		})
	}

	s := loadFixture(t, "values.yaml")
	got, err := s.Effective(realChart(t))
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	auth, ok := got["auth"].(map[string]any)
	if !ok || auth["edge"] != true {
		t.Fatalf("effective auth = %#v, want edge true", got["auth"])
	}
}

// TestMergeRefusesAuthEdgeInValues: auth.edge is box's, like a claim name. A
// user's file may not turn the login off -- or on: setting it at all is a grant
// key in values, and the error names the key and the line.
func TestMergeRefusesAuthEdgeInValues(t *testing.T) {
	for _, edge := range []string{"false", "true"} {
		t.Run(edge, func(t *testing.T) {
			s, err := Load(fixture(t, "grant.yaml"), writeTemp(t, "values.yaml", "auth:\n  edge: "+edge+"\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, err = s.Merge()
			if err == nil {
				t.Fatal("Merge accepted a values file that sets auth.edge")
			}
			if !strings.Contains(err.Error(), "values.yaml:2: auth.edge is set by box") {
				t.Errorf("error = %v, want it to name the key, the line and box", err)
			}
		})
	}
}

// TestMergeRefusesAuthEdgeInGrant: root's grant may not carry it either. box
// writes the constant itself, so the grant refusal says so rather than claiming
// it is derived from the grant.
func TestMergeRefusesAuthEdgeInGrant(t *testing.T) {
	grant := writeTemp(t, "grant.yaml", "namespace: alice\ngateway:\n  host: alice.example.com\nauth:\n  edge: true\n")
	s, err := Load(grant, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = s.Merge()
	if err == nil {
		t.Fatal("Merge accepted a grant that sets auth.edge")
	}
	if !strings.Contains(err.Error(), "grant.yaml:5: auth.edge is box's; box sets it itself") {
		t.Errorf("error = %v, want it to say box sets it itself", err)
	}
}
