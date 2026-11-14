package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/update"
)

// updateFlags are `box update`'s flags: apply's, because an update ends in
// applies, plus the two of its own. They are a value rather than locals in
// runUpdate so the mapping from flags to options is testable without a
// cluster, the way apply's is.
type updateFlags struct {
	*applyFlags
	check *bool
	plain *bool
}

func addUpdateFlags(fs *flag.FlagSet) *updateFlags {
	return &updateFlags{
		applyFlags: addApplyFlags(fs),
		check:      fs.Bool("check", false, "show what the box would update to, and write nothing"),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
	}
}

// options is what this invocation hands the Updater: every step runs apply
// with these, with the step's chart version in place of the pin.
func (o *updateFlags) options() update.Options {
	return update.Options{Apply: o.applyFlags.options(), Check: *o.check}
}

// updateHelm opens one release handle for an update. Its wait gets the health
// gate's budget: Helm gives up on a pod reporting CrashLoopBackOff at once, and
// a migration longer than Helm's own fifteen-minute default would fail the
// apply before the gate could wait it out. `box apply` keeps that default.
func updateHelm(kubeconfig, namespace string) (*release.Helm, error) {
	h, err := release.NewHelm(kubeconfig, namespace, release.ReleaseName)
	if err != nil {
		return nil, err
	}
	h.Timeout = update.DefaultHealthWait
	return h, nil
}

// updateChartSource is the one chart source an update works with. It is not
// chartSource's return type widened: this command needs the repository listing
// as well as a load, and one OCIChart for the command is what makes the release
// the plan settles on the one each step installs, out of the same cache.
func updateChartSource(out say.Reporter) *release.OCIChart {
	return &release.OCIChart{BoxVersion: version, Say: out}
}

// runUpdate is `box update`: it moves the installed release to a newer one,
// stepping through the releases the chart's upgrade-from floors require.
func runUpdate(ctx context.Context, args []string) {
	// The same two dev switches apply takes, stripped before the FlagSet sees
	// them so --help cannot offer them: --paths-in-node skips the host-path
	// check for the kind e2e, and --dns-file is the resolver the per-host step
	// checks an app host's name against.
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
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}

	out := newOutput(terminalSet(*o.plain), *o.yes, startupTheme(*o.valuesPath))
	defer out.Close()

	// The cluster decision belongs to `box bootstrap`; update reuses it the
	// same way apply does and never probes a cluster of its own.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// One chart source for the plan and every step: the release the plan
	// settles on is the one each apply installs, pulled once.
	charts := updateChartSource(out)
	app := &release.Applier{
		Charts: charts,
		// The step's apply is the update the operator said yes to. The update
		// asked once, before any step wrote; a second question partway through
		// a multi-step path would be asking after the box had already moved.
		Confirm: answered{},
		Say:     out,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return updateHelm(kc, namespace)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}
	applyOpts := o.options().Apply
	applyOpts.BoxValues = boxValues(kc)
	// Root's per-host TLS is apply's, so a release whose new image serves an
	// app at a host of its own gets that host's listener before the pods move.
	applyOpts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: resolver}
	// A step installs the release the plan settled on, and an app a rollback
	// holds stays on the image it ran: the holds file is overlaid by the same
	// apply a `box apply` runs, so a step does not undo a rollback.
	holds := newBoxHolds(out)
	applyOpts.Holds = holds.values

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
					ch, err := charts.Load(ctx, release.ChartRef{Repo: applyOpts.ChartRepo, Version: version})
					if err != nil {
						return nil, err
					}
					return update.ChartGlobs(ch)
				},
			}, nil
		},
		Apply: func(ctx context.Context, opts release.Options) (release.Result, error) {
			return applyThenSettle(ctx, app.Apply, opts, holds)
		},
		Holds:      update.FileHolds{},
		Confirm:    confirmer(out, *o.yes),
		State:      update.FileState{Path: update.FilePath(bootstrap.RecordPath)},
		BoxVersion: version,
		Say:        out,
	}
	res, err := up.Run(ctx, o.options())
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
