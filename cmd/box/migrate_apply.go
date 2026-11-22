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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/lock"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/platform"
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
	// install are the answers a host with no box has to be given; on a host
	// that already has one they are ignored.
	install *installFlags
	plain   *bool
	// details prints every rewrite, column by column, instead of the grouped
	// summary. The whole list is in the snapshot's report.txt either way.
	details *bool
}

func addMigrateApplyFlags(fs *flag.FlagSet) *migrateApplyFlags {
	return &migrateApplyFlags{
		applyFlags: addApplyFlags(fs),
		install:    addInstallFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
		details:    fs.Bool("details", false, "print every rewrite, column by column, instead of the grouped summary"),
	}
}

func runMigrateApply(ctx context.Context, args []string) {
	// --paths-in-node is the e2e's storage switch, taken out of the arguments
	// before the FlagSet sees them: a host with no box runs init here, and
	// init's dev switch has to reach it the same way `box bootstrap`'s does.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate apply", flag.ExitOnError)
	o := addMigrateApplyFlags(fs)
	o.install.pathsInNode = pathsInNode
	// The apply this command hands over to reads the same switch: on a host
	// whose paths are inside a test node, the second render must not stat them
	// on the host that does not have them.
	o.pathsInNode = pathsInNode
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
	if err := hostFiles(out, *o.install.record, *o.grantPath); err != nil {
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
// rehearsal applies locked and never adopts data, and a final snapshot is the
// one a real run applies.
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
	case archive.ModeFinal, archive.ModeRehearsal:
	case archive.ModeSurvey:
		cleanup()
		return nil, func() {}, fmt.Errorf("%s holds no configs: it is a survey of the source, and `box migrate cutover` writes the snapshot apply runs", file)
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
//
// A rehearsal snapshot takes the same path with the lock on: the grant gets
// box's rehearsal flag before the release is rendered, the release is applied
// locked with every app still off, the lock is proven from inside the
// namespace, and only then does the adoption switch anything on. No data is
// renamed, and the flag is in the grant, so every later `box apply` and `box
// update` renders the lock too.
func migrateApply(ctx context.Context, file string, o *migrateApplyFlags, out say.Reporter) error {
	opened, cleanup, err := openSnapshot(file, out)
	if err != nil {
		return err
	}
	defer cleanup()
	// Step 0: the plan, and then the host: a host with no box gets one from
	// the plan before anything is adopted, and a host with one is adopted into
	// as it stands.
	p, err := loadPlan(opened)
	if err != nil {
		return err
	}
	rehearsal := opened.Mode == archive.ModeRehearsal
	if rehearsal {
		// The masthead and the transcript both say what this run is. The live
		// renderer's phase head is the masthead; the fact is the line a plain
		// transcript and the e2e read.
		out.Phase("rehearsal: nothing leaves this box")
		out.Fact("rehearsal", "nothing leaves this box")
		if err := rehearsalSourceHost(p); err != nil {
			return err
		}
	} else if locked, err := grantLocked(*o.grantPath); err != nil {
		return err
	} else if locked {
		return fmt.Errorf("this box is locked from a rehearsal snapshot, and a final one never applies locked: a rehearsal box is disposable, so run this snapshot on a clean host (or one with no box)")
	}
	raw := opened.Plan
	charts := &chartRelease{flags: o.chart, out: out}
	// plat is the platform this run installed, on a host that had none: its
	// plan is shown first in the migration's one confirm, because that confirm
	// is the whole run's (no separate "install the platform layer?").
	var plat *platform.Plan
	if installed, err := boxInstalled(*o.install.record); err != nil {
		return err
	} else if !installed {
		applied, err := migrateInstall(ctx, o, o.install, p, charts, out)
		if err != nil {
			return err
		}
		plat = &applied
	}
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return err
	}
	if rehearsal {
		// A rehearsal is disposable, and it starts its own box: a box that
		// already runs an app is somebody's live one, and locking it would cut
		// that app's internet without asking.
		if apps := enabledApps(sb); len(apps) > 0 {
			return fmt.Errorf("this box already runs %s, and a rehearsal is disposable: it installs its own box on a clean host, so the live box is never locked or touched", strings.Join(apps, ", "))
		}
		// The gate's address goes in with the flag: every app pod of a
		// rehearsal box waits on it until its own policy is in place, and the
		// proof below dials the same address. It is written first, because a
		// grant that says rehearsal without a gate renders nothing.
		gate, err := rehearsalGateAddress(ctx, o)
		if err != nil {
			return err
		}
		if err := sb.SetGrant("rehearsalGate", gate.String()); err != nil {
			return fmt.Errorf("record the rehearsal gate in %s: %w", *o.grantPath, err)
		}
		if err := sb.SetGrant("rehearsal", true); err != nil {
			return fmt.Errorf("record the rehearsal lock in %s: %w", *o.grantPath, err)
		}
		// Re-read the grant: the effective values below are what the lock
		// apply renders, so they must carry the flag this run just wrote.
		if sb, err = seedbox.Load(*o.grantPath, *o.valuesPath); err != nil {
			return err
		}
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return err
	}
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.install.record})
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

	chart, err := loadMigrateChart(ctx, charts)
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
	env, cs, err := migrateEnv(p, opened, o, sb, chart, cat, kc, namespace, values, rehearsal, carriedNotes(p, opened), plat, out)
	if err != nil {
		return err
	}
	if rehearsal {
		// The release must render the lock with every app still off, and the
		// proof must run before the adoption switches one on. A release whose
		// pods never had the policy would make the proof a test of nothing.
		if _, err := migrateRelease(ctx, o, chart, kc, out); err != nil {
			return err
		}
		if err := migrateProveLock(ctx, cs, chart, values, namespace, out); err != nil {
			return err
		}
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
// against is the release it installs -- and on a host with no box, the release
// bootstrap and init installed.
func loadMigrateChart(ctx context.Context, charts *chartRelease) (release.Chart, error) {
	return charts.load(ctx)
}

// migrateEnv builds everything apply touches outside the plan directory.
func migrateEnv(p *plan.Plan, opened *bundle.Opened, o *migrateApplyFlags, sb *seedbox.Seedbox, chart release.Chart, cat *catalog.Catalog, kc, namespace string, values map[string]any, rehearsal bool, carried []apply.Carried, plat *platform.Plan, out say.Reporter) (apply.Env, kubernetes.Interface, error) {
	var env apply.Env
	vols, err := sb.Storage()
	if err != nil {
		return env, nil, err
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
		return env, nil, errors.New("the grant names no storage paths: box init writes both")
	}
	// Every config apply rewrites comes out of the snapshot, one directory per
	// planned app. An app the plan names and the snapshot does not carry is a
	// refusal preflight makes, naming the app.
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	env.Catalog = cat.Names()
	// The globals are numbers in the effective document, which is where a
	// renderer reads them too.
	if env.PUID, err = number(values, "global", "puid"); err != nil {
		return env, nil, err
	}
	if env.PGID, err = number(values, "global", "pgid"); err != nil {
		return env, nil, err
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
		return env, nil, err
	}
	env.Enabled = func(instance string) bool { return apply.EnabledIn(values, instance) }

	cfg, err := kube.Config(kc)
	if err != nil {
		return env, nil, fmt.Errorf("kubeconfig %s: %w", kc, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return env, nil, fmt.Errorf("cluster client: %w", err)
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
			return env, nil, err
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
	env.Rehearsal = rehearsal
	env.Carried = carried
	env.Confirm = migrateConfirm(out, confirmer(out, *o.yes), namespace, rehearsal, *o.details, plat)
	return env, cs, nil
}

// carriedNotes is the confirm's half of the report's line about the part of a
// config a snapshot does not hold: Plex's metadata is not in it, and the
// operator reads that before the copy rather than after. It is said only for an
// app whose extracted config is exactly what discovery's table names, so a
// snapshot that carried a whole tree is never described as a subset.
//
// The note comes with the app it belongs to: the confirm shows it once, named,
// and its line does not repeat the app's name twice.
func carriedNotes(p *plan.Plan, opened *bundle.Opened) []apply.Carried {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []apply.Carried
	for _, name := range names {
		carry, ok := discover.SnapshotCarry(name)
		if !ok || carry.Note == "" {
			continue
		}
		if trimmed(opened.Configs[name], carry.Files) {
			note := strings.TrimSpace(strings.TrimPrefix(carry.Note, name+": "))
			out = append(out, apply.Carried{App: name, Note: note})
		}
	}
	return out
}

// errNotCarried stops trimmed's walk at the first entry of a config directory
// that a carried set does not name.
var errNotCarried = errors.New("not part of the snapshot")

// trimmed reports whether dir holds the files a snapshot carries for one app,
// with the directories above them, and nothing else. It stops at the first
// entry outside the set, which is what keeps a whole Plex tree cheap to refuse.
func trimmed(dir string, files []discover.CarryFile) bool {
	if dir == "" {
		return false
	}
	carried := make(map[string]bool, len(files))
	for _, f := range files {
		carried[f.Path] = true
		for i := strings.LastIndex(f.Path, "/"); i > 0; i = strings.LastIndex(f.Path[:i], "/") {
			carried[f.Path[:i]] = true
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !carried[filepath.ToSlash(rel)] {
			return errNotCarried
		}
		return nil
	})
	return err == nil
}

// migrateConfirm shows the summary and asks the one question. It is the same
// confirmer `box apply` asks with, so a terminal gets the live form and a
// --plain or piped run gets the line prompt; --yes answers without asking, and
// no terminal without --yes refuses, as apply does.
//
// It is the run's only question. On a host that had no box, the platform this
// run installed is part of it, shown first: a run that asked "install the
// platform layer?" and then "move these into ...?" was two questions about one
// decision.
func migrateConfirm(out say.Reporter, ask asker, namespace string, rehearsal bool, details bool, plat *platform.Plan) func(apply.Summary) (bool, error) {
	return func(s apply.Summary) (bool, error) {
		if plat != nil {
			if items := plat.Items(); len(items) > 0 {
				out.Plan("platform", items)
			}
		}
		if details {
			// Every rewrite, column by column: the list report.txt holds, for
			// the operator who wants to check one row before saying yes.
			out.List("this migration", s.Lines())
		} else {
			g := s.Grouped()
			if g.Lead != "" {
				out.Muted(g.Lead)
			}
			for _, block := range g.Blocks {
				out.Table(block.Title, block.Rows)
			}
			if g.Footer != "" {
				out.Muted(g.Footer)
			}
		}
		if rehearsal {
			// The screen a person reads before the first write says what this
			// box is, not only what moves into it.
			out.List("this rehearsal", []string{
				"nothing leaves this box: no internet egress, and the release's own pods only",
				"every torrent starts stopped, and no data is renamed",
			})
			return ask.Ask(fmt.Sprintf("rehearse these into %s?", namespace))
		}
		return ask.Ask(fmt.Sprintf("move these into %s?", namespace))
	}
}

// migrateHandOver is step 11: the apply. It is `box apply`'s own path, with the
// chart already loaded and the question already answered.
func migrateHandOver(ctx context.Context, o *migrateApplyFlags, chart release.Chart, kc string, out say.Reporter) error {
	res, err := migrateRelease(ctx, o, chart, kc, out)
	if err != nil {
		return err
	}
	if res.Host != "" {
		out.Banner("seedbox ready", res.Host)
	}
	return nil
}

// migrateRelease runs `box apply`'s own path once: the same chart, the same
// files and the same hosts, against the cluster this run already holds. It is
// what renders a change to the release, so a rehearsal calls it twice -- once
// to put the lock in place before any app starts, and once at the end to switch
// on what the adoption moved -- and a final snapshot calls it once.
func migrateRelease(ctx context.Context, o *migrateApplyFlags, chart release.Chart, kc string, out say.Reporter) (release.Result, error) {
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
	return applyThenSettle(ctx, app.Apply, opts, holds)
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

// rehearsalInternet is the public address a rehearsal's second check dials. It
// only ever adds a refusal (a pod carrying the release's label reached it, so
// the egress policy is absent); not reaching it is also what a host with no
// internet looks like, and the in-cluster proof is the one that counts.
var rehearsalInternet = lock.Target{Host: "1.1.1.1", Port: 443}

// rehearsalSourceHost refuses a rehearsal on the host that holds the source's
// data. A locked Arr still writes the library, and adopting a tree that the
// live source is using is exactly the thing a rehearsal must not do; a
// rehearsal is for another host or VM.
//
// Two answers, either one enough: the plan names this host as its source, or
// the plan's data root exists here and is not empty. The host name is the
// source's node name, which on a single-node box is the host's own; the data
// root is what is actually at stake, and a source on another machine can still
// have left a tree here.
func rehearsalSourceHost(p *plan.Plan) error {
	var why []string
	if host, err := os.Hostname(); err == nil && p.Source != nil && sameHost(p.Source.Host, host) {
		why = append(why, fmt.Sprintf("source.host is %s, this host", p.Source.Host))
	}
	if p.Data != nil && strings.TrimSpace(p.Data.Root) != "" {
		if entries, err := os.ReadDir(p.Data.Root); err == nil && len(entries) > 0 {
			why = append(why, fmt.Sprintf("%s exists here and is not empty", p.Data.Root))
		}
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("this host holds the source's data (%s), and a rehearsal must not run beside the live box: rehearse on another host or VM", strings.Join(why, "; "))
}

// sameHost compares two host names by their short form: a plan may name a node
// as a bare name or as a fully-qualified one, and both mean the same machine.
func sameHost(a, b string) bool {
	short := func(s string) string {
		s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
		if i := strings.IndexByte(s, '.'); i > 0 {
			return s[:i]
		}
		return s
	}
	return short(a) != "" && short(a) == short(b)
}

// grantLocked reports whether this host's grant records a rehearsal box. A
// missing grant is not a lock: a host with no box has no grant yet.
func grantLocked(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	sb, err := seedbox.Load(path, "")
	if err != nil {
		return false, err
	}
	on, _ := sb.Grant.Map()["rehearsal"].(bool)
	return on, nil
}

// enabledApps is the apps this box's grant switches on, sorted, so a refusal
// can name them.
func enabledApps(sb *seedbox.Seedbox) []string {
	apps, _ := sb.Grant.Map()["apps"].(map[string]any)
	var out []string
	for name, raw := range apps {
		entry, _ := raw.(map[string]any)
		if on, _ := entry["enabled"].(bool); on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// rehearsalGateAddress is the in-cluster address a rehearsal box's gate and
// proof dial: the cluster DNS Service's metrics port, which the lock denies.
func rehearsalGateAddress(ctx context.Context, o *migrateApplyFlags) (lock.Target, error) {
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.install.record})
	if err != nil {
		return lock.Target{}, err
	}
	cfg, err := kube.Config(kc)
	if err != nil {
		return lock.Target{}, fmt.Errorf("kubeconfig %s: %w", kc, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return lock.Target{}, fmt.Errorf("cluster client: %w", err)
	}
	return lock.DNSTarget(ctx, lock.Cluster{Client: cs})
}

// migrateProveLock proves the release's egress lock from inside the namespace,
// after the release rendered it with every app still off and before the
// adoption switches one on. It uses the s5n image the release already runs, so
// no new image is pulled and the blessed-image policy admits the pods.
func migrateProveLock(ctx context.Context, cs kubernetes.Interface, chart release.Chart, values map[string]any, namespace string, out say.Reporter) error {
	image, err := apply.WiringImage(chart.Variants, chart.AppVersion, values)
	if err != nil {
		return err
	}
	client := lock.Cluster{Client: cs}
	// The proof dials the address every app's gate will: the one in the grant.
	target, err := lock.ParseTarget(fmt.Sprint(values["rehearsalGate"]))
	if err != nil {
		return fmt.Errorf("the rehearsal gate in the grant: %w", err)
	}
	h := out.Start(say.Op{
		Label: "proving the network lock",
		Plain: "network lock: proving it from inside " + namespace,
	})
	rep, err := lock.Prove(ctx, client, lock.Options{
		Namespace: namespace,
		Release:   release.ReleaseName,
		Image:     image,
		Target:    target,
		Internet:  rehearsalInternet,
		// Each probe dials until its answer holds (a CNI programs a new pod's
		// policy a moment after it starts), so a pod lives up to Settle plus
		// its start-up.
		Settle:  20 * time.Second,
		Timeout: 90 * time.Second,
		Poll:    time.Second,
	})
	if err != nil {
		h.Failed(err)
		return err
	}
	h.Done(fmt.Sprintf("network lock: proven (%s is unreachable from a pod carrying this release's label, and reachable without it)", target))
	if rep.Note != "" {
		out.Muted(rep.Note)
	}
	return nil
}
