package main

// migrate_apply.go is the write half: `box migrate apply [<snapshot>]`.
//
// It is the thin layer the design asks for. Everything that can be tested
// without a cluster is in internal/migrate/apply; this file opens the snapshot,
// reads the plan out of it, reads the seedbox, checks that the release exists,
// builds the environment apply talks to (the release's ownership record, the
// grant editor, the Plex runner, the one question) and then hands over to the
// same apply path `box apply` uses.
//
// The order matters as much here as in the package: nothing is written until
// the plan, the two files and the deployed release have all been read.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
)

// migrateApplyFlags are `box migrate apply`'s flags: apply's own (the grant,
// the values file, the kubeconfig, --yes and the chart reference) plus the one
// this command adds.
type migrateApplyFlags struct {
	*applyFlags
	plain *bool
}

func addMigrateApplyFlags(fs *flag.FlagSet) *migrateApplyFlags {
	return &migrateApplyFlags{
		applyFlags: addApplyFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
	}
}

func runMigrateApply(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate apply", flag.ExitOnError)
	o := addMigrateApplyFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// Go's flag package stops at the first argument that is not a flag, and the
	// snapshot is one: `box migrate apply <file> --yes` would otherwise read
	// --yes as a second argument. So what follows the file is parsed as flags,
	// and only the file itself may be left over.
	file := ""
	if rest := fs.Args(); len(rest) > 0 {
		file = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "box: %v\n", err)
			os.Exit(2)
		}
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "box: migrate apply takes one snapshot: box migrate apply [<snapshot>]\n")
		os.Exit(2)
	}
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}

	out := newOutput(terminalSet(*o.plain), *o.yes, startupTheme(*o.valuesPath))
	defer out.Close()
	if err := hostFiles(out, bootstrap.RecordPath, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}
	if err := migrateApply(ctx, file, o, out); err != nil {
		if errors.Is(err, apply.ErrCancelled) || errors.Is(err, release.ErrCancelled) {
			out.Line("oh heck, nvm")
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
}

// openSnapshot finds the snapshot apply runs and extracts it into a private
// directory. With no file named it is the newest one in bundle.Dir that is not
// a survey, and the run says which file it read.
//
// The mode is what apply is allowed to do: a survey holds no configs, a
// rehearsal may only be applied locked and that lock lands next, and a final
// snapshot is the one a real run applies.
func openSnapshot(file string, out say.Reporter) (*bundle.Opened, func(), error) {
	if strings.TrimSpace(file) == "" {
		newest, err := bundle.Newest(bundle.Dir)
		if err != nil {
			return nil, func() {}, err
		}
		file = newest
	}
	dir, err := os.MkdirTemp("", "box-migrate-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("a private directory for the snapshot: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }
	opened, err := bundle.Open(file, filepath.Join(dir, "snapshot"))
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	switch opened.Mode {
	case archive.ModeFinal:
	case archive.ModeSurvey:
		cleanup()
		return nil, func() {}, fmt.Errorf("%s holds no configs: it is a survey of the source, and `box migrate cutover` writes the snapshot apply runs", file)
	case archive.ModeRehearsal:
		cleanup()
		return nil, func() {}, fmt.Errorf("%s is a rehearsal snapshot, and the rehearsal lock lands next: a rehearsal may only be applied with no internet egress and every torrent started stopped, so this box will not apply it until it can do that", file)
	default:
		cleanup()
		return nil, func() {}, fmt.Errorf("%s is a %q snapshot, and apply runs a final one: `box migrate cutover` writes it", file, opened.Mode)
	}
	out.Line(fmt.Sprintf("snapshot: %s (%s)", file, opened.Mode))
	return opened, cleanup, nil
}

// migrateApply is the command without the exit, so a test can drive its
// refusals. It returns the error the run stopped on; a cancelled run returns
// apply.ErrCancelled after saying so in box's own voice.
//
// file is the snapshot to run, or empty for the newest one in bundle.Dir. The
// snapshot is extracted into a private directory for the length of the run and
// removed when it ends: the configs apply rewrites are its copies, not the
// source's.
func migrateApply(ctx context.Context, file string, o *migrateApplyFlags, out say.Reporter) error {
	opened, cleanup, err := openSnapshot(file, out)
	if err != nil {
		return err
	}
	defer cleanup()
	// Step 0: the plan and the seedbox, before any cluster client exists.
	p, err := loadPlan(opened)
	if err != nil {
		return err
	}
	raw := opened.Plan
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return err
	}
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		return err
	}
	// The release is what migrate adopts into: its claims hold this run's
	// staging tree, and its wiring record is where the adopted connections go.
	client, err := release.NewHelm(kc, namespace, release.ReleaseName)
	if err != nil {
		return err
	}
	_, deployed, err := client.Deployed(ctx)
	if err != nil {
		return err
	}
	if !deployed {
		return fmt.Errorf("migrate adopts into an installed seedbox: %s is not installed in %s. Run `box init` first, with the plan's apps left off", release.ReleaseName, namespace)
	}

	chart, err := loadMigrateChart(ctx, o, out)
	if err != nil {
		return err
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return err
	}
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return err
	}
	env, err := migrateEnv(p, opened, o, sb, chart, cat, kc, namespace, values, out)
	if err != nil {
		return err
	}

	res, err := apply.Run(ctx, p, raw, env)
	for _, note := range res.Notes {
		out.Line(note)
	}
	if err != nil {
		return err
	}
	if res.Done {
		out.Line(fmt.Sprintf("%s in %s: the plan is already applied", release.ReleaseName, namespace))
	} else {
		out.Success(fmt.Sprintf("moved %d instances into %s", len(res.Moved), env.ConfigRoot))
	}

	// Step 11: the same apply `box apply` runs, with the answer the operator
	// gave above. The files are re-read there, which is what makes the release
	// render what this run just moved into place.
	return migrateHandOver(ctx, o, chart, kc, out)
}

// loadPlan parses the plan the snapshot carries. A plan box cannot read is a
// refusal that names the snapshot, because that file is what an operator copies
// between hosts and what a bug report has to name.
func loadPlan(opened *bundle.Opened) (*plan.Plan, error) {
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		var perr *plan.Error
		if errors.As(err, &perr) {
			perr.Path = opened.Path + ":" + archive.PlanName
		}
		return nil, err
	}
	return p, nil
}

// loadMigrateChart loads the one chart release this command reads and installs.
// It is loaded once and handed to the apply, so the release migrate checks
// against is the release it installs.
func loadMigrateChart(ctx context.Context, o *migrateApplyFlags, out say.Reporter) (release.Chart, error) {
	return o.chart.load(ctx, chartSource(out))
}

// migrateEnv builds everything apply touches outside the plan directory.
func migrateEnv(p *plan.Plan, opened *bundle.Opened, o *migrateApplyFlags, sb *seedbox.Seedbox, chart release.Chart, cat *catalog.Catalog, kc, namespace string, values map[string]any, out say.Reporter) (apply.Env, error) {
	var env apply.Env
	vols, err := sb.Storage()
	if err != nil {
		return env, err
	}
	for _, v := range vols {
		switch v.Kind {
		case "config":
			env.ConfigRoot = v.Path
		case "data":
			env.DataRoot = v.Path
		}
	}
	if env.ConfigRoot == "" || env.DataRoot == "" {
		return env, errors.New("the grant names no storage paths: box init writes both")
	}
	// Every config apply rewrites comes out of the snapshot, one directory per
	// planned app. An app the plan names and the snapshot does not carry is a
	// refusal preflight makes, naming the app.
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	env.Catalog = cat.Names()
	// The globals are numbers in the effective document, which is where a
	// renderer reads them too.
	if env.PUID, err = number(values, "global", "puid"); err != nil {
		return env, err
	}
	if env.PGID, err = number(values, "global", "pgid"); err != nil {
		return env, err
	}
	// Every instance a rewriter may meet an address for: this plan's, and
	// every one the release already switches on.
	names := append([]string(nil), sortedKeys(p.Apps)...)
	for _, name := range apply.Instances(values) {
		if !containsString(names, name) {
			names = append(names, name)
		}
	}
	if env.Targets, err = apply.Targets(values, env.Catalog, names); err != nil {
		return env, err
	}
	env.Enabled = func(instance string) bool { return apply.EnabledIn(values, instance) }

	cfg, err := kube.Config(kc)
	if err != nil {
		return env, fmt.Errorf("kubeconfig %s: %w", kc, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return env, fmt.Errorf("cluster client: %w", err)
	}
	env.Owned = &wiringRecord{cs: cs, namespace: namespace, name: wiringConfigMap}
	env.Secret = &secretStore{cs: cs, namespace: namespace, name: releaseSecretName(values)}
	env.Values = &grantEditor{sb: sb, catalog: env.Catalog, values: values}

	// The one app whose database only its own image may write: the runner is
	// built only when the plan names it, because it needs the image the values
	// select and a claim to mount.
	if planNeedsRunner(p, env.Catalog) {
		image, err := apply.Image(chart.Variants, values, "plex")
		if err != nil {
			return env, err
		}
		claim := ""
		for _, v := range vols {
			if v.Kind == "config" {
				claim = v.Claim
			}
		}
		runner := apply.NewRunner(apply.RunnerConfig{
			Client:     cs,
			Namespace:  namespace,
			Claim:      claim,
			ConfigRoot: env.ConfigRoot,
			PlanID:     apply.PlanID(opened.Plan),
			Image:      image,
			PUID:       env.PUID,
			PGID:       env.PGID,
		})
		env.Run = runner.Run
	}
	env.Confirm = migrateConfirm(out, *o.yes, namespace)
	return env, nil
}

// migrateConfirm shows the summary and asks the one question. It is the same
// confirmer `box apply` asks with, so a terminal gets the live form and a
// --plain or piped run gets the line prompt; --yes answers without asking, and
// no terminal without --yes refuses, as apply does.
func migrateConfirm(out say.Reporter, yes bool, namespace string) func(apply.Summary) (bool, error) {
	ask := confirmer(out, yes)
	return func(s apply.Summary) (bool, error) {
		out.List("this migration", s.Lines())
		return ask.Ask(fmt.Sprintf("move these into %s?", namespace))
	}
}

// migrateHandOver is step 11: the apply. It is `box apply`'s own path, with the
// chart already loaded and the question already answered.
func migrateHandOver(ctx context.Context, o *migrateApplyFlags, chart release.Chart, kc string, out say.Reporter) error {
	app := &release.Applier{
		Charts:  chartSource(out),
		Confirm: answered{},
		Say:     out,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return release.NewHelm(kc, namespace, release.ReleaseName)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}
	opts := o.options()
	opts.Chart = &chart
	opts.BoxValues = boxValues(kc)
	// The same steps a bare apply runs: an app this migration switched on may
	// be served at a host of its own, which is root's listener and Certificate
	// to write before the release moves.
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: setup.NetResolver{}}
	holds := newBoxHolds(out)
	opts.Holds = holds.values
	res, err := applyThenSettle(ctx, app.Apply, opts, holds)
	if err != nil {
		return err
	}
	if res.Host != "" {
		out.Banner("seedbox ready", res.Host)
	}
	return nil
}

// releaseSecretName is the Secret the chart renders: `<release>-keys`, or the
// one `secrets.existingSecret` names (charts/s5n/templates/secret.yaml:7-9).
func releaseSecretName(values map[string]any) string {
	secrets, _ := values["secrets"].(map[string]any)
	if name, _ := secrets["existingSecret"].(string); strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return release.ReleaseName + "-keys"
}

// secretStore is the release Secret, one data key at a time. The chart's keys
// are plain base64 of the value, which is what corev1.Secret's Data already is,
// so nothing here encodes or prints anything.
type secretStore struct {
	cs        kubernetes.Interface
	namespace string
	name      string
}

func (s *secretStore) Get(ctx context.Context, key string) (string, error) {
	sec, err := s.secret(ctx)
	if err != nil {
		return "", err
	}
	return string(sec.Data[key]), nil
}

func (s *secretStore) Set(ctx context.Context, key, value string) error {
	sec, err := s.secret(ctx)
	if err != nil {
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[key] = []byte(value)
	// The object was just read, so the update carries its resourceVersion: a
	// concurrent writer is refused rather than overwritten.
	_, err = s.cs.CoreV1().Secrets(s.namespace).Update(ctx, sec, metav1.UpdateOptions{})
	return err
}

func (s *secretStore) secret(ctx context.Context) (*corev1.Secret, error) {
	sec, err := s.cs.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read secret %s in %s: %w", s.name, s.namespace, err)
	}
	return sec, nil
}

// wiringRecord is the `owned.json` key of `<release>-wiring`.
type wiringRecord struct {
	cs        kubernetes.Interface
	namespace string
	name      string
}

func (w *wiringRecord) Read(ctx context.Context) ([]byte, error) {
	cm, err := w.cs.CoreV1().ConfigMaps(w.namespace).Get(ctx, w.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return []byte(cm.Data["owned.json"]), nil
}

func (w *wiringRecord) Write(ctx context.Context, data []byte) error {
	cm, err := w.cs.CoreV1().ConfigMaps(w.namespace).Get(ctx, w.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["owned.json"] = string(data)
	_, err = w.cs.CoreV1().ConfigMaps(w.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// grantEditor switches an instance on in the seedbox's files.
//
// Both keys are grant keys (internal/seedbox/grant.go: apps.*.enabled and
// apps.*.instances), so this edits the grant and not the user's values file:
// a grant key in values is refused by the merge, and the grant is what decides
// which apps exist.
type grantEditor struct {
	sb      *seedbox.Seedbox
	catalog []string
	values  map[string]any
}

func (g *grantEditor) Enable(instance string) error {
	app := apply.CatalogApp(instance, g.catalog)
	if app == "" {
		return fmt.Errorf("%s: no app in the chart's catalog is named that", instance)
	}
	suffix := apply.Suffix(instance, app)
	defaults, takesInstances := instancesOf(g.values, app)
	if suffix != "" && !takesInstances {
		return fmt.Errorf("%s: %s takes no second instance, so there is nothing to switch on for %q", instance, app, suffix)
	}
	edits := []seedbox.Edit{{Path: "apps." + app + ".enabled", Value: true}}
	if takesInstances {
		own, hasOwn := grantInstances(g.sb, app)
		switch {
		case !hasOwn:
			// The grant names no list, so the release renders the chart's
			// defaults. The list is the grant's as a unit, so the one edit
			// writes it whole, with this instance on.
			edits = append(edits, seedbox.Edit{
				Path:  "apps." + app + ".instances",
				Value: switchOn(defaults, suffix, app),
			})
		default:
			if index, _ := findInstance(own, suffix); index >= 0 {
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d].enabled", app, index),
					Value: true,
				})
			} else {
				// An instance the grant's list does not carry: box init adds
				// one by appending, and the entry has the three keys the
				// schema requires.
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d]", app, len(own)),
					Value: map[string]any{"suffix": suffix, "category": categoryFor(app, suffix), "enabled": true},
				})
			}
		}
	}
	// One atomic write for both keys: a grant naming an app that is on and an
	// instance list without its entry is a release with one instance fewer than
	// the operator asked for.
	editor := g.sb.Grant.Editor()
	if err := editor.SetAll(edits); err != nil {
		return err
	}
	return editor.Save()
}

// grantInstances is the list the grant itself names for one app, and whether it
// names one at all: an app the operator never turned on has no instances key,
// and the chart's default list is what the release then renders.
func grantInstances(sb *seedbox.Seedbox, app string) ([]any, bool) {
	apps, _ := sb.Grant.Map()["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	list, ok := entry["instances"].([]any)
	return list, ok
}

// instancesOf is the app's effective instances list, and whether it has one at
// all: only an app that takes instances has the key.
func instancesOf(values map[string]any, app string) ([]any, bool) {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	list, ok := entry["instances"].([]any)
	return list, ok
}

// findInstance is the index of the entry with one suffix, or -1.
func findInstance(list []any, suffix string) (int, string) {
	for i, raw := range list {
		entry, _ := raw.(map[string]any)
		got, _ := entry["suffix"].(string)
		if got != suffix {
			continue
		}
		category, _ := entry["category"].(string)
		return i, category
	}
	return -1, ""
}

// switchOn is a list of instances with one suffix switched on, adding the entry
// when the list does not carry it.
func switchOn(list []any, suffix, app string) []any {
	out := make([]any, 0, len(list)+1)
	found := false
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		got, _ := entry["suffix"].(string)
		if got == suffix {
			found = true
			entry = cloneEntry(entry)
			entry["enabled"] = true
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, map[string]any{"suffix": suffix, "category": categoryFor(app, suffix), "enabled": true})
	}
	return out
}

// cloneEntry is one instance entry as a fresh map, so the edit does not mutate
// the document it was read from.
func cloneEntry(entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		out[k] = v
	}
	return out
}

// categoryFor names the category of an instance the chart's list does not
// carry. It is one qBittorrent category and one directory under each /data
// tree, so the suffix is the honest name: the instance is called sonarr-1080
// and its directory 1080. The primary instance of a list with no primary entry
// takes the app's name, because a category cannot be empty.
func categoryFor(app, suffix string) string {
	if suffix == "" {
		return app
	}
	return suffix
}

// planNeedsRunner reports whether a plan names the one app whose rewriter needs
// an image to write its format.
func planNeedsRunner(p *plan.Plan, catalogApps []string) bool {
	for name := range p.Apps {
		if apply.NeedsRunner(apply.CatalogApp(name, catalogApps)) {
			return true
		}
	}
	return false
}

// wiringConfigMap is `<release>-wiring`: the ConfigMap the chart renders and
// the wiring Job upserts its record into (charts/s5n/templates/wiring.yaml).
const wiringConfigMap = release.ReleaseName + "-wiring"

// number reads one integer out of the effective values document.
func number(values map[string]any, keys ...string) (int, error) {
	var cur any = values
	for _, key := range keys {
		m, _ := cur.(map[string]any)
		next, ok := m[key]
		if !ok {
			return 0, fmt.Errorf("%s: the values name no such key", strings.Join(keys, "."))
		}
		cur = next
	}
	switch n := cur.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		v, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("%s: %q is not a number", strings.Join(keys, "."), n)
		}
		return v, nil
	}
	return 0, fmt.Errorf("%s: %v is not a number", strings.Join(keys, "."), cur)
}

// sortedKeys is a plan's instance names, sorted.
func sortedKeys(m map[string]plan.App) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// containsString reports whether list holds name.
func containsString(list []string, name string) bool {
	for _, got := range list {
		if got == name {
			return true
		}
	}
	return false
}
