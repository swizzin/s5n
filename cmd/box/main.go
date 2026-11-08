// Command box is the host CLI: swizzin's installer and operator UI.
//
// `box bootstrap` decides which cluster to use, installs the platform layer
// the decision says is missing, and then runs init for root. `box init` asks a
// seedbox's questions, edits its two files key by key and hands over to `box
// apply`, whose diff and confirm are the review. `box apply` installs or
// upgrades one seedbox from those files, using the Helm SDK. There is no `box
// adduser` yet: init writes root's own seedbox, at the grant path it is given.
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
	"github.com/swizzin/s5n/internal/platform"
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
  bootstrap   decide which cluster to use, install the platform layer, then run init for root
  init        ask a seedbox's questions, edit its two files, then apply
  apply       install or upgrade root's seedbox from its grant and values
`)
	os.Exit(2)
}

func runBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "print the decision and reasons; probe nothing remote, install nothing")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	recordPath := fs.String("record", bootstrap.RecordPath, "where box records the cluster decision")
	yes := fs.Bool("yes", false, "confirm the platform and run init without asking")
	plain := fs.Bool("plain", false, "ask init's questions with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	gatewayClass := fs.String("gateway-class", "", "the GatewayClass to use when several are Accepted (platform.gateway.className)")
	initFlags := addInitFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}

	var records bootstrap.RecordStore = bootstrap.FilesystemRecords{Path: *recordPath}
	if *dryRun {
		records = bootstrap.ReadOnlyRecords{Inner: records}
	}

	// Choosing between several implementations is a question, and only a
	// terminal can answer it: --yes, no terminal, or a dry run leaves the
	// class unset, and the decision then refuses by naming the setting.
	var chooser bootstrap.ClassChooser
	if !*dryRun && !*yes && isTerminal(os.Stdin) {
		chooser = classChooser{prompt: newPrompt(*plain)}
	}

	d := &bootstrap.Decision{
		Source: bootstrap.FilesystemSource{},
		NewProber: func(kc []byte) (bootstrap.Prober, error) {
			return bootstrap.NewHTTPProber(kc, *dryRun)
		},
		Host:    bootstrap.LinuxHost{},
		Records: records,
		Install: bootstrap.PrintInstaller{W: os.Stdout},
		Prompt:  chooser,
	}

	res, err := d.Bootstrap(context.Background(), bootstrap.Options{
		Kubeconfig:    *kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
		GatewayClass:  *gatewayClass,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bootstrap.FormatResult(res))
	switch res.Action {
	case bootstrap.ActionStop:
		os.Exit(1)
	case bootstrap.ActionInstall:
		// The cluster installer is a later step: there is nothing to apply
		// to and nothing to init on a host box has not provisioned.
		return
	}
	if *dryRun {
		return
	}

	ctx := context.Background()
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, *yes); err != nil {
		if errors.Is(err, platform.ErrCancelled) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
	// bootstrap sets the machine up once; init sets up root's seedbox. It runs
	// here in the same process, over the same cluster, with the flags bootstrap
	// was given -- and stays runnable on its own (`box init`).
	if err := initFlags.run(ctx, res.Kubeconfig, *yes, *plain); err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
}

// applyPlatform applies the platform layer the decision says install. The
// confirmation is box's existing one: --yes, or no terminal to ask on, is
// unattended (docs/box.md, "bootstrap and init").
func applyPlatform(ctx context.Context, kubeconfig string, components []bootstrap.ComponentResult, gatewayClass string, yes bool) error {
	p := &platform.Decision{
		GatewayClass: gatewayClass,
		Open:         func() (platform.Cluster, error) { return platform.NewCluster(kubeconfig) },
		Releases:     platform.NewHelm(kubeconfig),
		// The LoadBalancer question is settled by the Gateway's own status, so
		// the probe is built only once the plan asks for it: after the confirm,
		// once the Gateway exists.
		Probe: func() (platform.GatewayProbe, error) { return platform.NewGatewayProbe(kubeconfig) },
		Confirm: &release.Terminal{
			In:    os.Stdin,
			Out:   os.Stdout,
			Yes:   yes || !isTerminal(os.Stdin),
			IsTTY: func() bool { return release.TTY(os.Stdin) },
		},
		Log: os.Stdout,
	}
	_, err := p.Apply(ctx, components)
	return err
}

// classChooser adapts bootstrap's class question onto setup.Prompter, so
// choosing an implementation is the same dialog init asks with. The classes
// are listed by name with their controller as the help line.
type classChooser struct {
	prompt setup.Prompter
}

func (c classChooser) ChooseGatewayClass(classes []bootstrap.GatewayClass) (string, error) {
	options := make([]setup.Option, 0, len(classes))
	for _, class := range classes {
		options = append(options, setup.Option{
			Value: class.Name,
			Label: class.Name,
			Help:  class.ControllerName,
		})
	}
	return c.prompt.Select(setup.Select{
		Key:     "platform.gateway.className",
		Prompt:  "Which GatewayClass should serve this box?",
		Options: options,
	})
}

// newPrompt builds the interactive prompt box uses on a terminal: huh when the
// terminal can carry a form, the line prompt otherwise. Unattended runs do not
// call it at all.
func newPrompt(plain bool) setup.Prompter {
	plan := tui.Decide(tui.Terminal{
		Stdin:   isTerminal(os.Stdin),
		Stdout:  isTerminal(os.Stdout),
		TERM:    os.Getenv("TERM"),
		NoColor: os.Getenv("NO_COLOR") != "",
		Plain:   plain,
	})
	if plan.Mode == tui.ModeHuh {
		return tui.New(tui.Options{In: os.Stdin, Out: os.Stdout, NoColor: plan.NoColor, Width: terminalWidth(os.Stdout)})
	}
	line := &setup.LinePrompt{In: os.Stdin, Out: os.Stdout}
	// A secret question is masked on a terminal, and only there: without a
	// terminal there is nobody to hide it from, and --set-transient is the
	// unattended form anyway.
	line.Mask = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	return line
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
	initFlags := addInitFlags(fs)
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig for the apply this hands over to, or fail with no fallback")
	yes := fs.Bool("yes", false, "ask nothing: flags and file keys only")
	plain := fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	if err := initFlags.run(context.Background(), *kubeconfig, *yes, *plain); err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
}

// initFlags are init's questions as flags. `box bootstrap` accepts the same
// ones and hands them to init for root once the platform is ready, so the two
// commands cannot drift apart; `box init` on its own parses exactly these.
type initFlags struct {
	fs          *flag.FlagSet
	chartDir    *string
	grantPath   *string
	valuesPath  *string
	namespace   *string
	domain      *string
	dataPath    *string
	configPath  *string
	apps        *string
	puid        *int
	pgid        *int
	transient   *transientAnswers
	pathsInNode *bool
}

func addInitFlags(fs *flag.FlagSet) *initFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &initFlags{fs: fs}
	o.chartDir = fs.String("chart", "", "chart directory to install (pulling a published chart is a later step)")
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file to write")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file to edit")
	o.namespace = fs.String("namespace", "seedbox", "the box's namespace; a re-run keeps whatever the grant says")
	o.domain = fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com")
	o.dataPath = fs.String("data-path", "", "the host path downloads and media live on")
	o.configPath = fs.String("config-path", "", "the host path the apps keep their settings on")
	o.apps = fs.String("apps", "", "comma separated app names from the chart's catalog")
	o.puid = fs.Int("puid", defaultPUID(), "uid every file on the data path is owned by (default: $SUDO_UID, else the invoking user, else 1000)")
	o.pgid = fs.Int("pgid", defaultPGID(), "gid every file on the data path is owned by (default: $SUDO_GID, else the invoking user, else 1000)")
	o.transient = &transientAnswers{}
	fs.Var(o.transient, "set-transient", "an answer that lives in no file, as key=value (repeatable), e.g. secrets.plexClaimToken=claim-xxxx")
	o.pathsInNode = fs.Bool("paths-in-node", false, "the storage paths live inside a test node (kind), not on this host: skip verifying and creating them")
	return o
}

// run asks a seedbox's questions, writes its two files and hands over to apply.
// The kubeconfig is the one bootstrap decided on, when bootstrap calls this;
// `box init` alone passes its flag, and apply resolves the record when it is
// empty.
func (o *initFlags) run(ctx context.Context, kubeconfig string, yes, plain bool) error {
	if *o.valuesPath == "" {
		return errors.New("no values file: pass --values")
	}

	// The questions come from the chart's catalog, so the chart is read before
	// anything is asked: a missing or broken manifest is a refusal, not a
	// half-finished dialog.
	cat, err := catalog.Load(*o.chartDir)
	if err != nil {
		return err
	}

	// Which prompt: huh when the terminal can carry a form, the line prompt
	// otherwise, and no prompt at all when nobody is there to answer. The
	// decision is tui.Decide's so it can be tested without a terminal, and the
	// fallback is deliberately not huh's accessible mode: that one returns an
	// empty secret without a terminal, which would silently install a box with
	// an empty claim token.
	unattended := yes || !isTerminal(os.Stdin)
	var prompt setup.Prompter
	if unattended {
		prompt = &setup.LinePrompt{}
	} else {
		// newPrompt is the same decision bootstrap's class question uses, so
		// the two commands cannot drift into different UIs.
		prompt = newPrompt(plain)
	}

	set := map[string]bool{}
	o.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	d := &setup.Decision{
		Host:    setup.LinuxPathHost{},
		Prompt:  prompt,
		Catalog: cat,
		Log:     os.Stdout,
		Apply: func(ctx context.Context, req setup.ApplyOptions) error {
			// The cluster decision belongs to `box bootstrap`; apply reuses it.
			// It is resolved here, after init has written the files, so a host
			// without a cluster yet still gets its two files written.
			kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
			if err != nil {
				return err
			}
			app := &release.Applier{
				Charts: release.DirChart{},
				Confirm: &release.Terminal{
					In:    os.Stdin,
					Out:   os.Stdout,
					Yes:   yes,
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

	_, err = d.Run(ctx, setup.Options{
		ChartDir:     *o.chartDir,
		GrantPath:    *o.grantPath,
		ValuesPath:   *o.valuesPath,
		Namespace:    *o.namespace,
		Domain:       *o.domain,
		DataPath:     *o.dataPath,
		ConfigPath:   *o.configPath,
		Apps:         []string{*o.apps},
		PUID:         *o.puid,
		PGID:         *o.pgid,
		PUIDSet:      set["puid"],
		PGIDSet:      set["pgid"],
		SetTransient: *o.transient,
		Unattended:   unattended,
		PathsInNode:  *o.pathsInNode,
	})
	return err
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
