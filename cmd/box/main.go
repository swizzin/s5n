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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/login"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
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

// version is the release box was built from: the tag its binaries are
// published under (cmd/box's release workflow passes -X main.version). A build
// without it is "dev", which is not a version a chart's box.s5n/min-box can be
// compared against, so box says it is skipping that check rather than pretending
// to meet it.
var version = "dev"

func main() {
	// One context for the whole process. A signal cancels the step in flight
	// through the error path that step already has, instead of killing box
	// between a volume and its claim (which is how a first install ends up
	// half-written and a re-run refuses).
	ctx, stop := signalContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bootstrap":
		runBootstrap(ctx, os.Args[2:])
	case "init":
		runInit(ctx, os.Args[2:])
	case "apply":
		runApply(ctx, os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

// signalContext cancels its context on the first signal, and stops handling
// signals once it has. signal.NotifyContext leaves the handler registered
// after the first signal, so a second one is swallowed by a channel nobody
// reads -- exactly when the operator presses it again because box looks stuck.
// Unregistering on cancellation restores Go's default behaviour: the second
// Ctrl-C terminates the process.
func signalContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, signals...)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: box <command> [flags]

commands:
  bootstrap   decide which cluster to use, install the platform layer, then run init for root
  init        ask a seedbox's questions, edit its two files, then apply
  apply       install or upgrade root's seedbox from its grant and values
  version     print the version box was built from
`)
	os.Exit(2)
}

// chartSource is where box reads the chart: a published release. box never
// loads charts/ from its own tree (docs/box.md, "box only uses published
// charts"), so even the e2e packages the chart from its commit and pushes it
// to a registry, and box pulls it the way it pulls any release.
func chartSource(r say.Reporter) release.ChartSource {
	return &release.OCIChart{BoxVersion: version, Say: r}
}

// terminalSet is this process's own terminal, as tui.Decide reads it. Both the
// reporter and the prompt are chosen from the same value, so one run cannot end
// up half live and half plain.
func terminalSet(plain bool) tui.Terminal {
	return tui.Terminal{
		Stdin:   isTerminal(os.Stdin),
		Stdout:  isTerminal(os.Stdout),
		TERM:    os.Getenv("TERM"),
		NoColor: os.Getenv("NO_COLOR") != "",
		Plain:   plain,
	}
}

// newOutput picks the renderer for the whole command. The same tui.Decide that
// chooses the prompt chooses this: a terminal that can carry a form gets the
// live reporter, and --plain, TERM=dumb or a piped stdout gets the exact plain
// transcript. NO_COLOR keeps the live layout without colour.
func newOutput(t tui.Terminal, yes bool, palette string) say.Reporter {
	plan := tui.Decide(t)
	if plan.Mode == tui.ModeHuh {
		r := tui.NewReporter(tui.ReporterOptions{Width: terminalWidth(os.Stdout), NoColor: plan.NoColor, Yes: yes, Theme: palette})
		return r
	}
	// Output only: a flow that asks builds its own question through
	// confirmer, because --yes is that flow's policy (the platform layer
	// treats "no terminal" as unattended, apply refuses) and this reporter
	// has one.
	return &say.Plain{W: os.Stdout, Err: os.Stderr}
}

// live returns the TUI reporter when this command has one. A plain run answers
// through confirmer instead, and keeps the two unattended rules it had: the
// platform flow auto-confirms without a terminal, apply refuses.
func live(r say.Reporter) (*tui.Reporter, bool) {
	tr, ok := r.(*tui.Reporter)
	return tr, ok
}

// asker is the one question release.Confirmer and platform.Confirmer both ask.
type asker interface {
	Ask(question string) (bool, error)
}

// confirmer is what a flow asks its one question with: the live reporter, which
// draws the same form init's questions use, or the plain transcript's line
// prompt.
//
// yes is the caller's unattended policy, not cmd/box's: the platform layer
// treats "there is no terminal to ask on" as unattended, while apply refuses
// there, so each flow passes its own answer. The plain prompt gets a reader
// only from a real terminal -- a character device is not enough, because
// /dev/null is one and a question asked on it fails with an EOF nobody typed.
func confirmer(out say.Reporter, yes bool) asker {
	if tr, ok := live(out); ok {
		tr.SetYes(yes)
		return tr
	}
	p := &say.Plain{W: os.Stdout, Err: os.Stderr, Yes: yes}
	if isTerminal(os.Stdin) {
		p.In = os.Stdin
	}
	return p
}

// chartFlags are the two flags that name a published release. Every command
// that loads a chart takes them from here, so bootstrap, init and apply cannot
// drift into asking for different releases.
type chartFlags struct {
	repo    *string
	version *string
}

func addChartFlags(fs *flag.FlagSet) *chartFlags {
	return &chartFlags{
		repo:    fs.String("chart-repo", release.DefaultChartRepo, "OCI reference of the chart repository"),
		version: fs.String("chart-version", "", "chart release to install (default: the newest released version this box can use)"),
	}
}

// ref is the reference the chart source is asked for. It is one function on
// purpose: a field dropped here is invisible in every other test, which is how
// the init path once lost --chart-version.
func (c *chartFlags) ref() release.ChartRef {
	return release.ChartRef{Repo: *c.repo, Version: *c.version}
}

// load reads the release the flags name. init reads the chart before it asks
// anything, and the apply it hands over to installs that same value rather
// than pulling a second time and risking a different release.
func (c *chartFlags) load(ctx context.Context, src release.ChartSource) (release.Chart, error) {
	return src.Load(ctx, c.ref())
}

// chartRelease is the one chart release a box command works with, pulled at
// most once.
//
// bootstrap reads the platform pins it installs k0s at, the platform half reads
// the chart versions it installs, init reads the questions from it and apply
// installs it. All four are the same release by construction: a bootstrap that
// pulls its own copy and then hands over to an init that pulls the newest again
// could settle two different versions, and the platform it installed would be
// one nobody tested against the seedbox.
type chartRelease struct {
	flags *chartFlags
	out   say.Reporter
	// source is the registry. Nil means chartSource(out); a test gives the
	// one source every load of the command must go through.
	source release.ChartSource
	once   sync.Once
	chart  release.Chart
	err    error
}

// load is the release the flags name, pulled on the first call and remembered
// after it. The error is remembered too: a registry that refused once is not
// asked again in the same run.
func (c *chartRelease) load(ctx context.Context) (release.Chart, error) {
	c.once.Do(func() {
		source := c.source
		if source == nil {
			source = chartSource(c.out)
		}
		c.chart, c.err = c.flags.load(ctx, source)
	})
	return c.chart, c.err
}

// pins is that release's platform.yaml, the versions everything box installs
// comes from.
func (c *chartRelease) pins(ctx context.Context) (release.Versions, error) {
	chart, err := c.load(ctx)
	if err != nil {
		return release.Versions{}, err
	}
	return chart.Pins, nil
}

func runBootstrap(ctx context.Context, args []string) {
	// The ACME server override is a development and e2e switch, not operator
	// surface: flag.PrintDefaults lists every registered flag even with an
	// empty usage, so the only way to keep it off --help is to take it out of
	// the arguments before the FlagSet parses them. Its removal (and the
	// e2e's switch to a test issuer of its own) is a follow-up.
	acmeServer, args, err := takeDevFlag(args, "acme-server")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --dns-file, --ca-file and --paths-in-node are init's dev switches and come
	// out the same way: bootstrap runs init in this process, so it accepts them
	// or the e2e's bootstrap line would be the one place they fail. The DNS
	// override exists because an e2e cannot write the test host's /etc/hosts,
	// and moving a name from "no record" to "a public one" between two runs is
	// exactly what the certificate card has to prove.
	dev, args, err := takeInitDevFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "print the decision and reasons; probe no cluster, install nothing")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	recordPath := fs.String("record", bootstrap.RecordPath, "where box records the cluster decision")
	yes := fs.Bool("yes", false, "confirm the platform and run init without asking")
	plain := fs.Bool("plain", false, "ask init's questions with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	gatewayClass := fs.String("gateway-class", "", "the Envoy Gateway GatewayClass to use when several are Accepted (platform.gateway.className)")
	initFlags := addInitFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}

	initFlags.takeDev(dev)

	palette := startupTheme(*initFlags.valuesPath)
	term := terminalSet(*plain)
	out := newOutput(term, *yes, palette)
	defer out.Close()

	// One chart release for the whole command: the k0s bootstrap installs, the
	// platform layer it applies and root's seedbox init installs all come from
	// it.
	charts := &chartRelease{flags: initFlags.chart, out: out}
	initFlags.charts = charts

	var records bootstrap.RecordStore = bootstrap.FilesystemRecords{Path: *recordPath}
	if *dryRun {
		records = bootstrap.ReadOnlyRecords{Inner: records}
	}

	// Choosing between several implementations is a question, and only a
	// terminal can answer it: --yes, no terminal, or a dry run leaves the
	// class unset, and the decision then refuses by naming the setting.
	var chooser bootstrap.ClassChooser
	if !*dryRun && !*yes && isTerminal(os.Stdin) {
		chooser = classChooser{prompt: newPrompt(term, palette)}
	}

	// A dry run reaches the same clean-host branch as a real run, and the only
	// honest installer it can have is one that says what it would do: nothing
	// is probed or written, so there is no host to change.
	var install bootstrap.Installer = bootstrap.K0sInstaller{Say: out}
	if *dryRun {
		install = bootstrap.DryRunInstaller{Say: out}
	}

	// The section header comes before the first probe: on a clean host the
	// installer's own steps are the first thing a person sees.
	out.Phase("cluster")

	d := &bootstrap.Decision{
		Source: bootstrap.FilesystemSource{},
		Pins:   charts.pins,
		NewProber: func(kc []byte) (bootstrap.Prober, error) {
			return bootstrap.NewHTTPProber(kc, *dryRun)
		},
		Host:    bootstrap.LinuxHost{},
		Records: records,
		Install: install,
		Prompt:  chooser,
	}

	res, err := d.Bootstrap(ctx, bootstrap.Options{
		Kubeconfig:    *kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
		GatewayClass:  *gatewayClass,
		DryRun:        *dryRun,
	})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	bootstrap.ReportResult(res, out)
	switch res.Action {
	case bootstrap.ActionStop:
		os.Exit(1)
	case bootstrap.ActionInstall:
		// A clean host that box has just installed k0s on now has a cluster:
		// fall through to the same platform apply and init every other path
		// runs, over the kubeconfig k0s wrote.
	}
	if *dryRun {
		return
	}

	// The platform's chart versions are the same release's pins. It is already
	// loaded on the clean-host path, where bootstrap needed the k0s pin.
	chart, err := charts.load(ctx)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, acmeServer, chart.Pins, *yes, out); err != nil {
		if errors.Is(err, platform.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
	// bootstrap sets the machine up once; init sets up root's seedbox. It runs
	// here in the same process, over the same cluster, with the flags bootstrap
	// was given -- and stays runnable on its own (`box init`).
	if err := initFlags.run(ctx, res.Kubeconfig, *yes, *plain, out); err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
}

// applyPlatform applies the platform layer the decision says install. The
// confirmation is box's existing one: --yes, or no terminal to ask on, is
// unattended (docs/box.md, "bootstrap and init").
func applyPlatform(ctx context.Context, kubeconfig string, components []bootstrap.ComponentResult, gatewayClass, acmeServer string, pins release.Versions, yes bool, out say.Reporter) error {
	out.Phase("platform")
	confirm := confirmer(out, yes || !isTerminal(os.Stdin))
	p := &platform.Decision{
		GatewayClass: gatewayClass,
		ACMEServer:   acmeServer,
		Pins:         pins,
		Open:         func() (platform.Cluster, error) { return platform.NewCluster(kubeconfig) },
		Releases:     platform.NewHelm(kubeconfig),
		// The LoadBalancer question is settled by the Gateway's own status, so
		// the probe is built only once the plan asks for it: after the confirm,
		// once the Gateway exists.
		Probe:   func() (platform.GatewayProbe, error) { return platform.NewGatewayProbe(kubeconfig) },
		Confirm: confirm,
		Say:     out,
	}
	_, err := p.Apply(ctx, components)
	return err
}

// boxValues is what `box apply` reads from the cluster rather than from either
// file. The NetworkPolicy peer follows the Gateway implementation in use: box
// asks the Gateway's own address which Service holds it, and admits that
// Service's namespace. With Envoy Gateway, the only implementation box
// supports, that is envoy-gateway-system; asking the cluster keeps it a fact
// rather than a guess.
func boxValues(kubeconfig string) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		peers, err := platform.GatewayFrom(ctx, kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("networkPolicy.gatewayFrom: %w", err)
		}
		return map[string]any{"networkPolicy": map[string]any{"gatewayFrom": peers}}, nil
	}
}

// classChooser adapts bootstrap's class question onto setup.Prompter, so
// choosing between Accepted Envoy Gateway classes is the same dialog init asks
// with. The classes are listed by name with their controller as the help line.
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
func newPrompt(t tui.Terminal, palette string) setup.Prompter {
	plan := tui.Decide(t)
	if plan.Mode == tui.ModeHuh {
		return tui.New(tui.Options{In: os.Stdin, Out: os.Stdout, NoColor: plan.NoColor, Width: terminalWidth(os.Stdout), Theme: palette})
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

// applyFlags are `box apply`'s flags, including the chart reference. They are
// a value rather than locals in runApply so the mapping from flags to the
// options the release client receives is testable without a cluster.
type applyFlags struct {
	chart      *chartFlags
	grantPath  *string
	valuesPath *string
	kubeconfig *string
	// pathsInNode is the dev switch (--paths-in-node): not a registered
	// flag, so apply's --help cannot offer it, and set from
	// takePathsInNode before the FlagSet parses what is left.
	pathsInNode bool
	yes         *bool
}

func addApplyFlags(fs *flag.FlagSet) *applyFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &applyFlags{chart: addChartFlags(fs)}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file")
	o.kubeconfig = fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	o.yes = fs.Bool("yes", false, "apply without asking")
	return o
}

// options is what this invocation hands the Applier. The chart flags become
// the ChartRef the chart source is asked for; BoxValues is added by the caller
// once it has a kubeconfig.
func (o *applyFlags) options() release.Options {
	ref := o.chart.ref()
	return release.Options{
		ChartRepo:    ref.Repo,
		ChartVersion: ref.Version,
		GrantPath:    *o.grantPath,
		ValuesPath:   *o.valuesPath,
		PathsInNode:  o.pathsInNode,
	}
}

func runApply(ctx context.Context, args []string) {
	// --paths-in-node is the e2e's switch, not operator surface: it is taken
	// out of the arguments before the FlagSet sees them so --help cannot
	// list it. See takePathsInNode.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --dns-file is the resolver override for the per-host step: apply
	// DNS-checks an app host's name against the box's domain before it writes
	// that host's listener and Certificate, and the e2e's names answer only in
	// CoreDNS. The anchor takes no DNS answer at all -- it follows the
	// Certificate the host is served with.
	dnsFile, args, err := takeDevFlag(args, "dns-file")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	var resolver setup.Resolver = setup.NetResolver{}
	if dnsFile != "" {
		resolver = setup.FileResolver{Path: dnsFile}
	}
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	o := addApplyFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}

	out := newOutput(terminalSet(false), *o.yes, startupTheme(*o.valuesPath))
	defer out.Close()

	// The cluster decision belongs to `box bootstrap`; apply reuses it and
	// never probes or re-detects a cluster of its own.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	confirm := confirmer(out, *o.yes)
	app := &release.Applier{
		Charts:  chartSource(out),
		Confirm: confirm,
		Say:     out,
		Open: func(namespace string) (release.ReleaseClient, error) {
			return release.NewHelm(kc, namespace, release.ReleaseName)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}

	opts := o.options()
	opts.BoxValues = boxValues(kc)
	// Root's per-host TLS: a values file that enables an app served at a host
	// of its own gets that host's listener and Certificate before the release
	// moves, so no apply leaves the app unreachable (docs/box.md,
	// "No run leaves an app host without its listener").
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: resolver}
	// No host-CA step here: the anchor follows the Certificate the box's host
	// is served with, and this run neither writes that Certificate nor may read
	// cert-manager's namespace. `box init` is the run that keeps it right.
	res, err := app.Apply(ctx, opts)
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
	if res.Host != "" {
		out.Banner("seedbox ready", res.Host)
	}
}

func runInit(ctx context.Context, args []string) {
	// init's dev switches are stripped before the FlagSet parses: they are
	// e2e-only, and --help must not offer them. See takeInitDevFlags.
	dev, args, err := takeInitDevFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	initFlags := addInitFlags(fs)
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig for the apply this hands over to, or fail with no fallback")
	yes := fs.Bool("yes", false, "ask nothing: flags and file keys only")
	plain := fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	initFlags.takeDev(dev)
	out := newOutput(terminalSet(*plain), *yes, startupTheme(*initFlags.valuesPath))
	defer out.Close()
	if err := initFlags.run(ctx, *kubeconfig, *yes, *plain, out); err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
}

// initFlags are init's questions as flags. `box bootstrap` accepts the same
// ones and hands them to init for root once the platform is ready, so the two
// commands cannot drift apart; `box init` on its own parses exactly these.
type initFlags struct {
	fs         *flag.FlagSet
	chart      *chartFlags
	grantPath  *string
	valuesPath *string
	namespace  *string
	domain     *string
	dataPath   *string
	configPath *string
	apps       *string
	loginName  *string
	password   *string
	puid       *int
	pgid       *int
	transient  *transientAnswers
	// dnsFile is the development-only resolver override (--dns-file): a
	// hosts-style file init resolves through instead of the system resolver.
	// Empty means the real one.
	dnsFile string
	// caFile is the development-only host-CA source (--ca-file): a PEM file
	// the anchor ConfigMap is written from instead of the box root's Secret.
	// An ACME-issued certificate has no ca.crt to copy, so an e2e that proves
	// the anchor with Pebble has to hand box the root it would otherwise read.
	// Empty means the real source.
	caFile string
	// pathsInNode is the development-only storage switch (--paths-in-node):
	// the paths are inside a test node (kind), not on this host, so init
	// neither reports nor creates them and the apply it hands over to skips
	// its directory check. A field rather than a registered flag, so
	// --help cannot offer it. Set from takeInitDevFlags.
	pathsInNode bool
	// charts is the release to install, when the command already has one:
	// `box bootstrap` loads it for its k0s pin and hands the same value here,
	// so bootstrap and init cannot ask the registry for two versions.
	charts *chartRelease
}

func addInitFlags(fs *flag.FlagSet) *initFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &initFlags{fs: fs, chart: addChartFlags(fs)}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file to write")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file to edit")
	o.namespace = fs.String("namespace", "seedbox", "the box's namespace; a re-run keeps whatever the grant says")
	o.domain = fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com")
	o.dataPath = fs.String("data-path", "", "the host path downloads and media live on")
	o.configPath = fs.String("config-path", "", "the host path the apps keep their settings on")
	o.apps = fs.String("apps", "", "comma separated app names from the chart's catalog")
	o.loginName = fs.String("login", "", "the account the login page admits (default: $SUDO_USER, else the invoking user)")
	o.password = fs.String("password-file", "", "read the login's password from this file, or from stdin with - (default: ask, or generate one)")
	o.puid = fs.Int("puid", defaultPUID(), "uid every file on the data path is owned by (default: $SUDO_UID, else the invoking user, else 1000)")
	o.pgid = fs.Int("pgid", defaultPGID(), "gid every file on the data path is owned by (default: $SUDO_GID, else the invoking user, else 1000)")
	o.transient = &transientAnswers{}
	fs.Var(o.transient, "set-transient", "an answer that lives in no file, as key=value (repeatable), e.g. secrets.plexClaimToken=claim-xxxx")
	return o
}

// takeDev applies the development switches the command already took out of
// argv. None of them is registered with o.fs: flag.PrintDefaults lists every
// registered flag even with an empty usage, so --help would otherwise offer
// switches that only work against a test node.
func (o *initFlags) takeDev(d initDevFlags) {
	o.dnsFile = d.dnsFile
	o.pathsInNode = d.pathsInNode
	o.caFile = d.caFile
}

// run asks a seedbox's questions, writes its two files and hands over to apply.
// The kubeconfig is the one bootstrap decided on, when bootstrap calls this;
// `box init` alone passes its flag, and apply resolves the record when it is
// empty.
func (o *initFlags) run(ctx context.Context, kubeconfig string, yes, plain bool, out say.Reporter) error {
	if *o.valuesPath == "" {
		return errors.New("no values file: pass --values")
	}

	// The chart is read before anything is asked: its catalog is where the
	// questions come from, and its values are what every default is prefilled
	// from, so a registry that cannot be reached or a broken manifest is a
	// refusal, not a half-finished dialog. The same value is handed to the
	// apply at the end, so the release init asked about is the release box
	// installs.
	source := chartSource(out)
	charts := o.charts
	if charts == nil {
		charts = &chartRelease{flags: o.chart, out: out}
	}
	chart, err := charts.load(ctx)
	if err != nil {
		return err
	}
	cat, err := catalog.Parse(chart.Ref+"/catalog.yaml", chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return err
	}
	if chart.Version != "" {
		out.Chart(chart.Name, chart.Version, chart.Ref)
	}

	// Which prompt: huh when the terminal can carry a form, the line prompt
	// otherwise. Unattended asks nothing, but the path report still needs a
	// renderer: on a terminal it goes through the same TUI as the questions,
	// so a `--yes` run is not a fancy transcript with one raw block in it.
	// Falling back rather than using huh's accessible mode is deliberate: that
	// one returns an empty secret without a terminal, which would silently
	// install a box with an empty claim token.
	unattended := yes || !isTerminal(os.Stdin)
	prompt := newPrompt(terminalSet(plain), startupTheme(*o.valuesPath))

	set := map[string]bool{}
	o.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var resolver setup.Resolver = setup.NetResolver{}
	if o.dnsFile != "" {
		resolver = setup.FileResolver{Path: o.dnsFile}
	}
	d := &setup.Decision{
		Host:      setup.LinuxPathHost{},
		Prompt:    prompt,
		Catalog:   cat,
		Say:       out,
		Resolver:  resolver,
		Login:     loginStep(kubeconfig, o.pathsInNode, out, chart),
		HasLogin:  hasLogin(o.pathsInNode),
		GatewayIP: gatewayProxyIP(kubeconfig),
		Schema:    chart.Schema,
		Apply: func(ctx context.Context, req setup.ApplyOptions) error {
			// The cluster decision belongs to `box bootstrap`; apply reuses it.
			// It is resolved here, after init has written the files, so a host
			// without a cluster yet still gets its two files written.
			kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
			if err != nil {
				return err
			}
			confirm := confirmer(out, yes)
			app := &release.Applier{
				Charts:  source,
				Confirm: confirm,
				Say:     out,
				Open: func(namespace string) (release.ReleaseClient, error) {
					return release.NewHelm(kc, namespace, release.ReleaseName)
				},
				OpenStorage: func(namespace string) (release.StorageClient, error) {
					return release.NewStorage(kc, namespace)
				},
			}
			_, err = app.Apply(ctx, release.Options{
				Chart:         &chart,
				GrantPath:     req.GrantPath,
				ValuesPath:    req.ValuesPath,
				PathsInNode:   req.PathsInNode,
				AfterConfirm:  afterConfirm(req),
				TransientKeys: req.TransientKeys,
				BoxValues:     boxValues(kc),
				Anchor:        hostCA(kc, out, o.caFile),
				// The per-host step, inside the apply: init decided every
				// name, and this writes each one's listener and Certificate
				// after the confirm and before the release moves. Running it
				// afterwards is a deadlock the OIDC clients found: Helm's wait
				// holds the apply open for an app that cannot start until it
				// can dial the issuer, and the issuer is served on the 443
				// listener this step has not written yet.
				//
				// installs: init installs the login after the confirm
				// (req.Login, in afterConfirm below), so the plan may read a
				// cluster that has none yet and still have something to show.
				Hosts: applyHosts{kubeconfig: kc, out: out, resolver: resolver, installs: true},
				// Every name init decided, not only the ones nothing answers
				// on: re-issuing a self-signed host under the box root is work
				// an existing host still has.
				HostsAll: true,
				// The issuer this run's per-host step writes the box's own
				// Certificate with: the anchor follows it, and on a first init
				// there is no Certificate in the cluster to read instead.
				AnchorIssuer: req.Issuer,
				// Every name's own decision, so the plan compares each
				// Certificate that already answers with the issuer this run
				// will write for it -- not with tls.issuer, which is empty
				// when DNS decided, and not with a second resolution.
				HostIssuers: req.HostIssuers,
			})
			return err
		},
	}

	res, err := d.Run(ctx, setup.Options{
		Chart:        seedbox.Chart{Values: chart.Values, Schema: chart.Schema},
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
		Login:        *o.loginName,
		LoginSet:     set["login"],
		DefaultLogin: defaultLogin(),
		PasswordFile: *o.password,
		Unattended:   unattended,
		PathsInNode:  o.pathsInNode,
	})
	if err != nil {
		return err
	}
	if res.Domain != "" {
		out.Banner("seedbox ready", res.Domain)
	}
	// The login is the one answer a person has to write down: the account, and
	// a password only when box made it. It is printed here and nowhere else.
	if res.Login != "" {
		out.Line("login: " + res.Login + " at https://" + res.Domain + "/auth/")
	}
	if res.GeneratedPassword != "" {
		out.Line("password: " + res.GeneratedPassword + "  (shown once; box keeps only the hash)")
	}
	// The final card's one warning: a name with no DNS record still installs,
	// self-signed, and a re-run is what moves it. Named per host, because an
	// app's name is a record the operator may not have thought about.
	for _, h := range res.Hosts {
		if !h.NoDNS {
			continue
		}
		out.Warn(setup.NoDNSWarning(h, "init"))
	}
	return nil
}

// afterConfirm is the one hook apply runs after its confirm and before its
// first write. The login goes first: a release behind a login that is not
// answering would be served unauthenticated, and a login Envoy Gateway refuses
// has to stop the run before the apps exist.
func afterConfirm(req setup.ApplyOptions) func(context.Context) (map[string]any, error) {
	if req.Login == nil && req.AfterConfirm == nil {
		return nil
	}
	return func(ctx context.Context) (map[string]any, error) {
		if req.Login != nil {
			if err := req.Login(ctx); err != nil {
				return nil, err
			}
		}
		if req.AfterConfirm != nil {
			return req.AfterConfirm(ctx)
		}
		return nil, nil
	}
}

// gatewayProxyIP is the Gateway's proxy address for the grant, read from the
// cluster by root's run. The kubeconfig is resolved inside the closure because
// init may be handed none: bootstrap's record answers then, and resolving it
// here would make `box init` on a host with no cluster fail before its files are
// written.
func gatewayProxyIP(kubeconfig string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
		if err != nil {
			return "", err
		}
		return platform.GatewayProxyIP(ctx, kc)
	}
}

// loginStep installs root's login. It resolves the cluster the same way apply
// and the certificate step do, and it does so inside the hook: a run that is
// refused at the confirm never opens a client.
func loginStep(kubeconfig string, pathsInNode bool, out say.Reporter, chart release.Chart) setup.LoginFunc {
	return func(ctx context.Context, l setup.Login) error {
		kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
		if err != nil {
			return err
		}
		d := &login.Decision{
			Open:    func() (login.Cluster, error) { return platform.NewCluster(kc) },
			Storage: func() (release.StorageClient, error) { return release.NewStorage(kc, login.Namespace) },
			Host:    login.LinuxHost{},
			Say:     out,
		}
		_, err = d.Apply(ctx, loginOptions(chart, l, pathsInNode))
		return err
	}
}

// loginOptions is what installs root's login: the answers init asked for, and
// the image the chart release pins. The image is the release's, not a constant
// here, because box writes the login's Deployment itself -- so the pin is the
// whole reference, tag and digest, and a bump is a chart release rather than a
// new box.
func loginOptions(chart release.Chart, l setup.Login, pathsInNode bool) login.Options {
	return login.Options{
		Host:        l.Host,
		Login:       l.Name,
		Password:    l.Password,
		NewPassword: l.NewPassword,
		PathsInNode: pathsInNode,
		Image:       chart.Pins.Authelia,
		Values:      l.Values,
		Hosted:      l.Hosted,
		Namespace:   l.Namespace,
		OIDC:        l.OIDC,
	}
}

// hasLogin answers whether the users database already holds this account. It
// is init's first-run-or-re-run question: an empty password keeps an existing
// row and makes one when there is none. A file inside a test node is not
// visible from here, which is why the kind e2e always passes --password-file.
func hasLogin(pathsInNode bool) setup.HasLoginFunc {
	return func(name string) bool {
		if pathsInNode {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(authelia.HostPath, authelia.UsersDBFile))
		if err != nil {
			return false
		}
		db, err := authelia.ParseUsers(raw)
		if err != nil {
			return false
		}
		_, ok := db.Get(name)
		return ok
	}
}

// defaultLogin is what init offers when no --login was given: the account of
// the person who ran sudo, because that is whose box this is. A root shell has
// no such answer and init asks (or refuses, unattended).
func defaultLogin() string {
	if user := strings.TrimSpace(os.Getenv("SUDO_USER")); user != "" && user != "root" {
		return user
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" && user != "root" {
		return user
	}
	return ""
}

// takeDevFlag removes one development-only flag from args before a FlagSet
// parses them, in either --name=value or --name value form. It is how a
// switch that is not operator surface stays off --help: registering it would
// list it there even with an empty usage.
func takeDevFlag(args []string, name string) (value string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--"+name || arg == "-"+name:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--%s needs a value", name)
			}
			value = args[i+1]
			found = true
			i++
		case strings.HasPrefix(arg, "--"+name+"=") || strings.HasPrefix(arg, "-"+name+"="):
			value = arg[strings.Index(arg, "=")+1:]
			found = true
		default:
			rest = append(rest, arg)
		}
	}
	if found && value == "" {
		return "", nil, fmt.Errorf("--%s needs a value", name)
	}
	return value, rest, nil
}

// pathsInNodeFlag is the storage switch the kind e2e passes to all three
// commands: its paths are inside the node, so box skips the checks this host
// cannot answer.
const pathsInNodeFlag = "paths-in-node"

// takePathsInNode is takeDevFlag's boolean sibling and the only switch of that
// shape: a boolean carries its own answer, so the spelling alone means true and
// there is no value to be missing. Every form a FlagSet would parse is taken
// out of the arguments -- one dash or two, with `=true`/`=false` or without --
// because the e2e writes it bare and a caller that spells it any other way must
// not get "flag provided but not defined" for a switch box accepts. The last one
// wins, as it does for a registered flag. Taking it out also removes the reason
// Go's parser stops reading: a stray positional cannot swallow the dev switch
// that follows it, and box takes no positionals to want it.
func takePathsInNode(args []string) (set bool, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--"+pathsInNodeFlag || arg == "-"+pathsInNodeFlag:
			set = true
		case strings.HasPrefix(arg, "--"+pathsInNodeFlag+"=") || strings.HasPrefix(arg, "-"+pathsInNodeFlag+"="):
			value := arg[strings.Index(arg, "=")+1:]
			b, perr := strconv.ParseBool(value)
			if perr != nil {
				return false, nil, fmt.Errorf("--%s wants true or false, not %q", pathsInNodeFlag, value)
			}
			set = b
		default:
			rest = append(rest, arg)
		}
	}
	return set, rest, nil
}

// initDevFlags are init's development switches as the command line asked for
// them: --dns-file's value, --ca-file's value and --paths-in-node's presence.
// They are fields on initFlags, never registered with its FlagSet, because
// flag.PrintDefaults lists every registered flag even with an empty usage and
// the usage an operator reads must not offer a switch that only works against a
// test node. bootstrap takes these three plus --acme-server; apply takes
// --dns-file and --paths-in-node, because its per-host step resolves names too.
type initDevFlags struct {
	dnsFile     string
	pathsInNode bool
	caFile      string
}

func takeInitDevFlags(args []string) (initDevFlags, []string, error) {
	var d initDevFlags
	var err error
	if d.dnsFile, args, err = takeDevFlag(args, "dns-file"); err != nil {
		return initDevFlags{}, nil, err
	}
	if d.pathsInNode, args, err = takePathsInNode(args); err != nil {
		return initDevFlags{}, nil, err
	}
	if d.caFile, args, err = takeDevFlag(args, "ca-file"); err != nil {
		return initDevFlags{}, nil, err
	}
	return d, args, nil
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
