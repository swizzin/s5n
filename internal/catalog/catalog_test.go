package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// miniManifest is a valid manifest for testdata/mini. Every hostile case below
// is this text with one thing broken, so the failure names the line a
// contributor would have got wrong.
const miniManifest = `apps:
  alpha:
    description: the first app
    instances: false
    default: true
    questions:
      - key: apps.alpha.greeting
        prompt: how should it greet you
        help: shown at the top of every page
        kind: string
        default: hello
      - key: apps.alpha.verbose
        prompt: log everything
        kind: bool
        default: false
  beta:
    description: the second app
    instances: true
    default: false
    questions:
      - key: global.tz
        prompt: timezone
        kind: string
        default: Etc/UTC
`

// brokenManifest breaks several things at once, in two apps: the ordering of
// its problem list is what TestProblemsComeInFileOrder pins.
const brokenManifest = `apps:
  alpha:
    description: ""
    instances: true
    default: true
    questions:
      - key: apps.alpha.greeting
        prompt: ""
        kind: string
        default: 7
      - key: apps.alpha.verbose
        prompt: log everything
        kind: bool
        default: "no"
      - key: apps.alpha.nonsense
        prompt: nothing at all
        kind: string
      - key: apps.beta.enabled
        prompt: broken kind
        kind: whisper
  beta:
    description: the second app
    instances: false
    questions:
      - key: apps.beta.greeting
        prompt: hi
        kind: string
      - key: apps.beta.greeting
        prompt: hi again
        kind: string
      - key: gateway.host
        prompt: host
        kind: string
      - key: apps.beta.instances[0].suffix
        prompt: list entry
        kind: string
  gamma:
    description: not an app
    instances: false
    default: false
    questions: []
`

func mini(t *testing.T) (values, schema []byte) {
	t.Helper()
	values, err := os.ReadFile("testdata/mini/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema, err = os.ReadFile("testdata/mini/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return values, schema
}

func parseMini(t *testing.T, manifest string) (*Catalog, error) {
	t.Helper()
	values, schema := mini(t)
	return Parse("catalog.yaml", []byte(manifest), values, schema)
}

func TestManifestKeepsOrderAndKinds(t *testing.T) {
	cat, err := parseMini(t, miniManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(cat.Names(), ","); got != "alpha,beta" {
		t.Fatalf("order = %s, want the manifest's alpha,beta", got)
	}
	if got := strings.Join(cat.Defaults(), ","); got != "alpha" {
		t.Fatalf("defaults = %s, want alpha only", got)
	}
	alpha, ok := cat.App("alpha")
	if !ok {
		t.Fatal("alpha is missing")
	}
	if alpha.Description != "the first app" || alpha.Instances || len(alpha.Questions) != 2 {
		t.Fatalf("alpha = %+v", alpha)
	}
	if got := len(alpha.Stored()); got != 2 {
		t.Fatalf("alpha stored questions = %d, want 2", got)
	}
	if got := len(alpha.Transient()); got != 0 {
		t.Fatalf("alpha transient questions = %d, want 0", got)
	}
	beta, _ := cat.App("beta")
	if !beta.Instances {
		t.Fatalf("beta should take instances: %+v", beta)
	}
	if _, ok := cat.App("gamma"); ok {
		t.Fatal("a name that is not in the manifest was found")
	}
}

func TestTransientIsASeparateList(t *testing.T) {
	manifest := strings.Replace(miniManifest,
		"      - key: global.tz\n        prompt: timezone\n        kind: string\n        default: Etc/UTC",
		"      - key: global.tz\n        prompt: timezone\n        kind: transient\n        optional: true", 1)
	cat, err := parseMini(t, manifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	beta, _ := cat.App("beta")
	if len(beta.Stored()) != 0 {
		t.Fatalf("a transient question is also a stored one: %+v", beta.Questions)
	}
	tr := beta.Transient()
	if len(tr) != 1 || tr[0].Key != "global.tz" || !tr[0].Optional {
		t.Fatalf("transient = %+v", tr)
	}
}

// A manifest that says nothing about exposure serves every app under a path;
// `host` is kept, and it is what box builds its list of names from.
func TestExposureDefaultsToPathAndHostIsKept(t *testing.T) {
	cat, err := parseMini(t, miniManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, name := range cat.Names() {
		app, _ := cat.App(name)
		if app.Exposure != ExposurePath {
			t.Fatalf("apps.%s exposure = %q, want path when the manifest says nothing", name, app.Exposure)
		}
	}
	if got := cat.Hosted(); len(got) != 0 {
		t.Fatalf("hosted = %v, want none", got)
	}

	manifest := strings.Replace(miniManifest,
		"    description: the first app\n",
		"    description: the first app\n    exposure: host\n", 1)
	cat, err = parseMini(t, manifest)
	if err != nil {
		t.Fatalf("parse with exposure host: %v", err)
	}
	alpha, _ := cat.App("alpha")
	if alpha.Exposure != ExposureHost {
		t.Fatalf("alpha exposure = %q, want host", alpha.Exposure)
	}
	if got := strings.Join(cat.Hosted(), ","); got != "alpha" {
		t.Fatalf("hosted = %s, want alpha", got)
	}
}

// An exposure box does not know is refused by name: it is a typo that would
// otherwise render the app's route under a path nobody meant.
func TestUnknownExposureIsNamed(t *testing.T) {
	manifest := strings.Replace(miniManifest,
		"    description: the first app\n",
		"    description: the first app\n    exposure: folder\n", 1)
	_, err := parseMini(t, manifest)
	if err == nil {
		t.Fatal("an unknown exposure was accepted")
	}
	if !strings.Contains(err.Error(), `apps.alpha.exposure: "folder" is not path or host`) {
		t.Fatalf("error = %v, want it to name the entry and both values", err)
	}
}

// An app with a host of its own needs one name to put before the domain; the
// Arrs take instances after the app's name, so the two cannot be combined.
func TestHostedAppWithInstancesIsRefused(t *testing.T) {
	manifest := strings.Replace(miniManifest,
		"    instances: true\n",
		"    instances: true\n    exposure: host\n", 1)
	_, err := parseMini(t, manifest)
	if err == nil {
		t.Fatal("an app with instances and a host of its own was accepted")
	}
	if !strings.Contains(err.Error(), "apps.beta.exposure: host needs one name, but this app takes instances") {
		t.Fatalf("error = %v, want it to name the entry", err)
	}
}

// TestHostileManifestIsNamed: every refusal says which entry is wrong. A
// manifest is untrusted input to a root process, and "the catalog is broken"
// would leave the contributor guessing.
func TestHostileManifestIsNamed(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		wants    []string
	}{
		{
			name:     "grant key",
			manifest: strings.Replace(miniManifest, "key: global.tz", "key: gateway.host", 1),
			wants:    []string{"apps.beta.questions[0].key", "gateway.host", "grant"},
		},
		{
			name:     "box-only grant key",
			manifest: strings.Replace(miniManifest, "key: global.tz", "key: namespace", 1),
			wants:    []string{"namespace", "grant"},
		},
		{
			name:     "box-sets grant key",
			manifest: strings.Replace(miniManifest, "key: global.tz", "key: storage.data.existingClaim", 1),
			wants:    []string{"storage.data.existingClaim", "set by box"},
		},
		{
			name:     "box-sets auth.edge",
			manifest: strings.Replace(miniManifest, "key: global.tz", "key: auth.edge", 1),
			wants:    []string{"auth.edge", "set by box"},
		},
		{
			name:     "unknown kind",
			manifest: strings.Replace(miniManifest, "kind: bool", "kind: integer", 1),
			wants:    []string{"apps.alpha.questions[1].kind", "integer"},
		},
		{
			name:     "missing kind",
			manifest: strings.Replace(miniManifest, "        kind: bool\n", "", 1),
			wants:    []string{"questions[1].kind", "missing"},
		},
		{
			name:     "unknown field",
			manifest: strings.Replace(miniManifest, "kind: bool", "kind: bool\n        prmopt: log everything", 1),
			wants:    []string{"prmopt"},
		},
		{
			name:     "unknown top-level field",
			manifest: "version: 2\n" + miniManifest,
			wants:    []string{"version"},
		},
		{
			name:     "key not in the schema",
			manifest: strings.Replace(miniManifest, "key: apps.alpha.greeting", "key: apps.alpha.nonsense", 1),
			wants:    []string{"apps.alpha.nonsense", "not a key in values.schema.json"},
		},
		{
			name:     "schema type mismatch",
			manifest: strings.Replace(miniManifest, "      - key: apps.alpha.verbose\n        prompt: log everything\n        kind: bool\n        default: false", "      - key: apps.alpha.verbose\n        prompt: log everything\n        kind: string\n        default: hi", 1),
			wants:    []string{"apps.alpha.questions[1].key", "is a boolean", "string question"},
		},
		{
			name:     "default type",
			manifest: strings.Replace(miniManifest, "default: false", `default: "no"`, 1),
			wants:    []string{"apps.alpha.questions[1].default", "true or false"},
		},
		{
			name:     "duplicate key",
			manifest: strings.Replace(miniManifest, "key: apps.alpha.verbose", "key: apps.alpha.greeting", 1),
			wants:    []string{"apps.alpha.questions[1].key", "asked twice"},
		},
		{
			name:     "app values.yaml does not have",
			manifest: miniManifest + "  gamma:\n    description: not an app\n    instances: false\n    default: false\n    questions: []\n",
			wants:    []string{"apps.gamma", "values.yaml has no such app"},
		},
		{
			name:     "app without an entry",
			manifest: miniManifest[:strings.Index(miniManifest, "  beta:")],
			wants:    []string{"apps.beta", "values.yaml has this app"},
		},
		{
			name:     "instances disagree with values.yaml",
			manifest: strings.Replace(miniManifest, "instances: true", "instances: false", 1),
			wants:    []string{"apps.beta.instances"},
		},
		{
			name:     "no description",
			manifest: strings.Replace(miniManifest, "description: the first app", `description: ""`, 1),
			wants:    []string{"apps.alpha.description", "is empty"},
		},
		{
			name:     "list entry key",
			manifest: strings.Replace(miniManifest, "key: global.tz", "key: apps.beta.instances[0].suffix", 1),
			wants:    []string{"apps.beta.questions[0].key"},
		},
		{
			name:     "empty prompt",
			manifest: strings.Replace(miniManifest, "prompt: timezone", `prompt: ""`, 1),
			wants:    []string{"apps.beta.questions[0].prompt", "is empty"},
		},
		{
			name:     "second document",
			manifest: miniManifest + "---\napps: {}\n",
			wants:    []string{"more than one YAML document"},
		},
		{
			name:     "empty manifest",
			manifest: "",
			wants:    []string{"is empty"},
		},
		{
			name:     "no apps key",
			manifest: "apps: {}\n",
			wants:    []string{"names no apps"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMini(t, tc.manifest)
			if err == nil {
				t.Fatalf("the manifest was accepted:\n%s", tc.manifest)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not name %q: %v", want, err)
				}
			}
		})
	}
}

// TestProblemsComeInFileOrder pins the whole problem list for one manifest: the
// apps in the manifest's order, each app's description, instances and questions
// in turn, and within a question the prompt, the default, the kind and the key.
// The table above looks for one problem at a time, so it cannot see a refactor
// that reorders the checks or rewords a message.
//
// The two grant problems are compared by their opening words only: the tail of
// each is internal/seedbox's own reason for the rule, which its tests pin, so a
// reworded reason should not fail here.
func TestProblemsComeInFileOrder(t *testing.T) {
	values, schema := mini(t)
	_, err := Parse("catalog.yaml", []byte(brokenManifest), values, schema)
	if err == nil {
		t.Fatal("the manifest was accepted")
	}
	var listed *Error
	if !errors.As(err, &listed) {
		t.Fatalf("error = %T (%v), want a *catalog.Error", err, err)
	}
	if listed.Path != "catalog.yaml" {
		t.Fatalf("path = %q, want catalog.yaml", listed.Path)
	}
	want := []string{
		"apps.gamma: values.yaml has no such app; every entry here must name one",
		"apps.alpha.description: is empty; init shows it when it asks which apps the box has",
		"apps.alpha.instances: says the app takes instances, but values.yaml gives it no `instances` list",
		"apps.alpha.questions[0].prompt: is empty",
		"apps.alpha.questions[0].default: a string question takes text, not a number",
		"apps.alpha.questions[1].default: a bool question takes true or false, not text",
		"apps.alpha.questions[2].key: apps.alpha.nonsense is not a key in values.schema.json",
		`apps.alpha.questions[3].kind: "whisper" is not a kind box knows (string, secret, bool, transient)`,
		"apps.beta.instances: values.yaml gives the app an `instances` list, so this must be true",
		"apps.beta.questions[0].key: apps.beta.greeting is not a key in values.schema.json",
		"apps.beta.questions[1].key: apps.beta.greeting is asked twice (also questions[0])",
		"apps.beta.questions[2].key: gateway.host is set by the grant (",
		"apps.beta.questions[3].key: apps.beta.instances[0].suffix names a list entry; a question answers one key",
		"apps.beta.questions[3].key: apps.beta.instances[0].suffix is set by the grant (",
	}
	if len(listed.Problems) != len(want) {
		t.Fatalf("problems = %d, want %d:\n%s", len(listed.Problems), len(want), strings.Join(listed.Problems, "\n"))
	}
	for i, prefix := range want {
		if !strings.HasPrefix(listed.Problems[i], prefix) {
			t.Fatalf("problems[%d] = %q, want it to start with %q\nall:\n%s",
				i, listed.Problems[i], prefix, strings.Join(listed.Problems, "\n"))
		}
	}
}

// TestRealChartManifestLoads is the one test that reads the shipped chart: the
// manifest and the chart's own values and schema have to agree, and a new app
// under `apps:` with no entry here fails here rather than in init.
func TestRealChartManifestLoads(t *testing.T) {
	cat, err := Load("../../charts/s5n")
	if err != nil {
		t.Fatalf("load the chart's catalog: %v", err)
	}
	if len(cat.Apps()) == 0 {
		t.Fatal("the chart's catalog is empty")
	}
	for _, app := range cat.Apps() {
		if app.Description == "" {
			t.Fatalf("apps.%s has no description", app.Name)
		}
	}
	qbit, ok := cat.App("qbittorrent")
	if !ok || len(qbit.Stored()) == 0 {
		t.Fatalf("qbittorrent = %+v, want at least one stored question", qbit)
	}
	plex, ok := cat.App("plex")
	if !ok {
		t.Fatal("plex is not in the catalog")
	}
	tr := plex.Transient()
	if len(tr) != 1 {
		t.Fatalf("plex transient questions = %d, want the claim token", len(tr))
	}
	if tr[0].Key != "secrets.plexClaimToken" || !tr[0].Optional {
		t.Fatalf("plex transient question = %+v, want secrets.plexClaimToken, optional", tr[0])
	}
	// The two apps whose UI cannot live under a prefix: the chart serves them
	// at a host of their own, and box is what resolves and certifies those
	// names.
	if got := strings.Join(cat.Hosted(), ","); got != "plex,seerr" {
		t.Fatalf("hosted apps = %s, want plex,seerr", got)
	}
	for _, name := range []string{"plex", "seerr"} {
		app, _ := cat.App(name)
		if app.Exposure != ExposureHost {
			t.Fatalf("apps.%s exposure = %q, want host", name, app.Exposure)
		}
	}
	// Everything else must be a stored answer: a transient question is the
	// exception, and the loader is what keeps it one.
	for _, app := range cat.Apps() {
		if len(app.Transient()) > 1 {
			t.Fatalf("apps.%s has %d transient questions; the kind is meant to be rare", app.Name, len(app.Transient()))
		}
	}
	// The apps that sign in through the login's provider, in manifest order:
	// the clients box registers for a box with both installed. A new one is an
	// entry here, and the chart wires it from the same flag.
	if got := strings.Join(cat.OIDCApps([]string{"qui", "autobrr", "maintainerr", "sonarr"}), ","); got != "qui,autobrr" {
		t.Fatalf("OIDC apps = %s, want qui,autobrr (only the catalog's flag makes a client)", got)
	}
	for _, name := range []string{"qui", "autobrr"} {
		app, _ := cat.App(name)
		if !app.OIDC {
			t.Fatalf("apps.%s is not an OIDC client", name)
		}
	}
	if app, _ := cat.App("maintainerr"); app.OIDC {
		t.Fatal("apps.maintainerr is marked as an OIDC client")
	}

	// The opt-in apps: off unless the operator says so, no instances, and
	// nothing to ask at init. A box that installs the defaults must not get
	// them, so `default: false` is the assertion that matters most here.
	for _, name := range []string{"qui", "autobrr", "maintainerr"} {
		app, ok := cat.App(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if app.Default || app.Instances {
			t.Fatalf("apps.%s = %+v, want an opt-in app with no instances", name, app)
		}
		if len(app.Questions) != 0 {
			t.Fatalf("apps.%s asks %d questions, want none", name, len(app.Questions))
		}
	}
}

func TestLoadNamesAMissingManifest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"values.yaml", "values.schema.json"} {
		data, err := os.ReadFile(filepath.Join("testdata/mini", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), FileName) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("error = %v, want it to name the missing file and the chart", err)
	}
}

// TestOversizeManifestIsRefused: the cap is a refusal, never a truncated read.
func TestOversizeManifestIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"values.yaml", "values.schema.json"} {
		data, err := os.ReadFile(filepath.Join("testdata/mini", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	big := append([]byte(miniManifest), []byte(strings.Repeat("# padding\n", MaxManifest/10))...)
	if err := os.WriteFile(filepath.Join(dir, FileName), big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want a size refusal", err)
	}
}

// TestRefOutsideTheSchemaIsRefused: a question must be checked against the
// schema the chart shipped, so a schema that reaches outside itself is a
// refusal, not something to chase.
func TestRefOutsideTheSchemaIsRefused(t *testing.T) {
	values, _ := mini(t)
	schema := `{"type":"object","properties":{"apps":{"type":"object","properties":{"alpha":{"$ref":"https://example.com/schema.json"}}}}}`
	manifest := strings.Replace(miniManifest, "key: apps.alpha.greeting", "key: apps.alpha.greeting", 1)
	_, err := Parse("catalog.yaml", []byte(manifest), values, []byte(schema))
	if err == nil || !strings.Contains(err.Error(), "only follows") {
		t.Fatalf("error = %v, want a refusal of the external reference", err)
	}
}
