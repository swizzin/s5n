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
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/migrate/work"
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
	// yolo runs a downgrade: the release's app is older than the version that
	// wrote the source's config, which box refuses unless this is given.
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
	// --paths-in-node is the e2e's storage switch, taken out of the arguments
	// before the FlagSet sees them: a host with no box runs init here, and
	// init's dev switch has to reach it the same way `box bootstrap`'s does.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --unit-dir is bootstrap's timer switch, taken out of the arguments the
	// same way: on a host with no box this run installs one, and the timer it
	// writes for it lands in the directory this run names.
	unitDir, args, err := takeDevFlag(args, "unit-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate apply", flag.ExitOnError)
	o := addMigrateApplyFlags(fs)
	o.install.pathsInNode = pathsInNode
	o.install.unitDir = unitDir
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

// openSnapshot finds the snapshot apply runs, verifies it, and extracts the part
// this run reads into a private directory beside it. With no file named it is the
// newest one in bundle.Dir that is not a survey, and the run says which file it
// read.
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
	// The plan says which part of each app's config this run's checks read, so it
	// is read out of the archive first: a plan box cannot parse is a refusal
	// before anything is extracted, and the bulk the plan says travels whole is
	// left in the snapshot for the staging step.
	p, err := snapshotPlan(file)
	if err != nil {
		return nil, func() {}, err
	}
	// The run works beside the snapshot and never in the system's temporary
	// directory: what it extracts is the same disk the snapshot is already on, and
	// on a Fedora or an Arch host /tmp is RAM.
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
	out.Line(fmt.Sprintf("snapshot: %s (%s)", file, opened.Mode))
	return opened, cleanup, nil
}

// keepFor is what an apply extracts out of its snapshot before its checks: the
// plan, the report and the sources, and every app's config whole -- except an app
// whose config is a library as well as its state, whose state is what a check
// reads and whose tree is written at the config root, once, by apply.Bulk.
//
// The plan is what says which app that is and which carry the snapshot took of
// it: a snapshot that carried the state alone has no tree to leave behind.
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

// snapshotPlan is the plan out of a snapshot, before any of it is extracted: it
// is what says which part of each app's config the extraction has to keep.
func snapshotPlan(file string) (*plan.Plan, error) {
	raw, err := archive.ReadEntry(file, archive.PlanName)
	if err != nil {
		return nil, err
	}
	return parsePlan(raw, file)
}

// parsePlan parses a plan that came out of one snapshot, naming the snapshot and
// the entry a refusal is about: that file is what an operator copies between
// hosts and what a bug report has to name.
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
	// installed says this host has no box, so this run is the installer: it
	// shows the whole run once and then does it, in that order. agreed is what
	// the operator said yes to -- nil on a host that already has a box, where
	// the one question is asked inside apply.Run after every check, as it always
	// was.
	installed, err := boxInstalled(*o.install.record)
	if err != nil {
		return err
	}
	// The release is read once, for both halves: the pre-install pass needs its
	// values and ports, and the adoption installs this same chart.
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
		if agreed != nil && apply.StoppedBeforeWrite(err) {
			// The install this run was told to make is done, and its checks
			// stopped the migration before anything of the snapshot moved: a
			// person has to know both, and what running it again does.
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

	// Step 11: the same apply `box apply` runs, with the answer the operator
	// gave above. The files are re-read there, which is what makes the release
	// render what this run just moved into place.
	return migrateHandOver(ctx, o, chart, kc, out)
}

// loadPlan parses the plan the snapshot carries.
func loadPlan(opened *bundle.Opened) (*plan.Plan, error) {
	return parsePlan(opened.Plan, opened.Path)
}

// snapshotBulk is apply's view of the part of a snapshot it does not extract: what
// the archive holds of each app's whole data, counted by the pass that verified it,
// and the one pass that writes it into the staging tree.
type snapshotBulk struct{ opened *bundle.Opened }

// HoldsTree reports whether the snapshot's copy of one instance is more than the
// extraction kept.
func (b snapshotBulk) HoldsTree(instance string) bool { return b.opened.Contents[instance].Tree }

// Measure is what staging one instance writes at the config root.
func (b snapshotBulk) Measure(instance string) (apply.Staged, error) {
	c, ok := b.opened.Contents[instance]
	if !ok {
		return apply.Staged{}, fmt.Errorf("%s: the snapshot holds no config for it", instance)
	}
	return apply.Staged{Files: c.Files, Bytes: c.Bytes}, nil
}

// Stage writes the whole of one instance's config out of the snapshot.
func (b snapshotBulk) Stage(instance, dst string, uid, gid int) (apply.Staged, error) {
	c, err := b.opened.Stage(instance, dst, uid, gid)
	if err != nil {
		return apply.Staged{}, err
	}
	return apply.Staged{Files: c.Files, Bytes: c.Bytes}, nil
}

// loadMigrateChart loads the one chart release this command reads and installs.
// It is loaded once and handed to the apply, so the release migrate checks
// against is the release it installs -- and on a host with no box, the release
// bootstrap and init installed.
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
	// Every config apply rewrites comes out of the snapshot, one directory per
	// planned app. An app the plan names and the snapshot does not carry is a
	// refusal preflight makes, naming the app.
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	// What the extraction left in the snapshot, for the one app whose config is a
	// library as well as its state: apply stages it once, at the config root.
	env.Bulk = snapshotBulk{opened: opened}
	env.Catalog = cat.Names()
	env.Image = chartImage(chart, values)
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
	// Rehearsal says this run locks the box and renames nothing, so the screen
	// says what a rehearsal leaves alone.
	Rehearsal bool
	// Details prints every rewrite instead of the grouped summary.
	Details bool
	// AllowDowngrades is --yolo: run an older app on a config a newer one wrote.
	AllowDowngrades bool
}

// askOptionsOf is this run's flags as askOptions. A nil flag is one this caller
// never parsed, and reads as absent.
func askOptionsOf(o *migrateApplyFlags, rehearsal bool) askOptions {
	return askOptions{
		Rehearsal:       rehearsal,
		Details:         o.details != nil && *o.details,
		AllowDowngrades: o.yolo != nil && *o.yolo,
	}
}

// migrateAnswer is the confirm a run uses. On a host that had no box the question
// was asked before anything was installed, so this run asks nothing and holds
// itself to the answer instead. On a host that already had one there is nothing
// to agree to beforehand, and the question is asked here, inside the run, after
// every app's Check -- Plex's included.
func migrateAnswer(out say.Reporter, o *migrateApplyFlags, namespace string, rehearsal bool, agreed *apply.Summary, screen confirmScreen) func(apply.Summary) (bool, error) {
	if agreed != nil {
		return migrateAgreed(out, *agreed)
	}
	return migrateConfirm(out, confirmer(out, *o.yes), namespace, askOptionsOf(o, rehearsal), nil, "", screen)
}

// confirmScreenOf is the heading and report path of one run: what it is about to
// apply, and where its page goes. target is the seedbox's name, or "" before one
// is chosen -- which reads as "this box" rather than as an empty column.
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

// migrateConfirm shows the summary and asks the one question. It is the same
// confirmer `box apply` asks with, so a terminal gets the live form and a
// --plain or piped run gets the line prompt; --yes answers without asking, and
// no terminal without --yes refuses, as apply does.
//
// It is the run's only question. On a host with no box it comes before anything
// is installed, so the platform this run would install is part of it, shown
// first, and the first line says so: a person deciding to install a cluster and
// a platform layer has to read that from the screen, not from the order of the
// output. A run that asked "install the platform layer?" and then "move these
// into ...?" was two questions about one decision, and the second one came after
// both.
//
// The screen is the heading, the blocks, and one footer line counting what folded
// away; the page with every row in it is written beside the snapshot before the
// question, so a person who says no still has what they were shown.
func migrateConfirm(out say.Reporter, ask asker, namespace string, opts askOptions, plat *platform.Plan, lead string, screen confirmScreen) func(apply.Summary) (bool, error) {
	target := confirmTarget(namespace)
	return func(s apply.Summary) (bool, error) {
		// The one refusal the plan itself can make, before anything is shown:
		// a release older than the version that wrote the config is not a
		// migration anyone can trust, and the row says which app and both
		// versions.
		if err := refuseDowngrades(out, s, opts.AllowDowngrades, screen); err != nil {
			return false, err
		}
		if err := showSummary(out, s, opts.Details, plat, lead, screen); err != nil {
			return false, err
		}
		if opts.Rehearsal {
			// The screen a person reads before the first write says what this
			// box is, not only what moves into it.
			out.List("this rehearsal", []string{
				"nothing leaves this box: no internet egress, and the release's own pods only",
				"every torrent starts stopped, and no data is renamed",
			})
			return ask.Ask(fmt.Sprintf("rehearse these %s into %s?", appsWord(len(s.Instances)), target))
		}
		if plat != nil {
			return ask.Ask(fmt.Sprintf("install, then move these %s in?", appsWord(len(s.Instances))))
		}
		return ask.Ask(fmt.Sprintf("move these %s in?", appsWord(len(s.Instances))))
	}
}

// refuseDowngrades stops a run whose release runs an older app than the version
// that wrote the source's config, and names every such app with both versions.
//
// The config and the database belong to the version that wrote them: an older app
// reads a newer schema at its peril, and a migration is the one moment the choice
// is still open. --yolo is the operator's word that they want it anyway, and then
// the confirm carries one `!` row per app instead of a refusal.
//
// The rows are the refusal: what the run found, the command that does it anyway,
// and the one line saying what that costs. Reported says box has already put it on
// screen, so the command exits non-zero without saying it twice.
func refuseDowngrades(out say.Reporter, s apply.Summary, allow bool, screen confirmScreen) error {
	if allow {
		return nil
	}
	downs := s.Downgrades()
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
	if screen.Snapshot != "" {
		command += " " + screen.Snapshot
	}
	command += " --yolo"
	commandLines(out, command)
	out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{"nothing has moved"}}})
	return say.Reported{Err: errDowngrade}
}

// errDowngrade says the release is older than the source, and the refusal has
// already been printed with the command that runs it anyway.
var errDowngrade = errors.New("the release runs an older app than wrote this config")

// showSummary is the screen above the question: the heading, the platform this
// run would install, the blocks, the footer, and the page beside the snapshot.
func showSummary(out say.Reporter, s apply.Summary, details bool, plat *platform.Plan, lead string, screen confirmScreen) error {
	if screen.Head != "" {
		out.Line(screen.Head)
		out.Line("")
	}
	if plat != nil {
		if lead != "" {
			out.Muted(lead)
		}
		// The folded plan bootstrap prints, not the detailed one: the confirm
		// is a decision, and the components it installs are one line of it.
		if items := plat.Summary(); len(items) > 0 {
			out.Plan("platform", items)
		}
		out.Line("")
	}
	// The page is written whatever shape the screen takes: --details prints the
	// list, and the report is the file an operator keeps.
	if err := writeApplyReport(out, s, screen); err != nil {
		return err
	}
	if details {
		// Every rewrite, column by column: the list the report holds, for the
		// operator who wants to check one row before saying yes.
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

// writeApplyReport puts this run's page beside the snapshot, before the question:
// the file the footer and --details point at, written 0600 like everything else
// next to a snapshot.
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

// confirmLead is the first line of the one question on a host with no box: what
// the answer buys, before any of it exists. The cluster is named only when box
// installs it -- a host that already has one gets the platform layer and the box,
// and promising a cluster there would be a promise about something already there.
func confirmLead(installsCluster bool) string {
	if installsCluster {
		return "nothing is installed yet: this run installs the cluster and the platform layer, writes the box, and then moves the snapshot into it"
	}
	return "nothing is installed yet: this run installs the platform layer, writes the box, and then moves the snapshot into it"
}

// confirmTarget is the box a question names: the namespace it will have, or "this
// box" when the install has not been told one yet -- a guided run asks for the
// login, and the login is what names it.
func confirmTarget(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	return "this box"
}

// migrateAgreed is the answer a host with no box already gave: the confirm that
// does not ask, and refuses a summary that is not the one the operator agreed to.
//
// Both runs read the same snapshot and the same plan, so they have to produce
// the same summary. The one thing that differs is the rows the pre-install pass
// could not check, and apply.Summary.Holds ignores exactly those. Anything else
// is a fault, and a run that would do what nobody was shown stops instead.
func migrateAgreed(out say.Reporter, agreed apply.Summary) func(apply.Summary) (bool, error) {
	return func(s apply.Summary) (bool, error) {
		if why := s.Holds(agreed); why != "" {
			return false, fmt.Errorf("this run would not do what it asked about, and nothing of the snapshot has been moved: %s. The snapshot was read twice from the same file and the plan is the same, so this is a fault in box, not something to answer differently", why)
		}
		// The rows the question could not fill in are filled in here: the check
		// it said would happen once the app's image ran has just run, and what
		// it found belongs where the operator read the promise.
		if rows := s.CheckedRows(agreed); len(rows) > 0 {
			out.Table("checked after the install", rows)
		}
		return true, nil
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

func (g *grantEditor) Enable(instance, downloadCategory string) error {
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
				Value: switchOn(defaults, suffix, app, downloadCategory),
			})
		default:
			if index, _ := findInstance(own, suffix); index >= 0 {
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d].enabled", app, index),
					Value: true,
				})
				// The category the torrents are in, when it is not the
				// directory the library is renamed onto.
				if downloadCategory != "" {
					edits = append(edits, seedbox.Edit{
						Path:  fmt.Sprintf("apps.%s.instances[%d].downloadCategory", app, index),
						Value: downloadCategory,
					})
				}
			} else {
				// An instance the grant's list does not carry: box init adds
				// one by appending, and the entry has the keys the schema
				// requires.
				entry := map[string]any{"suffix": suffix, "category": categoryFor(app, suffix), "enabled": true}
				if downloadCategory != "" {
					entry["downloadCategory"] = downloadCategory
				}
				edits = append(edits, seedbox.Edit{
					Path:  fmt.Sprintf("apps.%s.instances[%d]", app, len(own)),
					Value: entry,
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
func switchOn(list []any, suffix, app, downloadCategory string) []any {
	out := make([]any, 0, len(list)+1)
	found := false
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		got, _ := entry["suffix"].(string)
		if got == suffix {
			found = true
			entry = cloneEntry(entry)
			entry["enabled"] = true
			if downloadCategory != "" {
				// The category the instance's torrents are already in. It is
				// not the directory the library is renamed onto, so the two
				// keys disagree on a migrated box and agree on a fresh one.
				entry["downloadCategory"] = downloadCategory
			}
		}
		out = append(out, entry)
	}
	if !found {
		entry := map[string]any{"suffix": suffix, "category": categoryFor(app, suffix), "enabled": true}
		if downloadCategory != "" {
			entry["downloadCategory"] = downloadCategory
		}
		out = append(out, entry)
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

// The grant's two storage keys, as `box init` writes them
// (internal/seedbox/storage.go): the host path the apps' settings live on, and
// the tree downloads and media are in.
const (
	volumeData   = "data"
	volumeConfig = "config"
)

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
