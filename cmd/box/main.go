// Command box is the host CLI: swizzin's installer and operator UI.
//
// `box bootstrap` decides which cluster to use and probes the platform layer.
// `box apply` installs or upgrades root's own seedbox from its two files, using
// the Helm SDK, and reports the wiring receipt. Installing k0s, `box init`,
// `box adduser` and the rest of the multi-user path are later phases; nothing
// here touches the host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bootstrap":
		runBootstrap(os.Args[2:])
	case "apply":
		runApply(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: box <command> [flags]

commands:
  bootstrap   decide which cluster to use and probe the platform layer
  apply       install or upgrade root's seedbox from its grant and values
`)
	os.Exit(2)
}

func runBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "print the decision and reasons; probe nothing remote, install nothing")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}

	var records bootstrap.RecordStore = bootstrap.FilesystemRecords{Path: bootstrap.RecordPath}
	if *dryRun {
		records = bootstrap.ReadOnlyRecords{Inner: records}
	}

	d := &bootstrap.Decision{
		Source: bootstrap.FilesystemSource{},
		NewProber: func(kc []byte) (bootstrap.Prober, error) {
			return bootstrap.NewHTTPProber(kc, *dryRun)
		},
		Host:    bootstrap.LinuxHost{},
		Records: records,
		Install: bootstrap.PrintInstaller{W: os.Stdout},
	}

	res, err := d.Bootstrap(context.Background(), bootstrap.Options{
		Kubeconfig:    *kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bootstrap.FormatResult(res))
}

func runApply(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	chartDir := fs.String("chart", "", "chart directory to install (pulling a published chart is a later step)")
	grantPath := fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file")
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	valuesPath := fs.String("values", valuesDefault, "the user's values file")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	pathsInNode := fs.Bool("paths-in-node", false, "the grant's storage paths live inside a test node (kind), not on this host: skip the host directory check")
	yes := fs.Bool("yes", false, "apply without asking")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	if *valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}

	// The cluster decision belongs to `box bootstrap`; apply reuses it and
	// never probes or re-detects a cluster of its own.
	kc, err := release.ResolveKubeconfig(*kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}

	app := &release.Applier{
		Charts: release.DirChart{},
		Confirm: &release.Terminal{
			In:    os.Stdin,
			Out:   os.Stdout,
			Yes:   *yes,
			IsTTY: func() bool { return release.TTY(os.Stdin) },
		},
		Log: os.Stdout,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return release.NewHelm(kc, namespace, release.ReleaseName)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}

	_, err = app.Apply(context.Background(), release.Options{
		ChartDir:    *chartDir,
		GrantPath:   *grantPath,
		ValuesPath:  *valuesPath,
		PathsInNode: *pathsInNode,
	})
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
}
