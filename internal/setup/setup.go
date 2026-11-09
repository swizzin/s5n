// Package setup is `box init`: it asks the questions a seedbox has, writes the
// answers into the grant and the values file one key at a time, and hands over
// to `box apply`, whose diff and confirm are the review. There is no separate
// "here is your file" step (docs/box.md, "bootstrap and init").
//
// Two properties shape the code. Re-running init edits: every question is
// prefilled from the current merged values and only a changed answer is spliced,
// so hand edits elsewhere in either file survive byte for byte. And nothing is
// written until every question has an answer, so a cancel leaves both files
// exactly as they were.
//
// It is a pure decision over small interfaces, like internal/bootstrap: the UI
// is a Prompter, the host filesystem a PathHost, the files internal/seedbox, the
// questions internal/catalog, and the hand-over a function. Everything is tested
// from fixtures; the only implementation that touches a machine is
// LinuxPathHost, and only cmd/box builds it.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/seedbox"
)

// Decision is `box init`'s whole flow.
type Decision struct {
	Host    PathHost
	Prompt  Prompter
	Catalog *catalog.Catalog
	// Apply is the hand-over: the same `box apply` a user runs. It is a
	// function rather than the release package so init does not link Helm.
	Apply ApplyFunc
	Log   io.Writer

	// steps is how many steps this run shows, for a prompter that draws a step
	// counter. It is advice for a UI, never an input: nothing reads it back.
	steps int
}

// step tells a prompter that draws progress where the next answer belongs.
// Prompters without a Stepper (LinePrompt) hear nothing.
func (d *Decision) step(n int, question string) {
	if st, ok := d.Prompt.(Stepper); ok {
		st.Step(n, d.steps, question)
	}
}

// Options are one `box init`. Every grant question has a field here: that is
// what makes an unattended run possible, and the flags are the same names.
type Options struct {
	// Chart is the release box is installing: the defaults every question is
	// prefilled from and the schema every answer is checked against. init does
	// not read charts/ from its own tree, so the caller loads the published
	// release and hands it over.
	Chart      seedbox.Chart
	GrantPath  string
	ValuesPath string

	// Namespace is the release namespace. It is not one of the questions init
	// asks: a seedbox's namespace is its identity, so a re-run keeps whatever
	// the grant already says. The flag exists for the first run.
	Namespace  string
	Domain     string
	DataPath   string
	ConfigPath string
	// Apps is the grant's app list, by catalog name ("sonarr", not
	// "sonarr-4k": instances are root's, through `box install`).
	Apps []string
	// PUID and PGID are the ids every file on the data path is owned by.
	// PUIDSet and PGIDSet say the operator named them, so they win over a
	// re-run's grant; without them the grant's own values do.
	PUID, PGID int
	PUIDSet    bool
	PGIDSet    bool
	// SetTransient answers a transient question by its values path, which is
	// the unattended form of a question that cannot live in a file.
	SetTransient map[string]string

	// Unattended asks nothing: flags and file keys only, and a missing required
	// answer is an error naming the flag or key.
	Unattended bool
	// PathsInNode says the storage paths live inside a test node (kind), not on
	// this host, so init neither verifies nor creates them. `box apply` has the
	// same flag for the same reason.
	PathsInNode bool
}

// ApplyOptions is what init hands the applier: the same arguments `box apply`
// takes, plus the hook for answers that outlive no file.
type ApplyOptions struct {
	GrantPath   string
	ValuesPath  string
	PathsInNode bool
	// AfterConfirm asks the transient questions, after apply's own confirm.
	AfterConfirm func(ctx context.Context) (map[string]any, error)
	// TransientKeys are the values paths AfterConfirm answers, so the apply
	// keeps them out of its diff.
	TransientKeys []string
}

// ApplyFunc is one `box apply`.
type ApplyFunc func(ctx context.Context, opts ApplyOptions) error

// Result is what init did, for the caller to report and a test to assert.
type Result struct {
	ReRun bool
	// GrantChanged and ValuesChanged are the keys spliced, in the order they
	// were written. A re-run with identical answers leaves both empty.
	GrantChanged  []string
	ValuesChanged []string
	// Created are the directories init made, owned by the box's uid:gid.
	Created []string
	// Applied says the hand-over ran. It does not say the release changed:
	// apply decides that, and a re-run is a no-op there too.
	Applied bool
}

func (d *Decision) logf(format string, args ...any) {
	if d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, format, args...)
}

// stop prints the voice for a cancelled question and passes the error on.
func (d *Decision) stop(err error) error {
	if errors.Is(err, ErrCancelled) {
		d.logf("oh heck, nvm\n")
	}
	return err
}

// Run asks the questions, edits the two files and hands over to apply.
func (d *Decision) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result
	if err := d.check(); err != nil {
		return res, err
	}
	if len(opts.Chart.Values) == 0 || len(opts.Chart.Schema) == 0 {
		return res, errors.New("no chart: box init asks its questions from the chart release it installs")
	}
	sb, err := loadSeedbox(opts.GrantPath, opts.ValuesPath)
	if err != nil {
		return res, err
	}
	res.ReRun = sb.Grant.Exists

	// The baseline every question is prefilled from: what the two files say
	// today (merged, checked against the chart) or, on a fresh host, the chart's
	// own defaults with every app off.
	base, err := d.baseline(sb, opts.Chart)
	if err != nil {
		return res, err
	}
	grantNow := sb.Grant.Map()

	namespace, err := d.namespace(opts, grantNow, res.ReRun)
	if err != nil {
		return res, err
	}
	puid, pgid, err := d.ids(opts, grantNow, res.ReRun)
	if err != nil {
		return res, err
	}

	// The grant's questions, in the order docs/box.md lists them.
	//
	// The flow has three steps of its own, and a fourth when an app has
	// questions of its own (the catalog's qBittorrent does). A prompter that
	// draws a counter is told, and a prompter without one never notices.
	d.steps = 3
	for _, name := range d.appsOn(grantNow, res.ReRun) {
		if app, ok := d.Catalog.App(name); ok && len(app.Stored()) > 0 {
			d.steps = 4
			break
		}
	}
	d.step(1, "what domain will this box answer on?")
	domain, err := d.askText(textQuestion{
		key: "gateway.host", flag: "--domain",
		prompt: "domain", help: "the name this box is reached at, e.g. box.example.com",
		value: opts.Domain, prefill: prefill(base, grantNow, "gateway.host"),
		unattended: opts.Unattended, validate: validDomain,
	})
	if err != nil {
		return res, d.stop(err)
	}
	d.step(2, "where should your data and settings live?")
	dataPath, err := d.askText(textQuestion{
		key: "storage.data.path", flag: "--data-path",
		prompt: "data path", help: "where downloads and media live on this host",
		value: opts.DataPath, prefill: prefill(base, grantNow, "storage.data.path"),
		unattended: opts.Unattended, validate: validHostPath,
	})
	if err != nil {
		return res, d.stop(err)
	}
	configPath, err := d.askText(textQuestion{
		key: "storage.config.path", flag: "--config-path",
		prompt: "config path", help: "where the apps keep their settings on this host",
		value: opts.ConfigPath, prefill: prefill(base, grantNow, "storage.config.path"),
		unattended: opts.Unattended, validate: validHostPath,
	})
	if err != nil {
		return res, d.stop(err)
	}
	if err := checkPathPair(dataPath, configPath); err != nil {
		return res, err
	}

	// What is at those paths, and whether a missing one may be created. The
	// report is read-only; the directories are made after the last question, so
	// a cancel here still leaves the host alone.
	paths := []pathTarget{
		{kind: "data", flag: "--data-path", path: dataPath},
		{kind: "config", flag: "--config-path", path: configPath},
	}
	if !opts.PathsInNode {
		d.step(2, "where should your data and settings live?")
		if err := d.reportPaths(paths, puid, pgid, opts.Unattended); err != nil {
			return res, d.stop(err)
		}
	}

	d.step(3, "which apps should run here?")
	apps, err := d.askApps(opts, grantNow, res.ReRun)
	if err != nil {
		return res, d.stop(err)
	}
	// The app list decides whether there is a fourth step, so correct the
	// counter before the questions that belong to it.
	d.steps = 3
	for _, name := range apps {
		if app, ok := d.Catalog.App(name); ok && len(app.Stored()) > 0 {
			d.steps = 4
			break
		}
	}

	// Each selected app's own questions, from the catalog. A transient one is
	// not asked here: it goes to the apply, after its confirm.
	var stored []answer
	var transient []catalog.Question
	for _, name := range apps {
		app, ok := d.Catalog.App(name)
		if !ok {
			continue
		}
		transient = append(transient, app.Transient()...)
		if opts.Unattended {
			continue
		}
		if len(app.Stored()) > 0 {
			d.step(4, "how should "+name+" be set up?")
		}
		for _, q := range app.Stored() {
			value, err := d.askQuestion(q, base)
			if err != nil {
				return res, d.stop(err)
			}
			stored = append(stored, answer{key: q.Key, value: value})
		}
	}

	transientKeys := make([]string, 0, len(transient))
	for _, q := range transient {
		transientKeys = append(transientKeys, q.Key)
	}
	if err := checkTransientFlags(opts.SetTransient, transientKeys); err != nil {
		return res, err
	}
	if opts.Unattended {
		for _, q := range transient {
			if q.Optional {
				continue
			}
			if strings.TrimSpace(opts.SetTransient[q.Key]) == "" {
				return res, fmt.Errorf("%s is needed for this install: pass --set-transient %s=<value>", q.Prompt, q.Key)
			}
		}
	}

	grantAnswers := []answer{
		{key: "namespace", value: namespace},
		{key: "global.puid", value: puid},
		{key: "global.pgid", value: pgid},
		{key: "storage.data.path", value: dataPath},
		{key: "storage.config.path", value: configPath},
		{key: "gateway.host", value: domain},
	}
	for _, name := range d.Catalog.Names() {
		grantAnswers = append(grantAnswers, answer{key: "apps." + name + ".enabled", value: contains(apps, name)})
	}

	res.GrantChanged, res.ValuesChanged, err = d.writeFiles(opts, sb, grantNow, base, grantAnswers, stored)
	if err != nil {
		return res, err
	}
	d.reportWrites(opts, res)

	if !opts.PathsInNode {
		var dirs []string
		for _, t := range paths {
			if t.create {
				dirs = append(dirs, t.path)
			}
		}
		dirs = append(dirs, settingsDirs(configPath, apps, base)...)
		res.Created, err = d.makeDirs(dirs, puid, pgid)
		if err != nil {
			return res, err
		}
		if len(res.Created) > 0 {
			d.logf("created %s\n", strings.Join(res.Created, ", "))
		}
	}

	if d.Apply == nil {
		return res, errors.New("no apply: init edits the two files and hands over to `box apply`")
	}
	req := ApplyOptions{
		GrantPath:     opts.GrantPath,
		ValuesPath:    opts.ValuesPath,
		PathsInNode:   opts.PathsInNode,
		TransientKeys: transientKeys,
	}
	if len(transient) > 0 {
		req.AfterConfirm = d.transientAsker(transient, opts)
	}
	if err := d.Apply(ctx, req); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// check refuses a Decision that cannot run, before anything reads a file.
func (d *Decision) check() error {
	switch {
	case d.Host == nil:
		return errors.New("no host: box init needs the path probes")
	case d.Prompt == nil:
		return errors.New("no prompter: box init needs a way to ask")
	case d.Catalog == nil:
		return errors.New("no catalog: box init's questions come from the chart's catalog.yaml")
	}
	return nil
}

// baseline is the document every question is prefilled from.
func (d *Decision) baseline(sb *seedbox.Seedbox, chart seedbox.Chart) (map[string]any, error) {
	if !sb.Grant.Exists {
		return seedbox.Base(chart)
	}
	values, err := sb.Effective(chart)
	if err != nil {
		// init edits what is there; it does not replace it. A file that no
		// longer checks out has to be fixed first, or the prefill would be a
		// guess and the splice would hide the mistake.
		return nil, fmt.Errorf("this seedbox's files do not check out, and init edits them rather than replacing them: %w", err)
	}
	return values, nil
}

// textQuestion is one grant answer: a flag when unattended, a prompt otherwise,
// with the current file value as the default in both.
type textQuestion struct {
	key        string
	flag       string
	prompt     string
	help       string
	value      string
	prefill    any
	unattended bool
	validate   func(string) error
}

func (d *Decision) askText(q textQuestion) (string, error) {
	given := strings.TrimSpace(q.value)
	if q.unattended {
		if given == "" {
			return "", fmt.Errorf("%s is needed: pass %s", q.key, q.flag)
		}
		if q.validate != nil {
			if err := q.validate(given); err != nil {
				return "", fmt.Errorf("%s %q: %w", q.flag, given, err)
			}
		}
		return given, nil
	}
	def := strings.TrimSpace(text(q.prefill))
	if given != "" {
		// A flag given in guided mode is the starting answer, not a decision:
		// the prompt still shows it and the operator can change it.
		def = given
	}
	return d.Prompt.Input(Input{
		Key: q.key, Prompt: q.prompt, Help: q.help, Default: def, Validate: q.validate,
	})
}

// namespace resolves the one grant key that is not a question. A seedbox's
// namespace is its identity -- claim names and storage are derived from it -- so
// a re-run never changes it, whatever the flag says.
func (d *Decision) namespace(opts Options, grantNow map[string]any, reRun bool) (string, error) {
	if reRun {
		if ns, ok := lookupPath(grantNow, "namespace"); ok {
			if s, ok := ns.(string); ok && strings.TrimSpace(s) != "" {
				return s, nil
			}
		}
	}
	ns := strings.TrimSpace(opts.Namespace)
	if ns == "" {
		return "", errors.New("namespace is needed: pass --namespace")
	}
	if err := validNamespace(ns); err != nil {
		return "", fmt.Errorf("--namespace %q: %w", ns, err)
	}
	return ns, nil
}

// ids resolves puid and pgid: the flag when it was given, the grant's value on a
// re-run, and otherwise what cmd/box derived from the invoking user.
func (d *Decision) ids(opts Options, grantNow map[string]any, reRun bool) (int, int, error) {
	puid, pgid := opts.PUID, opts.PGID
	if reRun {
		if !opts.PUIDSet {
			if n, ok := intAt(grantNow, "global.puid"); ok {
				puid = n
			}
		}
		if !opts.PGIDSet {
			if n, ok := intAt(grantNow, "global.pgid"); ok {
				pgid = n
			}
		}
	}
	if puid <= 0 {
		return 0, 0, errors.New("--puid is needed: the uid every file on the data path is owned by")
	}
	if pgid <= 0 {
		return 0, 0, errors.New("--pgid is needed: the gid every file on the data path is owned by")
	}
	return puid, pgid, nil
}

// appsOn is the app list init offers preselected: the grant's on a re-run, the
// catalog's defaults on a fresh box. `--apps` is not here: in guided mode it is
// a starting answer the multi-select still shows, so the only use for this list
// is the step count before that question is asked.
func (d *Decision) appsOn(grantNow map[string]any, reRun bool) []string {
	names := d.Catalog.Names()
	var current []string
	if reRun {
		for _, name := range names {
			if on, ok := lookupPath(grantNow, "apps."+name+".enabled"); ok {
				if b, _ := on.(bool); b {
					current = append(current, name)
				}
			}
		}
		return current
	}
	return d.Catalog.Defaults()
}

// askApps resolves the app list: the flag unattended, a multi-select otherwise,
// preselected from the grant on a re-run and from the catalog on a fresh box.
func (d *Decision) askApps(opts Options, grantNow map[string]any, reRun bool) ([]string, error) {
	names := d.Catalog.Names()
	options := make([]Option, 0, len(names))
	for _, name := range names {
		app, _ := d.Catalog.App(name)
		options = append(options, Option{Value: name, Label: name, Help: app.Description})
	}

	current := d.appsOn(grantNow, reRun)

	given := splitApps(opts.Apps)
	if opts.Unattended {
		if len(given) == 0 {
			if reRun {
				return current, nil
			}
			return nil, fmt.Errorf("--apps is needed: comma separated app names from %s", strings.Join(names, ", "))
		}
		return checkApps(given, names)
	}

	selected := current
	if len(given) > 0 {
		chosen, err := checkApps(given, names)
		if err != nil {
			return nil, err
		}
		selected = chosen
	}
	return d.Prompt.MultiSelect(MultiSelect{
		Key:      "apps",
		Prompt:   "which apps should this box have",
		Options:  options,
		Selected: selected,
		Min:      1,
	})
}

// splitApps flattens --apps: one flag value that may carry commas, or several
// if it is ever made repeatable. Empty pieces disappear, so `--apps ""` is the
// same as not passing it.
func splitApps(given []string) []string {
	var out []string
	for _, raw := range given {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// checkApps validates an app list against the catalog and keeps the catalog's
// order, so the grant and the report are stable.
func checkApps(given []string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, part := range given {
		if !contains(names, part) {
			return nil, fmt.Errorf("--apps %s: the catalog has no such app (it has %s)", part, strings.Join(names, ", "))
		}
		want[part] = true
	}
	var out []string
	for _, name := range names {
		if want[name] {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--apps names no app; choose from %s", strings.Join(names, ", "))
	}
	return out, nil
}

// askQuestion asks one stored catalog question, prefilled from the current
// merged values so a re-run shows the answer that is in force.
func (d *Decision) askQuestion(q catalog.Question, base map[string]any) (any, error) {
	current, hasCurrent := lookupPath(base, q.Key)
	if q.Kind == catalog.KindBool {
		def := false
		if b, ok := current.(bool); ok && hasCurrent {
			def = b
		} else if b, ok := q.Default.(bool); ok {
			def = b
		}
		return d.Prompt.Confirm(Confirm{Prompt: q.Prompt, Default: def})
	}

	def := ""
	if hasCurrent {
		def = text(current)
	}
	if def == "" {
		if s, ok := q.Default.(string); ok {
			def = s
		}
	}
	if q.Kind == catalog.KindSecret {
		// A stored secret is never offered back as a default: pressing enter
		// keeps it (writeFiles skips an empty answer over a current value).
		return d.Prompt.Secret(Secret{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Optional: true})
	}
	var validate func(string) error
	if !q.Optional {
		validate = nonEmpty
	}
	return d.Prompt.Input(Input{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Default: def, Validate: validate})
}

// transientAsker builds the hook apply calls after its confirm. Unattended, it
// is the --set-transient answers; guided, it asks, and an empty answer is not an
// answer.
func (d *Decision) transientAsker(questions []catalog.Question, opts Options) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out := make(map[string]any, len(questions))
		for _, q := range questions {
			if v := strings.TrimSpace(opts.SetTransient[q.Key]); v != "" {
				out[q.Key] = v
				continue
			}
			if opts.Unattended {
				continue
			}
			v, err := d.Prompt.Secret(Secret{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Optional: true})
			if err != nil {
				if errors.Is(err, ErrCancelled) {
					d.logf("oh heck, nvm\n")
				}
				return nil, err
			}
			if strings.TrimSpace(v) != "" {
				out[q.Key] = v
			}
		}
		return out, nil
	}
}

// answer is one key and the value it should hold.
type answer struct {
	key   string
	value any
}

// writeFiles splices every changed answer into its file. Both files are loaded,
// each key is compared with what is there, and only a difference is written, so
// a re-run with the same answers touches nothing and hand edits elsewhere
// survive byte for byte.
func (d *Decision) writeFiles(opts Options, sb *seedbox.Seedbox, grantNow, base map[string]any, grantAnswers, storedAnswers []answer) ([]string, []string, error) {
	for _, dir := range []string{filepath.Dir(opts.GrantPath), filepath.Dir(opts.ValuesPath)} {
		if err := d.Host.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	var grantChanged, valuesChanged []string
	for _, a := range grantAnswers {
		if same(lookup(grantNow, a.key), a.value) {
			continue
		}
		if err := sb.SetGrant(a.key, a.value); err != nil {
			return grantChanged, valuesChanged, err
		}
		grantChanged = append(grantChanged, a.key)
	}
	for _, a := range storedAnswers {
		current, ok := lookupPath(base, a.key)
		if same(current, a.value) {
			continue
		}
		// An empty answer over a value that is already set is "keep it": a
		// secret question cannot offer its current value back, and init never
		// clears a setting by accident.
		if s, isString := a.value.(string); isString && strings.TrimSpace(s) == "" && ok {
			continue
		}
		if err := sb.SetValue(a.key, a.value); err != nil {
			return grantChanged, valuesChanged, err
		}
		valuesChanged = append(valuesChanged, a.key)
	}
	return grantChanged, valuesChanged, nil
}

func (d *Decision) reportWrites(opts Options, res Result) {
	if len(res.GrantChanged) == 0 {
		d.logf("%s: already says this\n", opts.GrantPath)
	} else {
		d.logf("%s: edited %d %s\n", opts.GrantPath, len(res.GrantChanged), plural(len(res.GrantChanged), "key", "keys"))
	}
	if len(res.ValuesChanged) == 0 {
		d.logf("%s: nothing to add\n", opts.ValuesPath)
	} else {
		d.logf("%s: edited %d %s\n", opts.ValuesPath, len(res.ValuesChanged), plural(len(res.ValuesChanged), "key", "keys"))
	}
}

// pathTarget is one of the two host paths, with what init found and whether the
// operator let it create the directory.
type pathTarget struct {
	kind   string
	flag   string
	path   string
	report PathReport
	create bool
}

// pathCheckFooter says what init then does without asking: the directories
// under the two paths that belong to the apps. It lives here, not in a UI,
// because both prompters must say the same thing.
const pathCheckFooter = "  downloads/<category> and media/<category> are made on the first start, owned as above"

// reportPaths shows what is at both paths and asks about each missing one, in
// data-then-config order: they are two volumes, and accepting one is no answer
// about the other. The answer is remembered, not acted on: the directories are
// created after the last question, so a cancel leaves the host as it was.
func (d *Decision) reportPaths(paths []pathTarget, uid, gid int, unattended bool) error {
	check := PathCheck{Footer: pathCheckFooter}
	for i := range paths {
		rep, err := d.inspect(paths[i].kind, paths[i].path, uid, gid)
		if err != nil {
			return err
		}
		paths[i].report = rep
		check.Reports = append(check.Reports, rep)
	}
	// A terminal UI draws the block; the line prompt gets the plain text, which
	// is exactly what it printed before this seam existed.
	var err error
	if pc, ok := d.Prompt.(PathChecker); ok {
		err = pc.ShowPathCheck(check)
	} else {
		err = d.Prompt.Note(check.Text())
	}
	if err != nil {
		return err
	}
	for i := range paths {
		if paths[i].report.Exists {
			continue
		}
		if unattended {
			return fmt.Errorf("%s %s does not exist, and --yes does not create it: create the directory (or mount its disk) and run init again", paths[i].flag, paths[i].path)
		}
		ok, err := d.Prompt.Confirm(Confirm{
			Prompt: "create " + paths[i].path + " on that filesystem?",
			// The safe answer stays the focused one: init never creates a
			// directory because somebody pressed enter.
			Default:     false,
			Affirmative: "create it",
			Negative:    "stop here",
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("not creating %s: nothing was written", paths[i].path)
		}
		paths[i].create = true
	}
	return nil
}

// checkPathPair refuses a config path inside the data path (or the reverse).
// Two volumes that overlap are a nested mount neither the chart nor a hardlink
// survives, and it is always a typo.
func checkPathPair(data, config string) error {
	data = filepath.Clean(data)
	config = filepath.Clean(config)
	if data == config {
		return fmt.Errorf("--data-path and --config-path are both %s: a box keeps its downloads and its settings apart", data)
	}
	if under(data, config) {
		return fmt.Errorf("--data-path %s is inside --config-path %s: the two volumes must not overlap", data, config)
	}
	if under(config, data) {
		return fmt.Errorf("--config-path %s is inside --data-path %s: the two volumes must not overlap", config, data)
	}
	return nil
}

// loadSeedbox reads the two files, or starts from empty ones. A missing grant is
// the normal case on a host that has never run init; a grant that is there but
// does not parse is not.
func loadSeedbox(grantPath, valuesPath string) (*seedbox.Seedbox, error) {
	if grantPath == "" {
		return nil, errors.New("no grant file: pass --grant")
	}
	if valuesPath == "" {
		return nil, errors.New("no values file: pass --values")
	}
	if _, err := os.Stat(grantPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", grantPath, err)
		}
		return seedbox.New(grantPath, valuesPath), nil
	}
	return seedbox.Load(grantPath, valuesPath)
}

// checkTransientFlags refuses an answer nothing asked for: a typo in
// --set-transient that was silently ignored would be a claim token the operator
// believes they supplied.
func checkTransientFlags(given map[string]string, keys []string) error {
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	bad := make([]string, 0, len(given))
	for k := range given {
		if !known[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	if len(keys) == 0 {
		return fmt.Errorf("--set-transient %s: no app this box has asks a question that lives in no file", bad[0])
	}
	return fmt.Errorf("--set-transient %s: no transient question in this box's catalog names it (%s)",
		bad[0], strings.Join(keys, ", "))
}

// prefill reads a key from the merged values, falling back to the grant for the
// keys that are box's own record and never reach the chart (the namespace, the
// host paths).
func prefill(base, grantNow map[string]any, key string) any {
	if v, ok := lookupPath(base, key); ok {
		return v
	}
	v, _ := lookupPath(grantNow, key)
	return v
}

// lookupPath reads a dotted key from a decoded values document.
func lookupPath(m map[string]any, key string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// lookup reads a dotted key, treating a miss as nil.
func lookup(m map[string]any, key string) any {
	v, _ := lookupPath(m, key)
	return v
}

// intAt reads an integer key, which YAML decodes as int.
func intAt(m map[string]any, key string) (int, bool) {
	switch v := lookup(m, key).(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case uint64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// same compares an answer with what a file already says, treating the integer
// widths YAML and JSON produce as one number.
func same(current, want any) bool {
	if current == nil && want == nil {
		return true
	}
	if a, ok := asInt(current); ok {
		if b, ok := asInt(want); ok {
			return a == b
		}
	}
	switch c := current.(type) {
	case string:
		w, ok := want.(string)
		return ok && c == w
	case bool:
		w, ok := want.(bool)
		return ok && c == w
	}
	return false
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), x == float64(int64(x))
	}
	return 0, false
}

// text renders a prefilled value as the default the prompt shows.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nonEmpty is the validator for a required text answer.
func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("a value is needed")
	}
	return nil
}

// validDomain is deliberately about shape, not reachability: init cannot know
// whether DNS points here, and doctor is the command that asks.
func validDomain(v string) error {
	if err := nonEmpty(v); err != nil {
		return err
	}
	if strings.Contains(v, "://") || strings.ContainsAny(v, " /\\") {
		return errors.New("write the name only, e.g. box.example.com")
	}
	if strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") || strings.Contains(v, "..") {
		return errors.New("that is not a host name")
	}
	if strings.Contains(v, ":") {
		return errors.New("drop the port: the box serves HTTP itself")
	}
	return nil
}

// validHostPath insists on the shape of a path the box will turn into a volume:
// absolute, clean, and not the root of the filesystem.
func validHostPath(v string) error {
	if err := nonEmpty(v); err != nil {
		return err
	}
	if !filepath.IsAbs(v) {
		return errors.New("give an absolute path, e.g. /srv/s5n/data")
	}
	if filepath.Clean(v) != v {
		return fmt.Errorf("write it without a trailing slash or a %q, as %s", "..", filepath.Clean(v))
	}
	if v == string(filepath.Separator) {
		return errors.New("that is the root of the filesystem; give the directory this box owns")
	}
	return nil
}

// validNamespace is the shape a Kubernetes namespace name has to have, said
// without Kubernetes words.
func validNamespace(v string) error {
	for i, r := range v {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		dash := r == '-'
		if !lower && !digit && !dash {
			return errors.New("a seedbox name is lowercase letters, digits and dashes")
		}
		if dash && (i == 0 || i == len(v)-1) {
			return errors.New("a seedbox name cannot start or end with a dash")
		}
	}
	return nil
}
