// Command box is the host CLI: swizzin's installer and operator UI. This phase
// wires only `box bootstrap --dry-run`, which runs the real read-only cluster
// detection and platform probes and prints the decision. Nothing here installs
// k0s or touches the host.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bootstrap":
		runBootstrap(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: box <command> [flags]

commands:
  bootstrap   decide which cluster to use and probe the platform layer
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
