package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/update"
)

// rollbackFlags are `box rollback`'s flags: apply's, because a rollback ends in
// an apply, plus the two of its own. They are a value rather than locals in
// runRollback so the mapping from flags to options is testable without a
// cluster, the way apply's is.
type rollbackFlags struct {
	*applyFlags
	pin   *bool
	plain *bool
}

func addRollbackFlags(fs *flag.FlagSet) *rollbackFlags {
	o := &rollbackFlags{
		applyFlags: addApplyFlags(fs),
		pin:        fs.Bool("pin", false, "pin the image in your values instead of holding it, so it lasts until you remove the pin"),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
	}
	// apply's flag offers the release that is installed, which a rollback
	// cannot honour either: its apply names the installed release itself, from
	// the cluster. --help has to say so, because the flag is refused rather
	// than ignored.
	if f := fs.Lookup("chart-version"); f != nil {
		f.Usage = "not used: a rollback applies the release that is installed"
	}
	return o
}

// options is what this invocation hands the Rollback: apply's options, because
// the apply at the end of a rollback is apply's own, and the app the operator
// named.
func (o *rollbackFlags) options(app string) update.RollbackOptions {
	return update.RollbackOptions{App: app, Apply: o.applyFlags.options(), Pin: *o.pin}
}

// runRollback is `box rollback <app>`: it puts one app back on the image it ran
// before an update moved it, from its newest snapshot.
//
// It is the hand-run version of the step-back an update does by itself, for the
// fault no gate sees. The release is not moved: the app runs its old image
// under the chart that is installed.
func runRollback(ctx context.Context, args []string) {
	// The same dev switch apply takes, stripped before the FlagSet sees it so
	// --help cannot offer it: --paths-in-node skips the host-path check for
	// the kind e2e, which this apply runs too.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	o := addRollbackFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	// Go's flag package stops at the first argument that is not a flag, and the
	// app is one: `box rollback sonarr --pin` would otherwise read --pin as a
	// second app. So whatever follows the app is parsed as flags, and only the
	// app itself may be left over.
	app := ""
	if rest := fs.Args(); len(rest) > 0 {
		app = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "box: %v\n", err)
			os.Exit(2)
		}
	}
	if app == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "box: rollback takes one app: box rollback <app>")
		os.Exit(2)
	}
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	// A rollback applies the release that is installed, so naming one is a
	// question box cannot answer: moving the release is `box update`, and
	// going back a release is not something 0.5 does at all.
	if version := *o.chart.version; version != "" {
		fmt.Fprintf(os.Stderr, "box: rollback applies the release that is installed, so --chart-version %s has nothing to do: it puts one app back, not the release\n", version)
		os.Exit(2)
	}

	out := newOutput(terminalSet(*o.plain), *o.yes, startupTheme(*o.valuesPath))
	defer out.Close()
	if err := hostFiles(out, bootstrap.RecordPath, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// The cluster decision belongs to `box bootstrap`; a rollback reuses it the
	// same way apply does and never probes a cluster of its own.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// One chart source for the plan and the apply: the release the rollback
	// read from Helm is the one the apply installs back.
	charts := updateChartSource(out)
	applier := &release.Applier{
		Charts: charts,
		// The rollback asked once, before anything stopped. The apply at the
		// end is the same answer.
		Confirm: answered{},
		Say:     out,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return updateHelm(kc, namespace)
		},
		OpenSecret: func(namespace string) (release.SecretKeys, error) {
			return release.NewSecret(kc, namespace)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}
	opts := o.options(app)
	opts.Apply.BoxValues = boxValues(kc)
	opts.Apply.PathsInNode = pathsInNode
	// The hold this rollback writes is overlaid by the apply that follows, the
	// same way `box apply` and an update's step overlay one -- and a hold the
	// release has moved past is forgotten only after an apply that succeeded.
	holds := newBoxHolds(out)
	opts.Apply.Holds = holds.values

	rb := &update.Rollback{
		Charts: charts,
		Open: func(namespace string) (update.Release, error) {
			return updateHelm(kc, namespace)
		},
		OpenCluster: func(namespace string) (update.Cluster, error) {
			return update.NewWorkloads(kc, namespace, release.ReleaseName)
		},
		// The newest copy of each instance, under the grant's config root: the
		// snapshot the step that moved this app took.
		OpenSnapshots: func(configRoot string, values map[string]any) (update.Snapshots, error) {
			return &update.NewestSnapshots{ConfigRoot: configRoot, Values: values}, nil
		},
		Apply: func(ctx context.Context, opts release.Options) (release.Result, error) {
			return applyThenSettle(ctx, applier.Apply, opts, holds)
		},
		Holds:   update.FileHolds{},
		Confirm: confirmer(out, *o.yes),
		Say:     out,
	}
	if _, err := rb.Run(ctx, opts); err != nil {
		if errors.Is(err, release.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
}
