// Package catalog reads the chart's catalog manifest: what a seedbox can run,
// and the setup questions `box init` asks for each app.
//
// The manifest is chart-owned, beside variants.yaml: it ships inside the chart
// archive, values cannot override it, and box reads it from the chart release
// it is installing. That is what makes adding an app a chart change: box names
// no app and asks no app-specific question of its own, so a new app arrives as
// one manifest entry plus its values key, and box only changes when an app
// needs a new *kind* of question.
//
// The file is untrusted input to a process that may run as root, and a wrong
// entry is a prompt that writes to a key root did not mean to give away. So
// Load is strict: unknown fields are refused rather than ignored, the file has
// a size cap, every app under `apps:` in values.yaml has exactly one entry and
// vice versa, and every question key must be a leaf of values.schema.json.
// Above all it must not be a grant key: domain, paths, uid and the app list are
// box's own questions (internal/seedbox's grant rules), because a contributor
// who could ask for one of them could move a box's data or hand it another
// box's downloads through a prompt the user thinks is tuning.
package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/seedbox"
)

// FileName is the manifest inside a chart directory, next to values.yaml.
const FileName = "catalog.yaml"

// MaxManifest bounds the manifest box will read. The real one is a few KB; the
// cap is here because the file arrives inside a chart and box may be running as
// root, so "read whatever this file says" needs a limit.
const MaxManifest = 256 << 10

// Kind is one kind of setup question. The list is deliberately short: a new
// kind is the only catalog change that needs a box release.
type Kind string

const (
	// KindString is free text.
	KindString Kind = "string"
	// KindSecret is text that must not appear on screen or in an apply diff:
	// box reads it without echoing, and writes it only to the values file.
	KindSecret Kind = "secret"
	// KindBool is a yes/no answer.
	KindBool Kind = "bool"
	// KindTransient is a secret that outlives no file. It is asked last, after
	// apply's confirm, handed to that one apply and never written to values or
	// the grant. The Plex claim token is the case: it expires about four
	// minutes after plex.tv/claim issues it, so it can only be supplied at
	// install time. The key still exists in values.schema.json, because the
	// answer is passed to the chart; it is just never stored by box.
	KindTransient Kind = "transient"
)

// Question is one setup question for an app.
type Question struct {
	// Key is the chart values path the answer is written to.
	Key string `yaml:"key"`
	// Prompt is the line box init shows.
	Prompt string `yaml:"prompt"`
	// Help is one optional line under the prompt.
	Help string `yaml:"help,omitempty"`
	// Kind decides how the answer is asked and where it goes.
	Kind Kind `yaml:"kind"`
	// Default is the starting answer: a string for string, secret and
	// transient, a bool for bool. Nil means no default.
	Default any `yaml:"default,omitempty"`
	// Optional says an empty answer is allowed.
	Optional bool `yaml:"optional,omitempty"`
}

// Transient reports whether the answer must not be written to a file.
func (q Question) Transient() bool { return q.Kind == KindTransient }

// Exposure is how the chart serves an app through the seedbox's Gateway: under
// a path on the box's own host name, or at a host of its own.
type Exposure string

const (
	// ExposurePath is the default: `https://<domain>/<name>`.
	ExposurePath Exposure = "path"
	// ExposureHost is `https://<app>.<domain>`, with its own listener and
	// certificate. An app whose UI writes absolute asset paths cannot live
	// under a prefix, so the chart serves it at a name of its own.
	ExposureHost Exposure = "host"
)

// Login is how a person gets into an app: through the box's one login, or with
// the app's own sign-in.
type Login string

const (
	// LoginPortal is the default: the app sits behind the box's one login, so
	// the edge asks for a session before it serves anything under it.
	LoginPortal Login = "portal"
	// LoginOwn is an app that signs its users in itself, on the name or the
	// path it is served at. Its host, or its whole path prefix, bypasses the
	// login: a media server that issues its own tokens, or a tool reached by
	// the same players and APIs.
	LoginOwn Login = "own"
)

// legacyOwnLogin are the apps chart releases published before `login:` carried
// without saying so: their Plex, Seerr and Tautulli bypassed the login, and a
// newer box must keep giving them that under those charts. The set is frozen --
// a new app says `login: own` in the catalog, never here -- and it goes when
// box's floor passes the first chart release that carries the key.
var legacyOwnLogin = map[string]bool{
	"plex":     true,
	"seerr":    true,
	"tautulli": true,
}

// App is one catalog entry.
type App struct {
	// Name is the app's key under `apps:` in values.yaml.
	Name string `yaml:"-"`
	// Description is one line, shown beside the app when init asks which apps
	// the box should have.
	Description string `yaml:"description"`
	// Instances says the app takes more than one instance (the Arrs do). The
	// instances themselves are the grant's, not a values question.
	Instances bool `yaml:"instances"`
	// Default says init preselects the app.
	Default bool `yaml:"default"`
	// Exposure is how the chart serves it; empty means ExposurePath.
	Exposure Exposure `yaml:"exposure"`
	// Login is how a person gets in: behind the box's one login, or with the
	// app's own. Empty in the manifest means Load resolves it to LoginPortal,
	// or to LoginOwn for the few apps a chart older than this key shipped.
	Login Login `yaml:"login"`
	// OIDC says the app signs in through the login's own OIDC provider: the
	// chart gives it the issuer and a client secret, and the login registers a
	// client for it in this box's namespace. It is the chart's decision, not
	// box's -- a second client later is one entry here and nothing in box
	// beyond reading this flag.
	OIDC bool `yaml:"oidc"`
	// Calls lists the in-release apps this one connects to, and it is the only
	// such list: the chart derives the release's NetworkPolicy from it, each
	// app's ingress admitting exactly the enabled apps that name it. So the
	// consequence of a missing entry is a call the callee refuses, which is why
	// Load checks every name against the catalog and refuses a self-reference.
	// "sonarr"/"radarr" mean every instance of that Arr. An app that dials
	// nothing inside the release leaves the list empty.
	Calls []string `yaml:"calls"`
	// Questions are asked in this order, after the app is selected.
	Questions []Question `yaml:"questions"`
}

// Transient returns the app's transient questions, in order. They are asked
// after apply's confirm, not with the others.
func (a App) Transient() []Question {
	var out []Question
	for _, q := range a.Questions {
		if q.Transient() {
			out = append(out, q)
		}
	}
	return out
}

// Stored returns the app's questions whose answers go into the values file.
func (a App) Stored() []Question {
	var out []Question
	for _, q := range a.Questions {
		if !q.Transient() {
			out = append(out, q)
		}
	}
	return out
}

// Catalog is a chart's manifest, validated against that chart's own values.
type Catalog struct {
	apps   []App // manifest order
	byName map[string]int
}

// Apps returns every entry in the order the manifest declares, which is the
// order box shows them in.
func (c *Catalog) Apps() []App {
	out := make([]App, len(c.apps))
	copy(out, c.apps)
	return out
}

// OIDCApps is the enabled apps that sign in through the login, in manifest
// order. enabled is the names init selected; an app the catalog marks and this
// box did not install is not a client, and a box with none is not a provider.
func (c *Catalog) OIDCApps(enabled []string) []string {
	on := make(map[string]bool, len(enabled))
	for _, name := range enabled {
		on[name] = true
	}
	var out []string
	for _, app := range c.apps {
		if app.OIDC && on[app.Name] {
			out = append(out, app.Name)
		}
	}
	return out
}

// App returns one entry.
func (c *Catalog) App(name string) (App, bool) {
	i, ok := c.byName[name]
	if !ok {
		return App{}, false
	}
	return c.apps[i], true
}

// Names returns the app names in manifest order.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.apps))
	for _, a := range c.apps {
		out = append(out, a.Name)
	}
	return out
}

// Defaults returns the apps init preselects, in manifest order.
func (c *Catalog) Defaults() []string {
	var out []string
	for _, a := range c.apps {
		if a.Default {
			out = append(out, a.Name)
		}
	}
	return out
}

// manifest is the file's shape. It is decoded with KnownFields, so a typo'd
// field is an error instead of a silently missing question.
type manifest struct {
	Apps map[string]manifestApp `yaml:"apps"`
}

type manifestApp struct {
	Description string     `yaml:"description"`
	Instances   bool       `yaml:"instances"`
	Default     bool       `yaml:"default"`
	Exposure    Exposure   `yaml:"exposure"`
	Login       Login      `yaml:"login"`
	OIDC        bool       `yaml:"oidc"`
	Calls       []string   `yaml:"calls"`
	Questions   []Question `yaml:"questions"`
}

// Error lists every problem with a manifest at once. A contributor who got the
// shape wrong wants the whole list, not one line per run.
type Error struct {
	Path     string
	Problems []string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Path)
	b.WriteString(":")
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p)
	}
	return b.String()
}

// Load reads and validates the catalog manifest in a chart directory, with
// values.yaml and values.schema.json from the same directory: the manifest is
// only meaningful against the chart it shipped with. `box init` reads a chart
// release instead — a pulled artifact's files are bytes, not a directory — and
// calls Parse with exactly those three files.
func Load(dir string) (*Catalog, error) {
	raw, err := readCapped(filepath.Join(dir, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("chart %s: no %s; box init builds its questions from it", dir, FileName)
		}
		return nil, fmt.Errorf("chart %s: %w", dir, err)
	}
	values, err := readCapped(filepath.Join(dir, "values.yaml"))
	if err != nil {
		return nil, fmt.Errorf("chart %s: values.yaml: %w", dir, err)
	}
	schema, err := readCapped(filepath.Join(dir, "values.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("chart %s: values.schema.json: %w", dir, err)
	}
	return Parse(filepath.Join(dir, FileName), raw, values, schema)
}

// Parse builds a catalog from the manifest bytes and the chart's values and
// schema. Load is Parse over a directory; the split is what lets the tests feed
// a hostile manifest without writing it into the chart.
func Parse(path string, raw, values, schema []byte) (*Catalog, error) {
	m, err := decodeManifest(path, raw)
	if err != nil {
		return nil, err
	}
	order, err := appOrder(raw)
	if err != nil {
		return nil, &Error{Path: path, Problems: []string{err.Error()}}
	}
	chartApps, err := valuesApps(values)
	if err != nil {
		return nil, fmt.Errorf("%s: values.yaml: %w", path, err)
	}
	sch, err := parseSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("%s: values.schema.json: %w", path, err)
	}

	problems := appSetProblems(m, order, chartApps)
	cat, appProblems := buildApps(m, order, chartApps, sch)
	problems = append(problems, appProblems...)
	if len(problems) > 0 {
		return nil, &Error{Path: path, Problems: problems}
	}
	return cat, nil
}

// decodeManifest decodes the manifest's first document into the shape the file
// is read as. Both of its refusals are about the file itself, before any entry
// is looked at, which is why they are their own step: unknown fields are a typo
// that would otherwise be a silently missing question, and a second document
// would be ignored by the decoder rather than read.
func decodeManifest(path string, raw []byte) (*manifest, error) {
	var m manifest
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{Path: path, Problems: []string{"is empty; a chart's catalog names its apps"}}
		}
		return nil, &Error{Path: path, Problems: []string{err.Error()}}
	}
	// A second document in the same file would be ignored by the decoder above,
	// so refuse it rather than silently reading only the first.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &Error{Path: path, Problems: []string{"has more than one YAML document"}}
	}
	return &m, nil
}

// appSetProblems checks the manifest's app set against the chart's: every
// manifest entry names an app the chart has, and every app the chart has is in
// the manifest. A workload nobody can configure is as wrong as a question for
// an app that does not exist.
func appSetProblems(m *manifest, order []string, chartApps map[string]chartApp) []string {
	var problems []string
	if len(m.Apps) == 0 {
		problems = append(problems, "names no apps; a chart's catalog describes every app under `apps:` in values.yaml")
	}
	for _, name := range order {
		if _, ok := m.Apps[name]; !ok {
			// appOrder reads the same document, so this cannot happen unless the
			// file has duplicate `apps:` keys, which yaml refuses first.
			problems = append(problems, fmt.Sprintf("apps.%s: declared twice", name))
		}
	}
	for _, name := range sortedKeys(m.Apps) {
		if _, ok := chartApps[name]; !ok {
			problems = append(problems, fmt.Sprintf("apps.%s: values.yaml has no such app; every entry here must name one", name))
		}
	}
	for _, name := range sortedKeys(chartApps) {
		if _, ok := m.Apps[name]; !ok {
			problems = append(problems, fmt.Sprintf("apps.%s: values.yaml has this app but the catalog does not; every app needs exactly one entry", name))
		}
	}
	return problems
}

// buildApps turns the manifest's entries into the catalog in manifest order,
// collecting every entry's problems. An entry with problems is still built:
// one run lists everything wrong with the file, and it is the problem list, not
// the build, that refuses the manifest.
func buildApps(m *manifest, order []string, chartApps map[string]chartApp, sch *schema) (*Catalog, []string) {
	cat := &Catalog{byName: make(map[string]int, len(order))}
	var problems []string
	seen := map[string]bool{}
	for _, name := range order {
		entry, ok := m.Apps[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		app := App{
			Name:        name,
			Description: strings.TrimSpace(entry.Description),
			Instances:   entry.Instances,
			Default:     entry.Default,
			Exposure:    entry.Exposure,
			Login:       entry.Login,
			OIDC:        entry.OIDC,
			Calls:       entry.Calls,
			Questions:   entry.Questions,
		}
		problems = append(problems, validateExposure(&app)...)
		problems = append(problems, validateLogin(&app)...)
		problems = append(problems, validateApp(&app, chartApps[name], chartApps, sch)...)
		cat.byName[name] = len(cat.apps)
		cat.apps = append(cat.apps, app)
	}
	return cat, problems
}

// validateExposure checks how the chart serves the app. An unknown value is a
// typo that would otherwise render the app's route under a path nobody meant,
// so it is refused by name like every other manifest mistake. An app that takes
// instances has no single name to put before the domain, so it cannot take a
// host of its own.
func validateExposure(app *App) []string {
	switch app.Exposure {
	case "", ExposurePath:
		app.Exposure = ExposurePath
		return nil
	case ExposureHost:
		if app.Instances {
			return []string{fmt.Sprintf("apps.%s.exposure: host needs one name, but this app takes instances; serve it under a path", app.Name)}
		}
		return nil
	default:
		return []string{fmt.Sprintf("apps.%s.exposure: %q is not %s or %s", app.Name, app.Exposure, ExposurePath, ExposureHost)}
	}
}

// Hosted returns the apps the chart serves at a host of their own, in manifest
// order. It is box's list of names to resolve and certify.
func (c *Catalog) Hosted() []string {
	var out []string
	for _, a := range c.apps {
		if a.Exposure == ExposureHost {
			out = append(out, a.Name)
		}
	}
	return out
}

// validateLogin checks how a person gets into the app, and resolves an absent
// key: every caller then has an answer, and only the catalog knows that an old
// chart's Plex, Seerr and Tautulli sign their users in themselves. An explicit
// value wins over that set, which is how a chart stops marking one of them.
func validateLogin(app *App) []string {
	switch app.Login {
	case "":
		if legacyOwnLogin[app.Name] {
			app.Login = LoginOwn
		} else {
			app.Login = LoginPortal
		}
		return nil
	case LoginPortal, LoginOwn:
		return nil
	default:
		return []string{fmt.Sprintf("apps.%s.login: %q is not %s or %s", app.Name, app.Login, LoginPortal, LoginOwn)}
	}
}

// SelfAuthenticating returns the apps that sign their users in themselves, in
// manifest order. The login gives each one a bypass instead of the one-factor
// rule, so a new app with its own sign-in is one manifest key and no box
// change.
func (c *Catalog) SelfAuthenticating() []string {
	var out []string
	for _, a := range c.apps {
		if a.Login == LoginOwn {
			out = append(out, a.Name)
		}
	}
	return out
}

// validateApp checks one manifest entry: its description (init shows it), the
// instances flag against the shape values.yaml gives the app, the apps it says
// it dials, and every question. The messages name the entry, because that is
// what a contributor edits.
func validateApp(app *App, chart chartApp, chartApps map[string]chartApp, sch *schema) []string {
	var problems []string
	if app.Description == "" {
		problems = append(problems, fmt.Sprintf("apps.%s.description: is empty; init shows it when it asks which apps the box has", app.Name))
	} else if strings.ContainsAny(app.Description, "\n\r") {
		problems = append(problems, fmt.Sprintf("apps.%s.description: must be one line", app.Name))
	}
	if app.Instances != chart.Instances {
		if app.Instances {
			problems = append(problems, fmt.Sprintf("apps.%s.instances: says the app takes instances, but values.yaml gives it no `instances` list", app.Name))
		} else {
			problems = append(problems, fmt.Sprintf("apps.%s.instances: values.yaml gives the app an `instances` list, so this must be true", app.Name))
		}
	}
	problems = append(problems, validateCalls(app, chartApps)...)
	return append(problems, validateQuestions(app, sch)...)
}

// validateCalls checks an entry's `calls` list. The chart inverts that list for
// the release's NetworkPolicy, so a name that is not a catalog app renders a
// peer selector nothing matches, a self-reference renders an app admitting
// itself, and a repeat renders the same peer twice: each one is a policy that
// says something its author did not mean, and each is refused by name here.
//
// The names are trimmed in place, so the catalog's App carries what the chart
// reads from the same file.
func validateCalls(app *App, chartApps map[string]chartApp) []string {
	var problems []string
	seen := map[string]int{}
	for i, call := range app.Calls {
		where := fmt.Sprintf("apps.%s.calls[%d]", app.Name, i)
		name := strings.TrimSpace(call)
		if name == "" {
			problems = append(problems, fmt.Sprintf("%s: is empty; name the in-release app it dials, or drop the entry", where))
			continue
		}
		app.Calls[i] = name
		if name == app.Name {
			problems = append(problems, fmt.Sprintf("%s: %s cannot call itself; a release's peers are other apps", where, name))
			continue
		}
		if j, dup := seen[name]; dup {
			problems = append(problems, fmt.Sprintf("%s: %s is listed twice (also calls[%d])", where, name, j))
			continue
		}
		seen[name] = i
		if _, ok := chartApps[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %q is not an app in this chart's catalog, so nothing would admit the call", where, name))
		}
	}
	return problems
}

// validateQuestions checks one app's questions. Every message names the entry
// by its position in the file, which is what a contributor edits.
func validateQuestions(app *App, sch *schema) []string {
	var problems []string
	seen := map[string]int{}
	for i, q := range app.Questions {
		where := fmt.Sprintf("apps.%s.questions[%d]", app.Name, i)
		key := strings.TrimSpace(q.Key)
		if key == "" {
			problems = append(problems, fmt.Sprintf("%s.key: is empty; it is the values path the answer is written to", where))
			continue
		}
		if j, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("%s.key: %s is asked twice (also questions[%d])", where, key, j))
			continue
		}
		seen[key] = i
		problems = append(problems, validateQuestionText(q, where)...)
		kindProblems, known := validateQuestionKind(q, where)
		problems = append(problems, kindProblems...)
		if !known {
			continue
		}
		problems = append(problems, validateQuestionKey(key, where, q.Kind, sch)...)
	}
	return problems
}

// validateQuestionText checks the two lines init shows: the prompt it asks and
// the help under it. Both are one line, because init draws them in a fixed
// frame.
func validateQuestionText(q Question, where string) []string {
	var problems []string
	if strings.TrimSpace(q.Prompt) == "" {
		problems = append(problems, fmt.Sprintf("%s.prompt: is empty", where))
	} else if strings.ContainsAny(q.Prompt, "\n\r") {
		problems = append(problems, fmt.Sprintf("%s.prompt: must be one line", where))
	}
	if strings.ContainsAny(q.Help, "\n\r") {
		problems = append(problems, fmt.Sprintf("%s.help: must be one line", where))
	}
	return problems
}

// validateQuestionKind checks the kind and the default's type, which the kind
// decides. known is false for a missing or unknown kind: every rule after it
// reads the kind, so the caller stops there rather than guess what the
// contributor meant.
func validateQuestionKind(q Question, where string) (problems []string, known bool) {
	switch q.Kind {
	case KindString, KindSecret, KindTransient:
		if q.Default != nil {
			if _, ok := q.Default.(string); !ok {
				problems = append(problems, fmt.Sprintf("%s.default: a %s question takes text, not %s", where, q.Kind, typeName(q.Default)))
			}
		}
	case KindBool:
		if q.Default != nil {
			if _, ok := q.Default.(bool); !ok {
				problems = append(problems, fmt.Sprintf("%s.default: a bool question takes true or false, not %s", where, typeName(q.Default)))
			}
		}
	case "":
		return []string{fmt.Sprintf("%s.kind: is missing; one of string, secret, bool, transient", where)}, false
	default:
		return []string{fmt.Sprintf("%s.kind: %q is not a kind box knows (string, secret, bool, transient)", where, q.Kind)}, false
	}
	return problems, true
}

// validateQuestionKey checks the key against the data contract: it must be a
// values path, not a list entry, not a grant key, described by the chart's own
// schema, and of the type the kind answers.
func validateQuestionKey(key, where string, kind Kind, sch *schema) []string {
	var problems []string
	path, err := seedbox.ParsePath(key)
	if err != nil {
		return []string{fmt.Sprintf("%s.key: %s: %v", where, key, err)}
	}
	for _, st := range path {
		if st.IsIndex {
			problems = append(problems, fmt.Sprintf("%s.key: %s names a list entry; a question answers one key", where, key))
			break
		}
	}
	// The grant is root's: a manifest must never be able to ask for the
	// domain, a host path, a uid, the namespace or the app list. BoxOnly and
	// BoxSets are refused with the same rigidity: neither file names a claim.
	if rule, ok := seedbox.GrantRule(path.Keys()); ok {
		return append(problems, fmt.Sprintf("%s.key: %s is set by %s (%s); the grant is box's question, never the catalog's",
			where, key, setBy(rule), rule.Why))
	}
	node, err := sch.at(path.Keys())
	if err != nil {
		return append(problems, fmt.Sprintf("%s.key: %s: %v", where, key, err))
	}
	if node == nil {
		return append(problems, fmt.Sprintf("%s.key: %s is not a key in values.schema.json", where, key))
	}
	if want := schemaType(kind); want != "" {
		if got, _ := node["type"].(string); got != "" && got != want {
			problems = append(problems, fmt.Sprintf("%s.key: %s is a %s in values.schema.json, so a %s question cannot answer it", where, key, got, kind))
		}
	}
	return problems
}

// setBy names who owns a grant key, so a rejected question says whose it is.
func setBy(rule seedbox.Rule) string {
	if rule.BoxSets {
		return "box"
	}
	return "the grant"
}

func schemaType(k Kind) string {
	switch k {
	case KindString, KindSecret, KindTransient:
		return "string"
	case KindBool:
		return "boolean"
	}
	return ""
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "text"
	case bool:
		return "true/false"
	case int, int64, uint64, float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprintf("%T", v)
}

// chartApp is what values.yaml says about one app.
type chartApp struct {
	Instances bool
}

// valuesApps reads the app keys and whether each declares an instances list.
func valuesApps(src []byte) (map[string]chartApp, error) {
	var doc struct {
		Apps map[string]map[string]any `yaml:"apps"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]chartApp, len(doc.Apps))
	for name, entry := range doc.Apps {
		_, instances := entry["instances"]
		out[name] = chartApp{Instances: instances}
	}
	return out, nil
}

// appOrder is the manifest's `apps:` key order. A YAML mapping decoded into a
// Go map loses it, and init shows the apps in the order a contributor wrote
// them, so it is read once from the node tree.
func appOrder(raw []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "apps" {
			continue
		}
		apps := root.Content[i+1]
		if apps.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("apps: must be a mapping of app names")
		}
		out := make([]string, 0, len(apps.Content)/2)
		seen := map[string]bool{}
		for j := 0; j+1 < len(apps.Content); j += 2 {
			name := apps.Content[j].Value
			if seen[name] {
				return nil, fmt.Errorf("apps.%s: is declared twice", name)
			}
			seen[name] = true
			out = append(out, name)
		}
		return out, nil
	}
	return nil, nil
}

// readCapped reads a chart file, refusing one over the cap. A file that is too
// big is a refusal, not a truncated read: half a manifest is worse than none.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifest {
		return nil, fmt.Errorf("%s is larger than %d bytes; a catalog manifest is a few KB", path, MaxManifest)
	}
	return data, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
