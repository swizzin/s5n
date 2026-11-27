package main

// migrate_apply.go is the write half: `box migrate apply [<snapshot>]`. The
// testable decision lives in internal/migrate/apply; this file opens the
// snapshot, reads the plan, the seedbox and the deployed release, builds the
// environment and hands over to `box apply`'s path. Nothing is written until all
// of that has been read.

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
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/migrate/work"
	"github.com/swizzin/s5n/internal/netproof"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/safefs"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
)

// migrateApplyFlags are `box migrate apply`'s flags: apply's own (the grant,
// the values file, the kubeconfig, --yes and the chart reference) plus the one
// this command adds.
type migrateApplyFlags struct {
	*applyFlags
	// install answers are for a host with no box; ignored when one exists.
	install *installFlags
	plain   *bool
	// details prints every rewrite; the report has the whole list either way.
	details *bool
	// yolo allows a downgrade: an older app on a newer config.
	yolo *bool
}

func addMigrateApplyFlags(fs *flag.FlagSet) *migrateApplyFlags {
	return &migrateApplyFlags{
		applyFlags: addApplyFlags(fs),
		install:    addInstallFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
		details:    fs.Bool("details", false, "print every rewrite, column by column, instead of the grouped summary"),
		yolo:       fs.Bool("yolo", false, "run a downgrade: an older app on a config a newer one wrote (refused otherwise)"),
	}
}

func runMigrateApply(ctx context.Context, args []string) {
	// --paths-in-node is the e2e's switch: a host with no box runs init here.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --unit-dir names where a fresh host's timer units land.
	unitDir, args, err := takeDevFlag(args, "unit-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate apply", flag.ExitOnError)
	o := addMigrateApplyFlags(fs)
	o.install.pathsInNode = pathsInNode
	o.install.unitDir = unitDir
	// The handed-over apply reads the same switch: a kind host cannot stat them.
	o.pathsInNode = pathsInNode
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// Go's flag parser stops at the snapshot, so parse what follows it as flags.
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

// openSnapshot finds and verifies the snapshot, extracting what the run reads
// into a private directory beside it. No file means the newest non-survey in
// bundle.Dir. The mode decides what apply may do: only a final is a real run.
func openSnapshot(file string, out say.Reporter) (*bundle.Opened, func(), error) {
	if strings.TrimSpace(file) == "" {
		newest, err := bundle.Newest(bundle.Dir)
		if err != nil {
			return nil, func() {}, err
		}
		file = newest
	}
	// Read the plan first: an unparseable plan refuses before extraction, and
	// whole-travelling bulk stays in the snapshot for staging.
	p, err := snapshotPlan(file)
	if err != nil {
		return nil, func() {}, err
	}
	// Work beside the snapshot, never /tmp, which is RAM on Fedora and Arch.
	dir, done, err := work.New(filepath.Dir(file))
	if err != nil {
		return nil, func() {}, fmt.Errorf("a private directory for the snapshot: %w", err)
	}
	cleanup := done
	opened, err := bundle.Open(file, filepath.Join(dir, "snapshot"), keepFor(p), nil)
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
	// The file goes to the log: a refusal's command already names it (rule 1).
	out.Detail(fmt.Sprintf("snapshot: %s (%s)", file, opened.Mode))
	return opened, cleanup, nil
}

// keepFor extracts the plan, report, sources and every config whole, except an
// app whose config is also a library: a check reads its state, and apply.Bulk
// writes its tree once at the config root. The plan says which app and carry.
func keepFor(p *plan.Plan) archive.Filter {
	return func(name string) bool {
		rest, ok := strings.CutPrefix(name, archive.ConfigPrefix+"/")
		if !ok {
			return true
		}
		instance, rel, _ := strings.Cut(rest, "/")
		if p.Apps[instance].Carry != plan.CarryFull {
			return true
		}
		if _, ok := discover.SnapshotCarry(instance, true); !ok {
			return true
		}
		return plex.State(rel)
	}
}

// snapshotPlan reads the plan before extraction, to decide what to keep.
func snapshotPlan(file string) (*plan.Plan, error) {
	raw, err := archive.ReadEntry(file, archive.PlanName)
	if err != nil {
		return nil, err
	}
	return parsePlan(raw, file)
}

// parsePlan names the snapshot and entry a refusal is about.
func parsePlan(raw []byte, file string) (*plan.Plan, error) {
	p, err := plan.Parse(raw)
	if err != nil {
		var perr *plan.Error
		if errors.As(err, &perr) {
			perr.Path = file + ":" + archive.PlanName
		}
		return nil, err
	}
	return p, nil
}

// migrateApply is the command without the exit. The snapshot is extracted into a
// private directory for the run; apply rewrites its copies, never the source's.
// A rehearsal sets the grant's lock, applies with every app off, proves the lock
// from inside the namespace, and only then switches apps on.
func migrateApply(ctx context.Context, file string, o *migrateApplyFlags, out say.Reporter) error {
	opened, cleanup, err := openSnapshot(file, out)
	if err != nil {
		return err
	}
	defer cleanup()
	// Step 0: the plan, then the host -- a fresh one gets a box, an existing one
	// is adopted as it stands.
	p, err := loadPlan(opened)
	if err != nil {
		return err
	}
	rehearsal := opened.Mode == archive.ModeRehearsal
	if rehearsal {
		// Phase head and fact both say what this run is.
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
	// installed means this run is the installer: it shows the run, asks once,
	// then does it. agreed is that answer; nil on an existing box, where the
	// question comes inside apply.Run after every check.
	installed, err := boxInstalled(*o.install.record)
	if err != nil {
		return err
	}
	// One release for both halves: the pre-install pass and the install.
	chart, err := loadMigrateChart(ctx, charts)
	if err != nil {
		return err
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return err
	}
	var agreed *apply.Summary
	if !installed {
		steps, err := migrationStepsWith(ctx, o, o.install, p, opened, raw, charts, chart, cat, rehearsal, out)
		if err != nil {
			return err
		}
		agreed, err = steps.beforeInstall(ctx, confirmer(out, *o.yes), askOptionsOf(o, rehearsal), installTarget(o.install), confirmScreenOf(opened, installTarget(o.install)), out)
		if err != nil {
			return err
		}
	}
	// The snapshot, plan and chart refusals have all happened; on a fresh host
	// the version column and the question did too. What follows reads the
	// cluster, then writes.
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return err
	}
	// Refuse a plan this release cannot honour before the grant is touched: an
	// unnamed instance, or an app with no port or URL base. The version column's
	// refusal stays with the confirm, because apply.Run decides it and an
	// already-applied plan is its own no-op.
	if effective, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema}); err != nil {
		return err
	} else if _, err := planFacts(p, cat, effective, chart); err != nil {
		return err
	}
	if rehearsal {
		// A rehearsal starts its own box: locking a live one would cut its
		// internet without asking.
		if apps := enabledApps(sb); len(apps) > 0 {
			return fmt.Errorf("this box already runs %s, and a rehearsal is disposable: it installs its own box on a clean host, so the live box is never locked or touched", strings.Join(apps, ", "))
		}
		// Write the gate address first: a grant saying rehearsal without one
		// renders nothing. Every pod waits on it, and the proof dials it.
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
		// Re-read the grant so the values carry the flag just written.
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
	// The release is what migrate adopts into: claims hold staging, its wiring
	// record takes the adopted connections.
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

	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return err
	}
	screen := confirmScreenOf(opened, namespace)
	env, cs, err := migrateEnv(p, opened, o, sb, chart, cat, kc, namespace, values, rehearsal, agreed, screen, out)
	if err != nil {
		return err
	}
	if rehearsal {
		// Render the lock with every app off before proving it: a pod that never
		// had the policy proves nothing.
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
		if agreed != nil && apply.StoppedBeforeWrite(err) {
			// The box is installed and nothing of the snapshot moved: say both,
			// and what a re-run does.
			return fmt.Errorf("%w\n\nthe box is installed and nothing of the snapshot has been moved into it: fix what this names, then run this again -- the next run finds a box already, so it asks its one question after these checks and carries on from here", err)
		}
		return err
	}
	if res.Done {
		out.Line(fmt.Sprintf("%s in %s: the plan is already applied", release.ReleaseName, namespace))
	} else {
		out.Rows([]say.Row{{
			Mark:  say.MarkDone,
			Name:  "moved",
			Facts: []string{appsWord(len(res.Moved)), "into " + env.ConfigRoot},
		}})
	}
	out.Rows([]say.Row{{
		Mark:    say.MarkNote,
		Name:    "report",
		Facts:   []string{screen.Report},
		Command: "less " + screen.Report,
	}})

	// Step 11: `box apply`'s own path, re-reading the files this run moved.
	return migrateHandOver(ctx, o, chart, kc, out)
}

// loadPlan parses the plan the snapshot carries.
func loadPlan(opened *bundle.Opened) (*plan.Plan, error) {
	return parsePlan(opened.Plan, opened.Path)
}

// snapshotBulk is the unevicted part of a snapshot, counted and staged once.
type snapshotBulk struct{ opened *bundle.Opened }

// HoldsTree says the snapshot's copy is more than extraction kept.
func (b snapshotBulk) HoldsTree(instance string) bool { return b.opened.Contents[instance].Tree }

// Measure is what staging one instance writes at the config root.
func (b snapshotBulk) Measure(instance string) (apply.Staged, error) {
	c, ok := b.opened.Contents[instance]
	if !ok {
		return apply.Staged{}, fmt.Errorf("%s: the snapshot holds no config for it", instance)
	}
	return apply.Staged{Files: c.Files, Bytes: c.Bytes}, nil
}

// Stage writes the whole of one instance's config out of the snapshot, into the
// name it has inside the run's config root.
func (b snapshotBulk) Stage(instance string, config *safefs.Anchor, name string, uid, gid int) (apply.Staged, error) {
	c, err := b.opened.Stage(instance, config, name, uid, gid)
	if err != nil {
		return apply.Staged{}, err
	}
	return apply.Staged{Files: c.Files, Bytes: c.Bytes}, nil
}

// loadMigrateChart loads the one release this command checks and installs.
func loadMigrateChart(ctx context.Context, charts *chartRelease) (release.Chart, error) {
	return charts.load(ctx)
}

// migrateEnv builds everything apply touches outside the plan directory.
func migrateEnv(p *plan.Plan, opened *bundle.Opened, o *migrateApplyFlags, sb *seedbox.Seedbox, chart release.Chart, cat *catalog.Catalog, kc, namespace string, values map[string]any, rehearsal bool, agreed *apply.Summary, screen confirmScreen, out say.Reporter) (apply.Env, kubernetes.Interface, error) {
	var env apply.Env
	vols, err := sb.Storage()
	if err != nil {
		return env, nil, err
	}
	for _, v := range vols {
		switch v.Kind {
		case volumeConfig:
			env.ConfigRoot = v.Path
		case volumeData:
			env.DataRoot = v.Path
		}
	}
	if env.ConfigRoot == "" || env.DataRoot == "" {
		return env, nil, errors.New("the grant names no storage paths: box init writes both")
	}
	// One directory per planned app; a missing one is preflight's refusal.
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	// What extraction left behind for an app that is also a library.
	env.Bulk = snapshotBulk{opened: opened}
	env.Catalog = cat.Names()
	env.Image = chartImage(chart, values)
	// The globals are numbers in the effective document.
	if env.PUID, err = number(values, "global", "puid"); err != nil {
		return env, nil, err
	}
	if env.PGID, err = number(values, "global", "pgid"); err != nil {
		return env, nil, err
	}
	// Every instance a rewriter may meet: the plan's and the release's.
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

	// The runner is built only when the plan names the app whose database only
	// its own image may write.
	if planNeedsRunner(p, env.Catalog) {
		image, err := apply.Image(chart.Variants, values, "plex")
		if err != nil {
			return env, nil, err
		}
		claim := ""
		for _, v := range vols {
			if v.Kind == volumeConfig {
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
	env.Confirm = migrateAnswer(out, o, namespace, rehearsal, agreed, screen)
	return env, cs, nil
}

// askOptions is how one run asks its one question: what the screen shows, and
// what it refuses.
type askOptions struct {
	// Rehearsal says the screen shows what a rehearsal leaves alone.
	Rehearsal bool
	// Details prints every rewrite instead of the grouped summary.
	Details bool
	// AllowDowngrades is --yolo.
	AllowDowngrades bool
}

// askOptionsOf is this run's flags; a nil flag reads as absent.
func askOptionsOf(o *migrateApplyFlags, rehearsal bool) askOptions {
	return askOptions{
		Rehearsal:       rehearsal,
		Details:         o.details != nil && *o.details,
		AllowDowngrades: o.yolo != nil && *o.yolo,
	}
}

// migrateAnswer holds a fresh install to the answer it already gave; an existing
// box asks here, after every app's Check.
func migrateAnswer(out say.Reporter, o *migrateApplyFlags, namespace string, rehearsal bool, agreed *apply.Summary, screen confirmScreen) func(apply.Summary) (bool, error) {
	if agreed != nil {
		return migrateAgreed(out, *agreed)
	}
	return migrateConfirm(out, confirmer(out, *o.yes), namespace, askOptionsOf(o, rehearsal), nil, "", screen)
}

// confirmScreenOf is one run's heading and report path.
func confirmScreenOf(opened *bundle.Opened, target string) confirmScreen {
	when := ""
	if !opened.Taken.IsZero() {
		when = " from " + opened.Taken.UTC().Format("2006-01-02 15:04 UTC")
	}
	return confirmScreen{
		Head:     fmt.Sprintf("box migrate apply · %s · %s snapshot%s", confirmTarget(target), opened.Mode, when),
		Snapshot: opened.Path,
		Report:   report.ApplyName(opened.Path),
		Kind:     string(opened.Mode),
		Taken:    opened.Taken.Format(time.RFC3339),
	}
}

// migrateConfirm shows the summary and asks the run's one question with apply's
// confirmer. A fresh host's question covers the platform it will install, so a
// person reads that from the screen. The report is written before the question,
// so a no still has what was shown.
func migrateConfirm(out say.Reporter, ask asker, namespace string, opts askOptions, plat *platform.Plan, lead string, screen confirmScreen) func(apply.Summary) (bool, error) {
	target := confirmTarget(namespace)
	return func(s apply.Summary) (bool, error) {
		// The version column's refusal, only when there is something to move.
		if err := refuseDowngrades(out, s.Versions(), opts.AllowDowngrades, screen.Snapshot); err != nil {
			return false, err
		}
		if err := showSummary(out, s, opts.Details, plat, lead, screen); err != nil {
			return false, err
		}
		// Default no: a migration rewrites configs and switches apps on.
		if opts.Rehearsal {
			// Say what this box is, not only what moves into it.
			out.List("this rehearsal", []string{
				"nothing leaves this box: no internet egress, and the release's own pods only",
				"every torrent starts stopped, and no data is renamed",
			})
			return ask.Ask(fmt.Sprintf("rehearse these %s into %s?", appsWord(len(s.Instances)), target), false)
		}
		if plat != nil {
			return ask.Ask(fmt.Sprintf("install, then move these %s in?", appsWord(len(s.Instances))), false)
		}
		return ask.Ask(fmt.Sprintf("move these %s in?", appsWord(len(s.Instances))), false)
	}
}

// refuseDowngrades stops an older app on a config a newer version wrote: the
// database belongs to the writer, and a migration is the last moment to choose.
// --yolo puts a `!` row per app instead. It reads two image references, so it
// answers before staging a file, and Reported keeps the line from printing twice.
func refuseDowngrades(out say.Reporter, versions []apply.AppVersion, allow bool, snapshot string) error {
	if allow {
		return nil
	}
	downs := apply.Downgrades(versions)
	if len(downs) == 0 {
		return nil
	}
	rows := make([]say.Row, 0, len(downs))
	for _, v := range downs {
		rows = append(rows, say.Row{
			Mark:  say.MarkFailed,
			Name:  v.App,
			Facts: []string{v.Source + " → " + v.Box},
			Why:   "the config is newer than the app",
		})
	}
	out.Rows(rows)
	command := "box migrate apply"
	if snapshot != "" {
		command += " " + snapshot
	}
	command += " --yolo"
	commandLines(out, command)
	out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{"nothing has moved"}}})
	return say.Reported{Err: errDowngrade}
}

// errDowngrade says the release is older than the source, and the refusal has
// already been printed with the command that runs it anyway.
var errDowngrade = errors.New("the release runs an older app than wrote this config")

// planFacts is the plan against the chart: versions per app, and the error an
// unhonourable plan makes. No configs or cluster, so the installer can refuse a
// downgrade before probing or staging.
func planFacts(p *plan.Plan, cat *catalog.Catalog, values map[string]any, chart release.Chart) ([]apply.AppVersion, error) {
	if _, err := apply.Targets(values, cat.Names(), sortedKeys(p.Apps)); err != nil {
		return nil, err
	}
	return apply.Versions(p, cat.Names(), chartImage(chart, values)), nil
}

// showSummary is the screen above the question and its page.
func showSummary(out say.Reporter, s apply.Summary, details bool, plat *platform.Plan, lead string, screen confirmScreen) error {
	if screen.Head != "" {
		out.Line(screen.Head)
		out.Line("")
	}
	if plat != nil {
		if lead != "" {
			out.Muted(lead)
		}
		// The folded plan: the confirm is a decision, not a component list.
		if items := plat.Summary(); len(items) > 0 {
			out.Plan("platform", items)
		}
		out.Line("")
	}
	// Write the page whatever the screen shape; --details prints the list.
	if err := writeApplyReport(out, s, screen); err != nil {
		return err
	}
	if details {
		// Every rewrite, for the operator checking a row before yes.
		out.List("this migration", s.Lines())
		return nil
	}
	g := s.Grouped()
	if g.Lead != "" {
		out.Muted(g.Lead)
	}
	for _, block := range g.Blocks {
		out.Group(block)
	}
	if g.Footer != "" {
		out.Note(g.Footer)
	}
	// The log keeps the whole account, and the report keeps the page.
	for _, line := range s.Lines() {
		out.Detail(line)
	}
	return nil
}

// writeApplyReport puts the page beside the snapshot, 0600, before the question.
func writeApplyReport(out say.Reporter, s apply.Summary, screen confirmScreen) error {
	if screen.Report == "" {
		return nil
	}
	taken := time.Now().UTC()
	if at, err := time.Parse(time.RFC3339, screen.Taken); err == nil {
		taken = at.UTC()
	}
	doc := s.Report(screen.Kind, screen.Snapshot, taken, version)
	if err := report.Write(screen.Report, doc); err != nil {
		return err
	}
	out.Detail("report: " + screen.Report)
	return nil
}

// appsWord is "one app" or "12 apps", for the question.
func appsWord(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// confirmLead says what a fresh host's answer buys; the cluster is named only
// when box installs it.
func confirmLead(installsCluster bool) string {
	if installsCluster {
		return "nothing is installed yet: this run installs the cluster and the platform layer, writes the box, and then moves the snapshot into it"
	}
	return "nothing is installed yet: this run installs the platform layer, writes the box, and then moves the snapshot into it"
}

// confirmTarget is the namespace, or "this box" before one is chosen.
func confirmTarget(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	return "this box"
}

// migrateAgreed refuses a summary that is not the one agreed; the only allowed
// difference is rows the pre-install pass could not check.
func migrateAgreed(out say.Reporter, agreed apply.Summary) func(apply.Summary) (bool, error) {
	return func(s apply.Summary) (bool, error) {
		if why := s.Holds(agreed); why != "" {
			return false, fmt.Errorf("this run would not do what it asked about, and nothing of the snapshot has been moved: %s. The snapshot was read twice from the same file and the plan is the same, so this is a fault in box, not something to answer differently", why)
		}
		// Fill in the rows the question promised after the install.
		if rows := s.CheckedRows(agreed); len(rows) > 0 {
			out.Table("checked after the install", rows)
		}
		return true, nil
	}
}

// migrateHandOver is step 11: `box apply`'s path, chart loaded, question answered.
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

// migrateRelease runs `box apply`'s path: a rehearsal twice (lock, then switch
// on), a final once.
func migrateRelease(ctx context.Context, o *migrateApplyFlags, chart release.Chart, kc string, out say.Reporter) (release.Result, error) {
	app := &release.Applier{
		Charts:  chartSource(out),
		Confirm: answered{},
		Say:     out,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return release.NewHelm(kc, namespace, release.ReleaseName)
		},
		OpenSecret: func(namespace string) (release.SecretKeys, error) {
			return release.NewSecret(kc, namespace)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}
	opts := o.options()
	opts.Chart = &chart
	opts.BoxValues = boxValues(kc)
	// A migration installs apps into a seedbox like any apply, so the cluster's
	// policy enforcement is proved for it too: a rehearsal's egress lock says
	// nothing about a final apply on a cluster box never proved.
	opts.NetworkProof = networkPolicyProof(kc, *o.install.record, out)
	// The same steps a bare apply runs, per-host TLS included.
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: setup.NetResolver{}}
	holds := newBoxHolds(out)
	opts.Holds = holds.values
	return applyThenSettle(ctx, app.Apply, opts, holds)
}

// releaseSecretName is `<release>-keys` or `secrets.existingSecret`.
func releaseSecretName(values map[string]any) string {
	secrets, _ := values["secrets"].(map[string]any)
	if name, _ := secrets["existingSecret"].(string); strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return release.ReleaseName + "-keys"
}

// secretStore is the release Secret; corev1.Secret's Data is already decoded.
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
	// The read carries a resourceVersion, so a concurrent write is refused.
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

// grantEditor switches an instance on. Both keys are grant keys, so this edits
// the grant: a grant key in values is refused by the merge.
type grantEditor struct {
	sb      *seedbox.Seedbox
	catalog []string
	values  map[string]any
}

func (g *grantEditor) Enable(instance string, planned plan.App) error {
	app := apply.CatalogApp(instance, g.catalog)
	if app == "" {
		return fmt.Errorf("%s: no app in the chart's catalog is named that", instance)
	}
	if planned.MediaServer != "" && app != appSeerr {
		return fmt.Errorf("%s: the plan names a mediaServer, and only a Seerr is set up against one", instance)
	}
	keys := plannedKeys(planned)
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
			// No list: write the chart's whole default list with this one on.
			edits = append(edits, seedbox.Edit{
				Path:  "apps." + app + ".instances",
				Value: switchOn(defaults, suffix, app, keys),
			})
		default:
			if index, _ := findInstance(own, suffix); index >= 0 {
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d].enabled", app, index),
					Value: true,
				})
				for _, k := range keyOrder(keys) {
					edits = append(edits, seedbox.Edit{
						Path:  fmt.Sprintf("apps.%s.instances[%d].%s", app, index, k),
						Value: keys[k],
					})
				}
			} else {
				// A new instance: append an entry with the schema's keys.
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d]", app, len(own)),
					Value: newInstance(app, suffix, keys),
				})
			}
		}
	}
	// One atomic write: an enabled app with no instance entry is one fewer than
	// asked for.
	editor := g.sb.Grant.Editor()
	if err := editor.SetAll(edits); err != nil {
		return err
	}
	return editor.Save()
}

// grantInstances is the grant's own list and whether it names one.
func grantInstances(sb *seedbox.Seedbox, app string) ([]any, bool) {
	apps, _ := sb.Grant.Map()["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	list, ok := entry["instances"].([]any)
	return list, ok
}

// instancesOf is an app's effective list, if it takes instances.
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

// plannedKeys is what the plan says about one instance that its entry keeps: the
// torrents' category when it differs from the library directory, and the server
// a Seerr signs in to.
func plannedKeys(planned plan.App) map[string]any {
	keys := map[string]any{}
	if planned.DownloadCategory != "" {
		keys["downloadCategory"] = planned.DownloadCategory
	}
	if planned.MediaServer != "" {
		keys["mediaServer"] = planned.MediaServer
	}
	return keys
}

// keyOrder is an entry's planned keys in order, so the edits are the same every run.
func keyOrder(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newInstance is an entry for an instance the list does not carry yet: the
// schema's keys, a category only for an app with a library directory.
func newInstance(app, suffix string, keys map[string]any) map[string]any {
	entry := map[string]any{"suffix": suffix, "enabled": true}
	if hasCategory(app) {
		entry["category"] = categoryFor(app, suffix)
	}
	for k, v := range keys {
		entry[k] = v
	}
	return entry
}

// switchOn enables one suffix, adding the entry when absent.
func switchOn(list []any, suffix, app string, keys map[string]any) []any {
	out := make([]any, 0, len(list)+1)
	found := false
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		got, _ := entry["suffix"].(string)
		if got == suffix {
			found = true
			entry = cloneEntry(entry)
			entry["enabled"] = true
			for k, v := range keys {
				entry[k] = v
			}
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, newInstance(app, suffix, keys))
	}
	return out
}

// cloneEntry copies an entry so the edit does not mutate its source.
func cloneEntry(entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		out[k] = v
	}
	return out
}

// hasCategory says whether an app's instances name a library directory. Seerr has none:
// it requests from a media server and keeps no root folders, and its schema refuses a
// category key outright.
func hasCategory(app string) bool { return app != appSeerr }

// appSeerr is Seerr's catalog name: the one app with instances and no library.
const appSeerr = "seerr"

// categoryFor is the suffix, or the app name for a primary: it is one
// qBittorrent category and one directory per /data tree, and cannot be empty.
func categoryFor(app, suffix string) string {
	if suffix == "" {
		return app
	}
	return suffix
}

// planNeedsRunner says a plan names the app whose rewriter needs an image.
func planNeedsRunner(p *plan.Plan, catalogApps []string) bool {
	for name := range p.Apps {
		if apply.NeedsRunner(apply.CatalogApp(name, catalogApps)) {
			return true
		}
	}
	return false
}

// The grant's two storage keys, as `box init` writes them.
const (
	volumeData   = "data"
	volumeConfig = "config"
)

// wiringConfigMap is the wiring Job's record (wiring.yaml).
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

// rehearsalInternet is the public address a rehearsal must not reach. Failing to
// reach it is not proof: the in-cluster check is the one that counts.
var rehearsalInternet = netproof.Target{Host: "1.1.1.1", Port: 443}

// rehearsalSourceHost refuses to rehearse beside the live source: a locked Arr
// still writes the library. Either the plan names this host, or its data root
// exists here non-empty -- a source elsewhere can still have left one.
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

// sameHost compares short forms: bare and fully-qualified names are one host.
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

// grantLocked: a missing grant is not a lock -- a host with no box has none.
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

// enabledApps is the grant's enabled apps, sorted.
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

// rehearsalGateAddress is the DNS metrics port the lock denies.
func rehearsalGateAddress(ctx context.Context, o *migrateApplyFlags) (netproof.Target, error) {
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.install.record})
	if err != nil {
		return netproof.Target{}, err
	}
	cfg, err := kube.Config(kc)
	if err != nil {
		return netproof.Target{}, fmt.Errorf("kubeconfig %s: %w", kc, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return netproof.Target{}, fmt.Errorf("cluster client: %w", err)
	}
	return netproof.DNSTarget(ctx, netproof.Cluster{Client: cs})
}

// migrateProveLock proves the egress lock inside the namespace, using the
// release's own image so the blessed-image policy admits the pods.
func migrateProveLock(ctx context.Context, cs kubernetes.Interface, chart release.Chart, values map[string]any, namespace string, out say.Reporter) error {
	image, err := apply.WiringImage(chart.Variants, chart.AppVersion, values)
	if err != nil {
		return err
	}
	client := netproof.Cluster{Client: cs}
	// The proof dials the address every app's gate will: the one in the grant.
	target, err := netproof.ParseTarget(fmt.Sprint(values["rehearsalGate"]))
	if err != nil {
		return fmt.Errorf("the rehearsal gate in the grant: %w", err)
	}
	h := out.Start(say.Op{
		Label: "proving the network lock",
		Plain: "network lock: proving it from inside " + namespace,
	})
	rep, err := netproof.ProveEgress(ctx, client, netproof.Options{
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

// chartImage is the image a release runs for one catalog app, resolved the way
// the chart's own template resolves it: the variant the values select, a pinned
// tag or digest narrowing it inside that repository. It is what the confirm's
// version column compares the source's image against, so the version a person
// reads is the one the release will run -- a hold an update wrote included.
func chartImage(chart release.Chart, values map[string]any) func(app string) string {
	return func(app string) string {
		ref, err := apply.Image(chart.Variants, values, app)
		if err != nil {
			// A chart with no image for the app leaves that row `?`: the run is
			// already refused elsewhere if it cannot install the app at all.
			return ""
		}
		return ref
	}
}
