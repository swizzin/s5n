package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/selfupdate"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/update"
)

// updateFlags are `box update`'s flags: apply's, because an update ends in
// applies, plus the few of their own and the record. They are a value rather
// than locals in runUpdate so the mapping from flags to options is testable
// without a cluster, the way apply's is.
type updateFlags struct {
	*applyFlags
	check       *bool
	plain       *bool
	timer       *bool
	record      *string
	holdBox     *bool
	rollbackBox *bool
}

func addUpdateFlags(fs *flag.FlagSet) *updateFlags {
	o := &updateFlags{
		applyFlags: addApplyFlags(fs),
		check:      fs.Bool("check", false, "show what the box would update to, and write nothing"),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
		timer:      fs.Bool("timer", false, "the run the update timer makes: check when root's config says one is due, and post notices"),
		// The record is bootstrap's -- the cluster decision update reads and
		// writes its own state beside -- so it is named the same way and
		// defaults to the same path. apply and rollback take no such flag:
		// neither writes state, and both get their kubeconfig from a flag or
		// the environment.
		record:      fs.String("record", bootstrap.RecordPath, "where box recorded the cluster decision; update keeps its own record beside it"),
		holdBox:     fs.Bool("hold-box", false, "take no new box even when a newer one is out: the chart update runs on this one"),
		rollbackBox: fs.Bool("rollback-box", false, "put back the box an update replaced, and do no chart work (root only)"),
	}
	// --chart-version comes from apply's flag set, where it now means "the
	// release that is installed". An update's names the release to move to,
	// so its help is put back to the install wording.
	if f := fs.Lookup("chart-version"); f != nil {
		f.Usage = chartVersionInstalls
	}
	return o
}

// statePath is this run's own record: update.json beside the cluster record,
// which is the rule update.FilePath states and the reason --record exists. A
// box whose record is not at bootstrap's default path would otherwise fail the
// whole update on its own bookkeeping, after the plan and before any step.
func (o *updateFlags) statePath() string { return update.FilePath(*o.record) }

// options is what this invocation hands the Updater: every step runs apply
// with these, with the step's chart version in place of the pin.
func (o *updateFlags) options() update.Options {
	return update.Options{Apply: o.applyFlags.options(), Check: *o.check}
}

// stepOptions is what each step's apply runs with: apply's options plus the
// three values box derives from the cluster -- the claim names, the per-host
// TLS, and the holds a rollback wrote. The Updater puts each step's release in
// place of the pin and keeps the rest.
func (o *updateFlags) stepOptions(kc string, out say.Reporter, resolver setup.Resolver, holds *boxHolds) release.Options {
	opts := o.applyFlags.options()
	opts.BoxValues = boxValues(kc)
	// Root's per-host TLS is apply's, so a release whose new image serves an app
	// at a host of its own gets that host's listener before the pods move.
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: resolver}
	// An app a rollback holds stays on the image it ran: the holds file is
	// overlaid by the same apply `box apply` runs, so a step does not undo a
	// rollback.
	opts.Holds = holds.values
	return opts
}

// runOptions is what `box update` hands the Updater: the options its steps run
// with, and the plan's own --check. Handing it options() instead would drop the
// claim names, the host work and the hold, and a step would install a release
// this box is not: the chart's default claims, a host with no listener, and the
// image the rollback avoided.
func (o *updateFlags) runOptions(step release.Options) update.Options {
	return update.Options{Apply: step, Check: *o.check}
}

// selfUpdateOptions is what box's own replacement needs from this command: the
// version it is running, the release page (empty means the published one; a
// development e2e serves its own), the package that owns the binary if one does,
// and the hold.
func (o *updateFlags) selfUpdateOptions(baseURL string) selfupdate.Options {
	return selfupdate.Options{
		Version:  version,
		BaseURL:  baseURL,
		Packaged: packaged,
		Hold:     *o.holdBox,
	}
}

// updateHelm opens one release handle for an update. Its wait gets the health
// gate's budget, and it waits only for the release's hooks: the step's health
// gate is box's, and it is the one that can tell a moved app that keeps exiting
// from one that is still starting.
//
// Helm's own resource wait cannot make that call. Helm v4's status watcher reads
// a crashlooping pod as failed and drops failed resources from the wait, so the
// release's StatefulSet keeps the wait going for the whole budget: a step whose
// new image exits after three seconds would sit for thirty-five minutes before
// the gate it is meant to roll back could look. The wiring Job is still waited
// for, which is the gate's other half, and `box apply` keeps Helm's own wait
// because nothing else gates it.
func updateHelm(kubeconfig, namespace string) (*release.Helm, error) {
	h, err := release.NewHelm(kubeconfig, namespace, release.ReleaseName)
	if err != nil {
		return nil, err
	}
	h.Timeout = update.DefaultHealthWait
	h.HooksOnly = true
	return h, nil
}

// updateChartSource is the one chart source an update works with. It is not
// chartSource's return type widened: this command needs the repository listing
// as well as a load, and one OCIChart for the command is what makes the release
// the plan settles on the one each step installs, out of the same cache.
func updateChartSource(out say.Reporter) *release.OCIChart {
	return &release.OCIChart{BoxVersion: version, Say: out}
}

// runUpdate is `box update`: it replaces box when a newer release verifies, then
// moves the installed release to a newer one, stepping through the releases the
// chart's upgrade-from floors require.
func runUpdate(ctx context.Context, args []string) {
	// The same dev switches apply takes, stripped before the FlagSet sees
	// them so --help cannot offer them: --paths-in-node skips the host-path
	// check for the kind e2e, --dns-file is the resolver the per-host step
	// checks an app host's name against, and --box-releases is the release page
	// the self-update e2e serves its own boxes from.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	dnsFile, args, err := takeDevFlag(args, "dns-file")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	boxReleases, args, err := takeDevFlag(args, "box-releases")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	var resolver setup.Resolver = setup.NetResolver{}
	if dnsFile != "" {
		resolver = setup.FileResolver{Path: dnsFile}
	}

	fs := flag.NewFlagSet("update", flag.ExitOnError)
	o := addUpdateFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	if *o.timer && *o.check {
		// A timer run is the unattended update itself; a check writes nothing
		// and would leave the timer never moving a box.
		fmt.Fprintln(os.Stderr, "box: --timer runs the update the timer would, so it cannot be a --check")
		os.Exit(2)
	}

	yes, plain := o.unattended()
	out := newOutput(terminalSet(plain), yes, startupTheme(*o.valuesPath))
	defer out.Close()

	// Root's config is read by every update, not only the timer's: it says
	// whether box takes a new box, and a file that does not parse is a refusal
	// naming the line -- a setting silently ignored is a box that quietly
	// stopped updating.
	conf, _, err := hostconf.Load(hostconf.Path(*o.record))
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	if conf.HoldBox {
		// The same hold as the flag: one decision in one place, so the timer
		// and a hand run agree about the box's own version.
		*o.holdBox = true
	}
	if *o.timer {
		t := timerRun{
			Conf:  conf,
			Path:  hostconf.Path(*o.record),
			State: update.FileState{Path: o.statePath()},
			Out:   out,
			Update: func(ctx context.Context) error {
				return updateRun{
					Box:    newBoxUpdate(out, os.Args[1:], os.Environ(), o.selfUpdateOptions(boxReleases)),
					Charts: func(ctx context.Context) { updateCharts(ctx, o, out, resolver) },
				}.run(ctx)
			},
		}
		// The self-update's own --rollback-box is not a timer decision: a timer
		// never puts the previous box back, and the flag is root's hand.
		if err := t.run(ctx); err != nil {
			out.Error(err)
			os.Exit(1)
		}
		return
	}

	// box's own replacement comes before any chart work, so the release the
	// update settles on is one the box that installs it meets min-box for. It
	// re-executes with this process's own arguments when it installs a newer
	// box, which is why the chart work below is handed to the run rather than
	// simply following it.
	su := newBoxUpdate(out, os.Args[1:], os.Environ(), o.selfUpdateOptions(boxReleases))
	su.RollbackBox = *o.rollbackBox
	su.CheckOnly = *o.check
	run := updateRun{
		Box:    su,
		Charts: func(ctx context.Context) { updateCharts(ctx, o, out, resolver) },
	}
	if err := run.run(ctx); err != nil {
		out.Error(err)
		os.Exit(1)
	}
}

// updateCharts is the chart half of `box update`: the release plan, the steps
// through the floors, and the applies under them. It is a function of its own
// because the box's replacement comes first and can end the run before any of
// this -- or hand the process over, so that this runs in the new binary.
func updateCharts(ctx context.Context, o *updateFlags, out say.Reporter, resolver setup.Resolver) {
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	yes, _ := o.unattended()

	// The cluster decision belongs to `box bootstrap`; update reuses it the
	// same way apply does and never probes a cluster of its own.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.record})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// One chart source for the plan and every step: the release the plan
	// settles on is the one each apply installs, pulled once.
	charts := updateChartSource(out)
	// The applier a step's apply runs through. It is built per apply rather
	// than once, because the release handle a step opens depends on the apply:
	// the one that moves the release runs no hooks (the step's gate judges the
	// moved apps first, and the chart's wiring Job cannot pass while one of
	// them is down), and the one that installs the state that stays runs them.
	applierFor := func(skipHooks bool) *release.Applier {
		return &release.Applier{
			Charts: charts,
			// The step's apply is the update the operator said yes to. The
			// update asked once, before any step wrote; a second question
			// partway through a multi-step path would be asking after the box
			// had already moved.
			Confirm: answered{},
			Say:     out,
			Open: func(namespace string) (release.ReleaseClient, error) {
				h, err := updateHelm(kc, namespace)
				if err != nil {
					return nil, err
				}
				h.SkipHooks = skipHooks
				return h, nil
			},
			OpenStorage: func(namespace string) (release.StorageClient, error) {
				return release.NewStorage(kc, namespace)
			},
		}
	}
	// The options every step applies with: apply's own, plus the three values
	// box derives from the cluster. The Updater replaces the release with each
	// step's, and keeps everything else.
	holds := newBoxHolds(out)
	stepOpts := o.stepOptions(kc, out, resolver, holds)

	// OpenSnapshots is the copy of each copying app's config, under the grant's
	// config root: a step copies it before the release moves so a crashloop or
	// a later `box rollback` has state to go back to.
	up := &update.Updater{
		Charts: charts,
		Open: func(namespace string) (update.Release, error) {
			return updateHelm(kc, namespace)
		},
		OpenCluster: func(namespace string) (update.Cluster, error) {
			return update.NewWorkloads(kc, namespace, release.ReleaseName)
		},
		OpenSnapshots: func(namespace string, values map[string]any) (update.Snapshotter, error) {
			sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
			if err != nil {
				return nil, err
			}
			root, err := update.ConfigRoot(sb)
			if err != nil {
				return nil, err
			}
			return &update.ConfigSnapshots{
				ConfigRoot: root,
				Values:     values,
				Globs: func(ctx context.Context, version string) (map[string][]string, error) {
					ch, err := charts.Load(ctx, release.ChartRef{Repo: stepOpts.ChartRepo, Version: version})
					if err != nil {
						return nil, err
					}
					return update.ChartGlobs(ch)
				},
			}, nil
		},
		Apply: func(ctx context.Context, opts release.Options) (release.Result, error) {
			return applyThenSettle(ctx, applierFor(opts.SkipHooks).Apply, opts, holds)
		},
		Holds:      update.FileHolds{},
		Confirm:    confirmer(out, yes),
		State:      update.FileState{Path: o.statePath()},
		BoxVersion: version,
		Say:        out,
	}
	res, err := up.Run(ctx, o.runOptions(stepOpts))
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
	if !*o.check && !res.Current && len(res.Taken) > 0 {
		out.Banner("seedbox updated", fmt.Sprintf("%s is on %s", res.Name, res.Target))
	}
}
