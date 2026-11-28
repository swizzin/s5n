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

// rollbackFlags are apply's flags plus rollback's two, a value for testing.
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
	// The chart flag is refused, not ignored, so --help says the installed release
	// is used.
	if f := fs.Lookup("chart-version"); f != nil {
		f.Usage = "not used: a rollback applies the release that is installed"
	}
	return o
}

// options is apply's options and the named app.
func (o *rollbackFlags) options(app string) update.RollbackOptions {
	return update.RollbackOptions{App: app, Apply: o.applyFlags.options(), Pin: *o.pin}
}

// runRollback puts one app back on its old image from its newest snapshot; the
// release is not moved.
func runRollback(ctx context.Context, args []string) {
	// --paths-in-node, as apply takes it, out of --help.
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
	// Go's parser stops at the app, so parse what follows as flags.
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
	// Naming a release cannot be honoured: moving one is `box update`.
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

	// Reuse bootstrap's cluster, as apply does.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// One chart source: the release read from Helm is the one applied back.
	charts := updateChartSource(out)
	applier := &release.Applier{
		Charts: charts,
		// The one question was asked before anything stopped.
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
	// The apply overlays the hold; one the release moved past is forgotten only
	// after a successful apply.
	holds := newBoxHolds(out)
	opts.Apply.Holds = holds.values
	// A pin is the stronger claim and renders with its digest here too.
	pins := newBoxPins(out, release.Registry{})
	opts.Apply.Pins = pins.values

	rb := &update.Rollback{
		Charts: charts,
		Open: func(namespace string) (update.Release, error) {
			return updateHelm(kc, namespace)
		},
		OpenCluster: func(namespace string) (update.Cluster, error) {
			return update.NewWorkloads(kc, namespace, release.ReleaseName)
		},
		// The newest copy under the grant's config root.
		OpenSnapshots: func(configRoot string, values map[string]any) (update.Snapshots, error) {
			return &update.NewestSnapshots{ConfigRoot: configRoot, Values: values}, nil
		},
		Apply: func(ctx context.Context, opts release.Options) (release.Result, error) {
			return applyThenSettle(ctx, applier.Apply, opts, holds, pins)
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
