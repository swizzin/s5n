package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/notice"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/selfupdate"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/update"
)

// updateFlags are apply's flags plus update's own; a value so the mapping is
// testable without a cluster.
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
		// The record is bootstrap's; update writes its state beside it. apply and
		// rollback write no state and take no such flag.
		record:      fs.String("record", bootstrap.RecordPath, "where box recorded the cluster decision; update keeps its own record beside it"),
		holdBox:     fs.Bool("hold-box", false, "take no new box even when a newer one is out: the chart update runs on this one"),
		rollbackBox: fs.Bool("rollback-box", false, "put back the box an update replaced, and do no chart work (root only)"),
	}
	// apply's --chart-version means "installed"; update's means "move to".
	if f := fs.Lookup("chart-version"); f != nil {
		f.Usage = chartVersionInstalls
	}
	return o
}

// statePath is update.json beside the cluster record, which is why --record
// exists: a non-default record must not fail the update on its own bookkeeping.
func (o *updateFlags) statePath() string { return update.FilePath(*o.record) }

// options is what the Updater runs every step's apply with.
func (o *updateFlags) options() update.Options {
	return update.Options{Apply: o.applyFlags.options(), Check: *o.check}
}

// stepOptions adds the cluster-derived values: claims, per-host TLS and holds.
func (o *updateFlags) stepOptions(kc string, out say.Reporter, resolver setup.Resolver, holds *boxHolds) release.Options {
	opts := o.applyFlags.options()
	opts.BoxValues = boxValues(kc)
	// Per-host TLS writes a listener before the pods move.
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: resolver}
	// Apply the same holds `box apply` does, so a step does not undo a rollback.
	opts.Holds = holds.values
	return opts
}

// runOptions keeps stepOptions: plain options would install default claims, no
// listener, and the image a rollback avoided.
func (o *updateFlags) runOptions(step release.Options) update.Options {
	return update.Options{Apply: step, Check: *o.check}
}

// selfUpdateOptions is the version, release page, package and hold.
func (o *updateFlags) selfUpdateOptions(baseURL string) selfupdate.Options {
	return selfupdate.Options{
		Version:  version,
		BaseURL:  baseURL,
		Packaged: packaged,
		Hold:     *o.holdBox,
	}
}

// updateHelm gives the handle the health gate's budget and hooks-only waiting:
// Helm v4's status watcher reads a crashloop as failed and waits the full
// thirty-five minutes, so the step's gate must judge first. `box apply` keeps
// Helm's own wait because nothing else gates it.
func updateHelm(kubeconfig, namespace string) (*release.Helm, error) {
	h, err := release.NewHelm(kubeconfig, namespace, release.ReleaseName)
	if err != nil {
		return nil, err
	}
	h.Timeout = update.DefaultHealthWait
	h.HooksOnly = true
	return h, nil
}

// foldedReceipt drops the apply's receipt: update folds it into one row, and the
// full list goes to the log.
type foldedReceipt struct{ say.Reporter }

func (foldedReceipt) Receipt(string, []say.ReceiptRow) {}

// updateChartSource is one OCIChart, so the settled release is each step's
// install, from one cache.
func updateChartSource(out say.Reporter) *release.OCIChart {
	return &release.OCIChart{BoxVersion: version, Say: out}
}

// runUpdate replaces box when a newer release verifies, then moves the chart
// through its upgrade-from floors.
func runUpdate(ctx context.Context, args []string) {
	// apply's dev switches, out of --help: --paths-in-node, --dns-file, and
	// --box-releases for the self-update e2e's own page.
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
	if err := parseNoArgs(fs, "update", args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	if *o.valuesPath == "" {
		// No values file is said before replacing box or opening the cluster.
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	if *o.timer && *o.check {
		// A timer run is the update; a check would never move a box.
		fmt.Fprintln(os.Stderr, "box: --timer runs the update the timer would, so it cannot be a --check")
		os.Exit(2)
	}

	yes, plain := o.unattended()
	out := newOutput(terminalSet(plain), yes, startupTheme(*o.valuesPath))
	defer out.Close()
	if err := hostFiles(out, *o.record, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// Every update reads root's config: it says whether to take a new box, and a
	// parse failure refuses by line rather than silently stopping updates.
	conf, _, err := hostconf.Load(hostconf.Path(*o.record))
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	if conf.HoldBox {
		// The flag and the file are one decision.
		*o.holdBox = true
	}
	if *o.timer {
		charts := func(ctx context.Context) error { return updateCharts(ctx, o, out, resolver, conf) }
		t := timerRun{
			Conf:  conf,
			Path:  hostconf.Path(*o.record),
			State: update.FileState{Path: o.statePath()},
			Out:   out,
			Update: func(ctx context.Context) error {
				return updateRun{
					Box:    newBoxUpdate(out, os.Args[1:], os.Environ(), o.selfUpdateOptions(boxReleases)),
					Charts: charts,
				}.run(ctx)
			},
			// Read health here, so it happens even if the chart half stopped.
			Health: func(ctx context.Context) { healthOnly(ctx, o, out, conf) },
		}
		// A timer never rolls back the box; --rollback-box is root's hand.
		if err := t.run(ctx); err != nil {
			out.Error(err)
			os.Exit(1)
		}
		return
	}

	// Replace box first, so the release settled on meets min-box. It re-executes
	// with these arguments, so the chart work is handed to the run.
	su := newBoxUpdate(out, os.Args[1:], os.Environ(), o.selfUpdateOptions(boxReleases))
	su.RollbackBox = *o.rollbackBox
	su.CheckOnly = *o.check
	run := updateRun{
		Box:    su,
		Charts: func(ctx context.Context) error { return updateCharts(ctx, o, out, resolver, conf) },
	}
	if err := run.run(ctx); err != nil {
		if errors.Is(err, release.ErrCancelled) {
			// A declined question is an answer, not a fault.
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
}

// updateCharts is the chart half: plan, steps, applies. It returns failures
// instead of exiting, because a timer's wake reads health after it whatever it
// returned.
func updateCharts(ctx context.Context, o *updateFlags, out say.Reporter, resolver setup.Resolver, conf hostconf.Config) error {
	yes, _ := o.unattended()

	// Reuse bootstrap's cluster, as apply does.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.record})
	if err != nil {
		return err
	}

	// The grant gives the notices the namespace and the box's name.
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return err
	}
	// The notice address is a secret: keep it out of the log.
	redact(conf.NoticeURL)
	poster := notice.Poster{URL: conf.NoticeURL}
	noticeOpts := notice.Options{Box: grantDomain(sb), Seedbox: namespace}
	post := func(n notice.Notice) {
		// A failed notice is one line: the update is the work.
		if err := poster.Post(ctx, n); err != nil {
			out.Warn(err.Error())
		}
	}

	// One chart source: the settled release is each step's install.
	charts := updateChartSource(out)
	// The applier is built per apply: the move runs no hooks (the gate judges
	// first), and the apply that keeps the state runs them.
	applierFor := func(skipHooks bool) *release.Applier {
		return &release.Applier{
			Charts: charts,
			// The update already asked; a second question would come after it
			// moved.
			Confirm: answered{},
			Say:     foldedReceipt{out},
			Open: func(namespace string) (release.ReleaseClient, error) {
				h, err := updateHelm(kc, namespace)
				if err != nil {
					return nil, err
				}
				h.SkipHooks = skipHooks
				return h, nil
			},
			OpenSecret: func(namespace string) (release.SecretKeys, error) {
				return release.NewSecret(kc, namespace)
			},
			OpenStorage: func(namespace string) (release.StorageClient, error) {
				return release.NewStorage(kc, namespace)
			},
		}
	}
	// Each step applies with these; the Updater swaps only the release.
	holds := newBoxHolds(out)
	stepOpts := o.stepOptions(kc, out, resolver, holds)

	// OpenSnapshots copies under the grant's config root before the move.
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
		Holds:   update.FileHolds{},
		Confirm: confirmer(out, yes),
		State:   update.FileState{Path: o.statePath()},
		Notify: func(ctx context.Context, n update.Notice) {
			post(noticeFrom(noticeOpts, n))
		},
		BoxVersion: version,
		Say:        out,
	}
	res, err := up.Run(ctx, o.runOptions(stepOpts))
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			return err
		}
		return reportUpdateFailure(out, res, portRangeHint(err))
	}
	return nil
}

// reportUpdateFailure puts a failure's reason on screen once, as drawn. Before a
// plan the chart gets the ✗ row; after it the step's row carries the reason
// (rule 2).
func reportUpdateFailure(out say.Reporter, res update.Result, err error) error {
	if res.Target == "" {
		out.Rows([]say.Row{{Mark: say.MarkFailed, Name: "chart", Facts: []string{err.Error()}}})
	} else {
		out.Note(err.Error())
	}
	if len(res.Steps) == 0 && len(res.Taken) == 0 {
		out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{"nothing on this box changed"}}})
		out.Detail("the files are the only place this run wanted to write, and it wrote none of them")
	}
	return say.Reported{Err: err}
}
