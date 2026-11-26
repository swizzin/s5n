package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// miniManifest is a valid manifest for testdata/mini. Every hostile case is this
// text with one thing broken, so the failure names the line a contributor got
// wrong.
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

// brokenManifest breaks several things at once, in two apps: TestProblemsComeInFileOrder
// pins the ordering of its problem list.
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

// A manifest that says nothing about exposure serves every app under a path, and
// `host` is what box builds its list of names from.
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

// An unknown exposure would render the app's route under a path nobody meant.
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

// A host of its own needs one name to put before the domain, which instances
// cannot give it.
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

// An entry that says nothing sits behind the box's one login, and `login: own` is
// kept: a caller never sees an empty value either way.
func TestLoginDefaultsToThePortalAndOwnIsKept(t *testing.T) {
	cat, err := parseMini(t, miniManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	alpha, _ := cat.App("alpha")
	if alpha.Login != LoginPortal {
		t.Fatalf("apps.alpha login = %q, want the portal when the manifest says nothing", alpha.Login)
	}
	if got := cat.SelfAuthenticating(); len(got) != 0 {
		t.Fatalf("self-authenticating = %v, want none", got)
	}

	own := strings.Replace(miniManifest,
		"    description: the first app\n",
		"    description: the first app\n    login: own\n", 1)
	cat, err = parseMini(t, own)
	if err != nil {
		t.Fatalf("parse with login own: %v", err)
	}
	if alpha, _ = cat.App("alpha"); alpha.Login != LoginOwn {
		t.Fatalf("apps.alpha login = %q, want own", alpha.Login)
	}
	if beta, _ := cat.App("beta"); beta.Login != LoginPortal {
		t.Fatalf("apps.beta login = %q, want the portal", beta.Login)
	}
	if got := strings.Join(cat.SelfAuthenticating(), ","); got != "alpha" {
		t.Fatalf("self-authenticating = %s, want alpha", got)
	}

	portal := strings.Replace(miniManifest,
		"    description: the first app\n",
		"    description: the first app\n    login: portal\n", 1)
	cat, err = parseMini(t, portal)
	if err != nil {
		t.Fatalf("parse with login portal: %v", err)
	}
	if alpha, _ = cat.App("alpha"); alpha.Login != LoginPortal {
		t.Fatalf("apps.alpha login = %q, want portal", alpha.Login)
	}
}

// An unknown login decides whether a password stands in front of the app, which
// is not a typo to guess at.
func TestUnknownLoginIsNamed(t *testing.T) {
	manifest := strings.Replace(miniManifest,
		"    description: the first app\n",
		"    description: the first app\n    login: sso\n", 1)
	_, err := parseMini(t, manifest)
	if err == nil {
		t.Fatal("an unknown login was accepted")
	}
	if !strings.Contains(err.Error(), `apps.alpha.login: "sso" is not portal or own`) {
		t.Fatalf("error = %v, want it to name the entry and both values", err)
	}
}

// realChart is charts/s5n's catalog with its values and schema, read the way Load
// reads them.
func realChart(t *testing.T) (manifest, values, schema []byte) {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("../../charts/s5n", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return read(FileName), read("values.yaml"), read("values.schema.json")
}

// A release published before `login:` carries none, and its Plex, Seerr and
// Tautulli still sign their users in themselves: the frozen set keeps that bypass
// under a newer box.
func TestLoginAbsentKeepsTheLegacyAppsSelfAuthenticating(t *testing.T) {
	manifest, values, schema := realChart(t)
	// A release that predates the key carries neither spelling.
	old := []byte(strings.NewReplacer("    login: own\n", "", "    login: portal\n", "").Replace(string(manifest)))
	if string(old) == string(manifest) {
		t.Fatal("the chart's catalog carries no `login:` key to strip, so this fixture no longer looks like an older release")
	}
	cat, err := Parse("catalog.yaml", old, values, schema)
	if err != nil {
		t.Fatalf("parse an older chart's manifest: %v", err)
	}
	if got := strings.Join(cat.SelfAuthenticating(), ","); got != "plex,tautulli,seerr" {
		t.Fatalf("self-authenticating = %s, want plex,tautulli,seerr", got)
	}
}

// An explicit value wins over the legacy set, so a later chart can bring one of
// those three behind the login without a box release.
func TestExplicitPortalBeatsTheLegacyName(t *testing.T) {
	manifest, values, schema := realChart(t)
	// Plex is the first `login: own` in the manifest.
	edited := strings.Replace(string(manifest), "    login: own\n", "    login: portal\n", 1)
	cat, err := Parse("catalog.yaml", []byte(edited), values, schema)
	if err != nil {
		t.Fatalf("parse with plex on the portal: %v", err)
	}
	plex, _ := cat.App("plex")
	if plex.Login != LoginPortal {
		t.Fatalf("apps.plex login = %q, want the explicit portal", plex.Login)
	}
	if got := strings.Join(cat.SelfAuthenticating(), ","); got != "seerr,jellyfin" {
		t.Fatalf("self-authenticating = %s, want seerr,jellyfin", got)
	}
}

// Tautulli's name is in the legacy set, so an entry that simply dropped the key
// would read as `login: own` and undo the chart's decision: the value has to be
// written out.
func TestTheChartsTautulliIsBehindThePortal(t *testing.T) {
	manifest, values, schema := realChart(t)
	cat, err := Parse("catalog.yaml", manifest, values, schema)
	if err != nil {
		t.Fatalf("parse the chart's catalog: %v", err)
	}
	tautulli, ok := cat.App("tautulli")
	if !ok {
		t.Fatal("the chart's catalog has no tautulli")
	}
	if tautulli.Login != LoginPortal {
		t.Fatalf("apps.tautulli login = %q, want the box's portal (an absent value means own for that name)", tautulli.Login)
	}
	if strings.Contains(strings.Join(cat.SelfAuthenticating(), ","), "tautulli") {
		t.Fatalf("tautulli is still self-authenticating: %v", cat.SelfAuthenticating())
	}
}

// Every refusal names the entry that is wrong: a manifest is untrusted input to a
// root process, and "the catalog is broken" would leave the contributor guessing.
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

// Pins the whole problem list for one manifest: apps in order, then description,
// instances and questions, and within a question the prompt, default, kind and
// key. The table above cannot see a refactor that reorders the checks.
//
// The two grant problems are compared by their opening words only: the tail is
// internal/seedbox's reason, which its own tests pin.
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

// The one test that reads the shipped chart: its manifest and values must agree,
// and a new app under `apps:` with no entry fails here rather than in init.
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
	// qBittorrent asks nothing: its WebUI skips the login for the release's
	// addresses and the edge's login is the one a person uses. The values key
	// stays (the init container still seals a username).
	qbit, ok := cat.App("qbittorrent")
	if !ok {
		t.Fatal("qbittorrent is not in the catalog")
	}
	if got := len(qbit.Stored()) + len(qbit.Transient()); got != 0 {
		t.Fatalf("qbittorrent asks %d questions, want none", got)
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
	// An absolute /api path or assets built from the site root cannot live under a
	// prefix, so the chart serves them at a host of their own.
	if got := strings.Join(cat.Hosted(), ","); got != "plex,seerr,jellyfin,pbh" {
		t.Fatalf("hosted apps = %s, want plex,seerr,jellyfin,pbh", got)
	}
	for _, name := range []string{"plex", "seerr", "jellyfin", "pbh"} {
		app, _ := cat.App(name)
		if app.Exposure != ExposureHost {
			t.Fatalf("apps.%s exposure = %q, want host", name, app.Exposure)
		}
	}
	// A transient question is the exception: the kind is meant to be rare.
	for _, app := range cat.Apps() {
		if len(app.Transient()) > 1 {
			t.Fatalf("apps.%s has %d transient questions; the kind is meant to be rare", app.Name, len(app.Transient()))
		}
	}
	// The clients box registers for a box with these installed, in manifest order:
	// a box that already exists registers one when its next run takes a chart with
	// the app enabled, because the login half of every root apply writes a client
	// it does not find.
	if got := strings.Join(cat.OIDCApps([]string{"qui", "autobrr", "filebrowser", "maintainerr", "sonarr"}), ","); got != "qui,autobrr,filebrowser" {
		t.Fatalf("OIDC apps = %s, want qui,autobrr,filebrowser (only the catalog's flag makes a client)", got)
	}
	for _, name := range []string{"qui", "autobrr", "filebrowser"} {
		app, _ := cat.App(name)
		if !app.OIDC {
			t.Fatalf("apps.%s is not an OIDC client", name)
		}
	}
	if app, _ := cat.App("maintainerr"); app.OIDC {
		t.Fatal("apps.maintainerr is marked as an OIDC client")
	}

	// The opt-in apps: off unless the operator says so, no instances, nothing
	// asked. `default: false` is the assertion that matters most.
	for _, name := range []string{"qui", "autobrr", "filebrowser", "maintainerr"} {
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

// A question is checked against the schema the chart shipped, so a schema that
// reaches outside itself is refused, not chased.
func TestRefOutsideTheSchemaIsRefused(t *testing.T) {
	values, _ := mini(t)
	schema := `{"type":"object","properties":{"apps":{"type":"object","properties":{"alpha":{"$ref":"https://example.com/schema.json"}}}}}`
	manifest := strings.Replace(miniManifest, "key: apps.alpha.greeting", "key: apps.alpha.greeting", 1)
	_, err := Parse("catalog.yaml", []byte(manifest), values, []byte(schema))
	if err == nil || !strings.Contains(err.Error(), "only follows") {
		t.Fatalf("error = %v, want a refusal of the external reference", err)
	}
}

// `calls` is the list the chart inverts for the NetworkPolicy, so every name must
// be an app this chart has, and an app cannot list itself or a peer twice.
func TestCallsAreReadAndChecked(t *testing.T) {
	withCalls := func(calls string) string {
		return strings.Replace(miniManifest, "    default: true\n    questions:",
			"    default: true\n    calls: "+calls+"\n    questions:", 1)
	}
	cat, err := parseMini(t, withCalls("[beta]"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	alpha, _ := cat.App("alpha")
	if got := strings.Join(alpha.Calls, ","); got != "beta" {
		t.Fatalf("apps.alpha.calls = %q, want beta", got)
	}
	if beta, _ := cat.App("beta"); len(beta.Calls) != 0 {
		t.Fatalf("apps.beta.calls = %v, want none: an omitted list is an app that dials nothing", beta.Calls)
	}

	cases := []struct {
		name  string
		calls string
		wants []string
	}{
		{
			name:  "an app this chart does not have",
			calls: "[beta, gamma]",
			wants: []string{"apps.alpha.calls[1]", "gamma", "not an app"},
		},
		{
			name:  "itself",
			calls: "[alpha]",
			wants: []string{"apps.alpha.calls[0]", "cannot call itself"},
		},
		{
			name:  "the same peer twice",
			calls: "[beta, beta]",
			wants: []string{"apps.alpha.calls[1]", "listed twice"},
		},
		{
			name:  "an empty name",
			calls: `[""]`,
			wants: []string{"apps.alpha.calls[0]", "is empty"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseMini(t, withCalls(c.calls))
			if err == nil {
				t.Fatalf("calls %s was accepted", c.calls)
			}
			for _, want := range c.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

// `snapshot` is what box copies before an update and what a rollback deletes, so
// an absolute glob, one that walks up, or one that is not a pattern is refused at
// load. The messages are compared whole, so a contributor can act on the line.
func TestSnapshotGlobsAreReadAndChecked(t *testing.T) {
	withSnapshot := func(globs string) string {
		return strings.Replace(miniManifest, "    default: true\n    questions:",
			"    default: true\n    snapshot: "+globs+"\n    questions:", 1)
	}
	cat, err := parseMini(t, withSnapshot("[ app.db , db/db.sqlite3-wal ]"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	alpha, _ := cat.App("alpha")
	// The list is trimmed in place, so the copier runs what the loader checked.
	if got := strings.Join(alpha.Snapshot, ","); got != "app.db,db/db.sqlite3-wal" {
		t.Fatalf("apps.alpha.snapshot = %q, want the trimmed list", got)
	}
	if beta, _ := cat.App("beta"); len(beta.Snapshot) != 0 {
		t.Fatalf("apps.beta.snapshot = %v, want none: an omitted key is an app with nothing to restore", beta.Snapshot)
	}

	cases := []struct {
		name  string
		globs string
		want  string
	}{
		{
			name:  "empty",
			globs: `[""]`,
			want:  "apps.alpha.snapshot[0]: is empty; name a file under the app's config directory, or drop the entry",
		},
		{
			name:  "absolute",
			globs: `[/etc/passwd]`,
			want:  "apps.alpha.snapshot[0]: /etc/passwd: is absolute; an app's state is somewhere under its config directory",
		},
		{
			name:  "walks up",
			globs: `[../sonarr/sonarr.db]`,
			want:  `apps.alpha.snapshot[0]: ../sonarr/sonarr.db: has a ".." segment; a snapshot never leaves the app's config directory`,
		},
		{
			name:  "not clean",
			globs: `[./app.db]`,
			want:  `apps.alpha.snapshot[0]: ./app.db: is not a clean relative path (write "app.db")`,
		},
		{
			name:  "not a pattern",
			globs: `["app[.db"]`,
			want:  "apps.alpha.snapshot[0]: app[.db: is not a path.Match pattern: syntax error in pattern",
		},
		{
			name:  "twice",
			globs: `[app.db, app.db]`,
			want:  "apps.alpha.snapshot[1]: app.db is listed twice (also snapshot[0])",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMini(t, withSnapshot(tc.globs))
			if err == nil {
				t.Fatalf("snapshot %s was accepted", tc.globs)
			}
			want := "catalog.yaml:\n  " + tc.want
			if err.Error() != want {
				t.Fatalf("error =\n%s\nwant\n%s", err, want)
			}
		})
	}
}

// Pins what each entry copies: the chart's statement of what an image upgrade can
// rewrite beyond going back. An app added without one fails here rather than
// shipping a snapshot that restores nothing.
func TestRealChartSnapshots(t *testing.T) {
	cat, err := Load("../../charts/s5n")
	if err != nil {
		t.Fatalf("load the chart's catalog: %v", err)
	}
	want := map[string]string{
		"qbittorrent": "qBittorrent/qBittorrent.conf",
		"sonarr":      "sonarr.db,sonarr.db-wal,sonarr.db-shm,config.xml",
		"radarr":      "radarr.db,radarr.db-wal,radarr.db-shm,config.xml",
		"lidarr":      "lidarr.db,lidarr.db-wal,lidarr.db-shm,config.xml",
		"bazarr":      "config/config.yaml,db/bazarr.db,db/bazarr.db-wal,db/bazarr.db-shm",
		"sabnzbd":     "sabnzbd.ini",
		"filebrowser": "database.sqlite,database.sqlite-wal,database.sqlite-shm",
		"prowlarr":    "prowlarr.db,prowlarr.db-wal,prowlarr.db-shm,config.xml",
		"plex": "Library/Application Support/Plex Media Server/Preferences.xml," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db-wal," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db-shm," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-wal," +
			"Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-shm",
		"tautulli":    "config.ini,tautulli.db,tautulli.db-wal,tautulli.db-shm",
		"seerr":       "settings.json,db/db.sqlite3,db/db.sqlite3-wal,db/db.sqlite3-shm",
		"qui":         "config.toml,qui.db,qui.db-wal,qui.db-shm",
		"autobrr":     "config.toml,autobrr.db,autobrr.db-wal,autobrr.db-shm",
		"pbh":         "config.yml,persist/peerbanhelper-nt.db,persist/peerbanhelper-nt.db-wal,persist/peerbanhelper-nt.db-shm",
		"maintainerr": "maintainerr.sqlite,maintainerr.sqlite-wal,maintainerr.sqlite-shm",
		"jellyfin":    "config/*.xml,data/jellyfin.db,data/jellyfin.db-wal,data/jellyfin.db-shm",
	}
	for name, globs := range want {
		app, ok := cat.App(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if got := strings.Join(app.Snapshot, ","); got != globs {
			t.Fatalf("apps.%s.snapshot = %s, want %s", name, got, globs)
		}
	}
	for _, app := range cat.Apps() {
		if _, ok := want[app.Name]; !ok {
			t.Fatalf("apps.%s has no snapshot list here: decide what an image upgrade can rewrite, add it to the chart and to this table", app.Name)
		}
	}
}

// The catalog's lists are what the chart's NetworkPolicy is derived from, so this
// pins the edges a shipped release got wrong (Seerr into the Arrs, everything
// Maintainerr calls, autobrr into qui, qui into the Arrs). A name here is also an
// app that must exist.
func TestRealChartCallsAreTheReleaseEdges(t *testing.T) {
	cat, err := Load("../../charts/s5n")
	if err != nil {
		t.Fatalf("load the chart's catalog: %v", err)
	}
	want := map[string]string{
		"sonarr":      "prowlarr,qbittorrent,sabnzbd",
		"radarr":      "prowlarr,qbittorrent,sabnzbd",
		"lidarr":      "prowlarr,qbittorrent,sabnzbd",
		"bazarr":      "radarr,sonarr",
		"prowlarr":    "lidarr,radarr,sonarr",
		"tautulli":    "plex",
		"seerr":       "plex,radarr,sonarr",
		"qui":         "qbittorrent,radarr,sonarr",
		"autobrr":     "lidarr,qbittorrent,qui,radarr,sonarr",
		"maintainerr": "plex,qbittorrent,radarr,seerr,sonarr,tautulli",
	}
	for name, edges := range want {
		app, ok := cat.App(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		got := append([]string(nil), app.Calls...)
		sort.Strings(got)
		if strings.Join(got, ",") != edges {
			t.Fatalf("apps.%s.calls = %v, want %s", name, got, edges)
		}
	}
	// The two apps that are only ever called.
	for _, name := range []string{"qbittorrent", "plex"} {
		if app, _ := cat.App(name); len(app.Calls) != 0 {
			t.Fatalf("apps.%s.calls = %v, want none: it is a callee, not a caller", name, app.Calls)
		}
	}
	// Every name resolves to a catalog app and never to its own entry: the same
	// rules Load enforces, asserted over the shipped file.
	for _, app := range cat.Apps() {
		for _, call := range app.Calls {
			if call == app.Name {
				t.Fatalf("apps.%s calls itself", app.Name)
			}
			if _, ok := cat.App(call); !ok {
				t.Fatalf("apps.%s calls %q, which is not a catalog app", app.Name, call)
			}
		}
	}
}
