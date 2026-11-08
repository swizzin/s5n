// Command box is the host CLI: swizzin's installer and operator UI.
//
// `box bootstrap` decides which cluster to use and probes the platform layer.
// `box init` asks a seedbox's questions, edits its two files key by key and
// hands over to `box apply`, whose diff and confirm are the review. `box apply`
// installs or upgrades one seedbox from those files, using the Helm SDK. There
// is no `box adduser` yet: init writes root's own seedbox, at the grant path it
// is given.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/tui"
)

// isTerminal is the check a dialog needs. A character device is not enough:
// /dev/null is one, and a form drawn on it would wait for keys nobody can
// press.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// terminalWidth is how many columns box draws into. A terminal that cannot
// report its size gets zero, which the UI reads as the narrowest width init
// supports: wrapping text to a width nobody measured is always safe.
func terminalWidth(f *os.File) int {
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bootstrap":
		runBootstrap(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
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
  init        ask a seedbox's questions, edit its two files, then apply
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

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	chartDir := fs.String("chart", "", "chart directory to install (pulling a published chart is a later step)")
	grantPath := fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file to write")
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	valuesPath := fs.String("values", valuesDefault, "the user's values file to edit")
	namespace := fs.String("namespace", "seedbox", "the box's namespace; a re-run keeps whatever the grant says")
	domain := fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com")
	dataPath := fs.String("data-path", "", "the host path downloads and media live on")
	configPath := fs.String("config-path", "", "the host path the apps keep their settings on")
	apps := fs.String("apps", "", "comma separated app names from the chart's catalog")
	puid := fs.Int("puid", defaultPUID(), "uid every file on the data path is owned by (default: $SUDO_UID, else the invoking user, else 1000)")
	pgid := fs.Int("pgid", defaultPGID(), "gid every file on the data path is owned by (default: $SUDO_GID, else the invoking user, else 1000)")
	transient := transientAnswers{}
	fs.Var(&transient, "set-transient", "an answer that lives in no file, as key=value (repeatable), e.g. secrets.plexClaimToken=claim-xxxx")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig for the apply this hands over to, or fail with no fallback")
	pathsInNode := fs.Bool("paths-in-node", false, "the storage paths live inside a test node (kind), not on this host: skip verifying and creating them")
	yes := fs.Bool("yes", false, "ask nothing: flags and file keys only")
	plain := fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	if *valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}

	// The questions come from the chart's catalog, so the chart is read before
	// anything is asked: a missing or broken manifest is a refusal, not a
	// half-finished dialog.
	cat, err := catalog.Load(*chartDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}

	// Which prompt: huh when the terminal can carry a form, the line prompt
	// otherwise, and no prompt at all when nobody is there to answer. The
	// decision is tui.Decide's so it can be tested without a terminal, and the
	// fallback is deliberately not huh's accessible mode: that one returns an
	// empty secret without a terminal, which would silently install a box with
	// an empty claim token.
	unattended := *yes || !isTerminal(os.Stdin)
	var prompt setup.Prompter
	switch {
	case unattended:
		prompt = &setup.LinePrompt{}
	default:
		plan := tui.Decide(tui.Terminal{
			Stdin:   isTerminal(os.Stdin),
			Stdout:  isTerminal(os.Stdout),
			TERM:    os.Getenv("TERM"),
			NoColor: os.Getenv("NO_COLOR") != "",
			Plain:   *plain,
		})
		if plan.Mode == tui.ModeHuh {
			prompt = tui.New(tui.Options{In: os.Stdin, Out: os.Stdout, NoColor: plan.NoColor, Width: terminalWidth(os.Stdout)})
			break
		}
		line := &setup.LinePrompt{In: os.Stdin, Out: os.Stdout}
		// A secret question is masked on a terminal, and only there: without a
		// terminal there is nobody to hide it from, and --set-transient is the
		// unattended form anyway.
		line.Mask = func() (string, error) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(b), err
		}
		prompt = line
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	d := &setup.Decision{
		Host:    setup.LinuxPathHost{},
		Prompt:  prompt,
		Catalog: cat,
		Log:     os.Stdout,
		Apply: func(ctx context.Context, req setup.ApplyOptions) error {
			// The cluster decision belongs to `box bootstrap`; apply reuses it.
			// It is resolved here, after init has written the files, so a host
			// without a cluster yet still gets its two files written.
			kc, err := release.ResolveKubeconfig(*kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
			if err != nil {
				return err
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
			_, err = app.Apply(ctx, release.Options{
				ChartDir:      req.ChartDir,
				GrantPath:     req.GrantPath,
				ValuesPath:    req.ValuesPath,
				PathsInNode:   req.PathsInNode,
				AfterConfirm:  req.AfterConfirm,
				TransientKeys: req.TransientKeys,
			})
			return err
		},
	}

	_, err = d.Run(context.Background(), setup.Options{
		ChartDir:     *chartDir,
		GrantPath:    *grantPath,
		ValuesPath:   *valuesPath,
		Namespace:    *namespace,
		Domain:       *domain,
		DataPath:     *dataPath,
		ConfigPath:   *configPath,
		Apps:         []string{*apps},
		PUID:         *puid,
		PGID:         *pgid,
		PUIDSet:      set["puid"],
		PGIDSet:      set["pgid"],
		SetTransient: transient,
		Unattended:   unattended,
		PathsInNode:  *pathsInNode,
	})
	if err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
}

// transientAnswers is a repeatable --set-transient key=value, the unattended
// form of a question that has no file to live in.
type transientAnswers map[string]string

func (t transientAnswers) String() string {
	if len(t) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t))
	for k, v := range t {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (t transientAnswers) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("want key=value, e.g. secrets.plexClaimToken=claim-xxxx")
	}
	t[strings.TrimSpace(key)] = value
	return nil
}

// defaultPUID and defaultPGID are what init offers when the flag was not given.
// sudo keeps the invoking user's ids in the environment, which is what a
// seedbox's files should belong to; root's own ids are never a good answer (the
// apps run unprivileged), so a root shell falls back to 1000, the first ordinary
// uid in the images.
func defaultPUID() int { return defaultID("SUDO_UID", os.Geteuid) }

func defaultPGID() int { return defaultID("SUDO_GID", os.Getegid) }

func defaultID(env string, own func() int) int {
	if s := os.Getenv(env); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	if n := own(); n > 0 {
		return n
	}
	return 1000
}
