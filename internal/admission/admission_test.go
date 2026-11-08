package admission

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// decodeAll parses a multi-document YAML stream into plain values. Both sides of
// the parity test go through it, so indentation, comments, map order and flow
// vs block style cannot make a difference: only the objects can.
func decodeAll(t *testing.T, what string, data []byte) []any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []any
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: parse: %v", what, err)
		}
		if doc == nil {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestRenderMatchesThePreviousRenderer is the parity proof: each golden is the
// output of the platform layer's original Python renderer, captured once per
// input before that renderer was deleted in favour of this package. The Go
// output has to parse to the same objects, document for document.
//
// The first case renders the real chart, so the golden fails when
// charts/s5n/variants.yaml gains, loses or renames a repository. That is on
// purpose: the params are where "blessed by the chart" reaches already-installed
// seedboxes, and the failure is the prompt to decide whether the new list is
// meant. Regenerate the golden and say so in the commit.
func TestRenderMatchesThePreviousRenderer(t *testing.T) {
	cases := []struct {
		name   string
		chart  string
		grant  Grant
		golden string
	}{
		{
			name:   "the e2e's own input, on the real chart",
			chart:  "../../charts/s5n",
			grant:  Grant{Namespace: "seedbox", TorrentNodePort: 31111, ServiceType: "NodePort"},
			golden: "testdata/golden/seedbox-nodeport.yaml",
		},
		{
			name:   "two apps on one repository, LoadBalancer",
			chart:  "testdata/charts/dedupe",
			grant:  Grant{Namespace: "example-user", TorrentNodePort: 30500, ServiceType: "LoadBalancer"},
			golden: "testdata/golden/example-user-loadbalancer.yaml",
		},
		{
			name:   "one repository, ClusterIP",
			chart:  "testdata/charts/clusterip",
			grant:  Grant{Namespace: "example-media", TorrentNodePort: 31999, ServiceType: "ClusterIP"},
			golden: "testdata/golden/example-media-clusterip.yaml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := RenderChart(tc.chart, tc.grant)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			out, err := docs.YAML()
			if err != nil {
				t.Fatalf("yaml: %v", err)
			}
			got := decodeAll(t, "go", out)
			want := decodeAll(t, "golden", readFixture(t, tc.golden))
			if len(got) != len(want) {
				t.Fatalf("%d objects, want %d\ngot:\n%s", len(got), len(want), out)
			}
			for i := range want {
				if reflect.DeepEqual(got[i], want[i]) {
					continue
				}
				gotYAML, _ := yaml.Marshal(got[i])
				wantYAML, _ := yaml.Marshal(want[i])
				t.Errorf("document %d differs\n--- go:\n%s\n--- golden:\n%s", i, gotYAML, wantYAML)
			}
		})
	}
}

func TestRepositoriesDedupesAndSorts(t *testing.T) {
	// Two apps share registry.example.com/seedbox/arr; one app has a second
	// variant. Dedupe first, then sort: a re-render of unchanged input has to
	// produce the same string, or every `kubectl apply` rewrites the params.
	got, err := Repositories(readFixture(t, "testdata/charts/dedupe/variants.yaml"))
	if err != nil {
		t.Fatalf("repositories: %v", err)
	}
	want := []string{
		"registry.example.com/seedbox/arr",
		"registry.example.com/seedbox/arr-alt",
		"registry.example.com/seedbox/qbittorrent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repositories = %v, want %v", got, want)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	variants := readFixture(t, "testdata/charts/clusterip/variants.yaml")
	good := Grant{Namespace: "example-user", TorrentNodePort: 30000, ServiceType: "NodePort"}
	cases := []struct {
		name   string
		mutate func(*Grant)
		want   string
	}{
		{"empty namespace", func(g *Grant) { g.Namespace = "" }, `namespace ""`},
		{"uppercase namespace", func(g *Grant) { g.Namespace = "Seedbox" }, `"Seedbox"`},
		{"underscore in namespace", func(g *Grant) { g.Namespace = "example_user" }, `"example_user"`},
		{"leading dash", func(g *Grant) { g.Namespace = "-example" }, `"-example"`},
		{"trailing dash", func(g *Grant) { g.Namespace = "example-" }, `"example-"`},
		{"64-character namespace", func(g *Grant) { g.Namespace = strings.Repeat("a", 64) }, strings.Repeat("a", 64)},
		{"port below the range", func(g *Grant) { g.TorrentNodePort = 29999 }, "29999"},
		{"port above the range", func(g *Grant) { g.TorrentNodePort = 32768 }, "32768"},
		{"port zero", func(g *Grant) { g.TorrentNodePort = 0 }, "nodePort 0 "},
		{"ExternalName", func(g *Grant) { g.ServiceType = "ExternalName" }, `"ExternalName"`},
		{"lowercase type", func(g *Grant) { g.ServiceType = "nodeport" }, `"nodeport"`},
		{"empty type", func(g *Grant) { g.ServiceType = "" }, `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := good
			tc.mutate(&g)
			_, err := Render(variants, g)
			if err == nil {
				t.Fatalf("rendered a grant with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the bad input %q", err, tc.want)
			}
		})
	}
}

// TestRenderRejectsBadVariants: the chart owns variants.yaml, so a file this
// package cannot read is a chart bug, and the render refuses rather than
// writing params with a partial blessed list.
func TestRenderRejectsBadVariants(t *testing.T) {
	grant := Grant{Namespace: "example-user", TorrentNodePort: 30000, ServiceType: "NodePort"}
	cases := []struct {
		name     string
		variants string
		want     string
	}{
		{"empty", "", "is empty"},
		{"whitespace only", "  \n\t\n", "is empty"},
		{"comments only", "# nothing here\n", "names no repositories"},
		{"not YAML", "apps: [\n", "variants.yaml"},
		{"top level a list", "- plex\n", "variants.yaml"},
		{"top level a string", "plex\n", "variants.yaml"},
		{"app is a string", "plex: registry.example.com/plex\n", `app "plex" is not a map`},
		{"variant is a string", "plex:\n  default: registry.example.com/plex\n", "plex.default is not a variant"},
		{"no repository", "plex:\n  default:\n    tag: \"1.43.5\"\n", "names no repositories"},
		{"empty repository", "plex:\n  default:\n    repository: \"\"\n", "names no repositories"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render([]byte(tc.variants), grant)
			if err == nil {
				t.Fatal("rendered variants that name nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestBindingsSelectExactlyTheNamespace is the tenancy invariant: a
// cluster-scoped binding, selecting by the namespace's own name label, reading
// params from root's namespace, and denying when the params are gone.
func TestBindingsSelectExactlyTheNamespace(t *testing.T) {
	g := Grant{Namespace: "example-user", TorrentNodePort: 31111, ServiceType: "NodePort"}
	docs, err := Render(readFixture(t, "testdata/charts/clusterip/variants.yaml"), g)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if docs.Params.Metadata.Namespace != ParamsNamespace {
		t.Errorf("params namespace = %q, want %q", docs.Params.Metadata.Namespace, ParamsNamespace)
	}
	if name := docs.Params.Metadata.Name; name != "s5n-grant-example-user" {
		t.Errorf("params name = %q", name)
	}
	if len(docs.Bindings) != 2 {
		t.Fatalf("%d bindings, want 2", len(docs.Bindings))
	}
	for i, wantPolicy := range []string{"s5n-blessed-images", "s5n-granted-service"} {
		b := docs.Bindings[i]
		if b.Spec.PolicyName != wantPolicy {
			t.Errorf("binding %d policy = %q, want %q", i, b.Spec.PolicyName, wantPolicy)
		}
		if b.Metadata.Namespace != "" {
			t.Errorf("binding %s is namespaced %q: it must be cluster-scoped", b.Metadata.Name, b.Metadata.Namespace)
		}
		if !reflect.DeepEqual(b.Spec.ValidationActions, []string{"Deny"}) {
			t.Errorf("binding %s actions = %v", b.Metadata.Name, b.Spec.ValidationActions)
		}
		if b.Spec.ParamRef.Name != docs.Params.Metadata.Name || b.Spec.ParamRef.Namespace != ParamsNamespace {
			t.Errorf("binding %s reads params %s/%s, want %s/%s", b.Metadata.Name,
				b.Spec.ParamRef.Namespace, b.Spec.ParamRef.Name, ParamsNamespace, docs.Params.Metadata.Name)
		}
		if b.Spec.ParamRef.ParameterNotFoundAction != "Deny" {
			t.Errorf("binding %s admits on missing params", b.Metadata.Name)
		}
		exprs := b.Spec.MatchResources.NamespaceSelector.MatchExpressions
		if len(exprs) != 1 || exprs[0].Key != labelSelectorKey || exprs[0].Operator != "In" ||
			!reflect.DeepEqual(exprs[0].Values, []string{g.Namespace}) {
			t.Errorf("binding %s selects %+v, want namespace %q only", b.Metadata.Name, exprs, g.Namespace)
		}
	}
}

// TestPolicyReadsTheseParams fails when policy.yaml stops reading a key this
// package writes: the params object and the policies that consume it move
// together, and nothing else would notice a renamed key.
func TestPolicyReadsTheseParams(t *testing.T) {
	policy := string(readFixture(t, "../../install/platform/admission/policy.yaml"))
	for _, want := range []string{
		`params.data.blessedRepositories.split(',')`,
		"params.data.serviceType",
		"params.data.torrentNodePort",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy.yaml no longer reads %q: this package writes it", want)
		}
	}
}

// TestRenderChartKeepsTheBadInputAndTheBadChartApart: a namespace root typed
// wrong must not be reported as a variants.yaml problem, and a variants.yaml
// this package cannot read must name the file it read.
func TestRenderChartKeepsTheBadInputAndTheBadChartApart(t *testing.T) {
	chart := t.TempDir()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(chart, "variants.yaml"), []byte(content), 0o600); err != nil {
			t.Fatalf("write variants: %v", err)
		}
	}
	write("plex:\n  default:\n    repository: registry.example.com/plex\n")
	_, err := RenderChart(chart, Grant{Namespace: "NotALabel", TorrentNodePort: 30001, ServiceType: "NodePort"})
	if err == nil {
		t.Fatal("rendered a namespace that is not a DNS label")
	}
	if strings.Contains(err.Error(), "variants.yaml") {
		t.Fatalf("bad namespace reported as a chart problem: %v", err)
	}
	write("apps: [\n")
	_, err = RenderChart(chart, Grant{Namespace: "example-user", TorrentNodePort: 30001, ServiceType: "NodePort"})
	if err == nil {
		t.Fatal("rendered a variants.yaml that does not parse")
	}
	if want := filepath.Join(chart, "variants.yaml"); !strings.Contains(err.Error(), want) {
		t.Fatalf("bad variants reported as %v, want it to name %s", err, want)
	}
}
