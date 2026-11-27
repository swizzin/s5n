// Command box is the host CLI: bootstrap decides the cluster and installs the
// platform layer, then runs init; init asks a seedbox's questions, writes its
// two files and hands over to apply, whose diff and confirm are the review;
// apply installs or upgrades the seedbox through the Helm SDK. Init writes
// root's own seedbox: there is no `box adduser` yet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/login"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/tui"
)

// isTerminal is the check a dialog needs: /dev/null is a character device, and
// a form on it waits for keys nobody can press.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// terminalWidth is the drawing width; zero means the narrowest, which is safe.
func terminalWidth(f *os.File) int {
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

// version is the release box was built from (-X main.version); "dev" cannot be
// compared with min-box, so box says it skips that check.
var version = "dev"

// packaged names the package manager (-X main.packaged); a packaged box never
// replaces its own binary, because the manager owns it and would undo that.
var packaged string

func main() {
	// A signal cancels the step in flight, instead of killing box between a
	// volume and its claim.
	ctx, stop := signalContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer closeLog()
	if len(os.Args) < 2 {
		usage()
	}
	// --version and -v mean the same as `box version`, not a usage screen.
	if versionFlag(os.Args[1]) {
		fmt.Println(version)
		return
	}
	atStart()
	switch os.Args[1] {
	case "bootstrap":
		runBootstrap(ctx, os.Args[2:])
	case "init":
		runInit(ctx, os.Args[2:])
	case "apply":
		runApply(ctx, os.Args[2:])
	case "update":
		runUpdate(ctx, os.Args[2:])
	case "status":
		runStatus(ctx, os.Args[2:])
	case "rollback":
		runRollback(ctx, os.Args[2:])
	case "migrate":
		runMigrate(ctx, os.Args[2:])
	default:
		usage()
	}
}

// versionFlag reports whether this invocation asks for the version. All three
// spellings print the same line, so a script cannot get usage out of the one it
// guessed.
func versionFlag(arg string) bool {
	switch arg {
	case "version", "--version", "-v":
		return true
	}
	return false
}

// closeLog finishes the transcript; a failing command already wrote every line.
func closeLog() {
	if runLog != nil {
		_ = runLog.Close()
	}
}

// signalContext cancels on the first signal and stops handling it, so a second
// Ctrl-C terminates box, as it should when the first looked stuck.
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
  update      replace box when a newer one verifies, then move the seedbox to a newer release
  status      one line: what is running, what the last update did, when box checks again
  rollback    put one app back on the image it ran before an update moved it
  migrate     adopt an install box did not make: discover writes the plan, apply runs it
  version     print the version box was built from
`)
	os.Exit(2)
}

// chartSource reads a published release; box never loads charts/ from its own
// tree (docs/box.md, "box only uses published charts").
func chartSource(r say.Reporter) release.ChartSource {
	return &release.OCIChart{BoxVersion: version, Say: r}
}

// terminalSet is read once, so a run cannot be half live and half plain.
func terminalSet(plain bool) tui.Terminal {
	return tui.Terminal{
		Stdin:   isTerminal(os.Stdin),
		Stdout:  isTerminal(os.Stdout),
		TERM:    os.Getenv("TERM"),
		NoColor: os.Getenv("NO_COLOR") != "",
		Plain:   plain,
	}
}

// newOutput picks the renderer: a form-capable terminal gets the live reporter;
// --plain, TERM=dumb or a piped stdout gets the plain transcript.
func newOutput(t tui.Terminal, yes bool, palette string) say.Reporter {
	startLog()
	plan := tui.Decide(t)
	if plan.Mode == tui.ModeHuh {
		r := tui.NewReporter(tui.ReporterOptions{
			Width:   terminalWidth(os.Stdout),
			NoColor: plan.NoColor,
			Yes:     yes,
			Theme:   palette,
			Log:     runLog,
			LogPath: logPath(runLog),
		})
		return r
	}
	// A flow that asks builds its own question: --yes is its policy, not this
	// reporter's.
	return &say.Plain{W: os.Stdout, Err: os.Stderr, Log: runLog, LogPath: logPath(runLog)}
}

// live returns the TUI reporter when the command has one.
func live(r say.Reporter) (*tui.Reporter, bool) {
	tr, ok := r.(*tui.Reporter)
	return tr, ok
}

// asker is the one question both confirmers ask; the flow chooses def.
type asker interface {
	Ask(question string, def bool) (bool, error)
}

// confirmer asks with the live reporter or the plain line prompt. yes is the
// flow's unattended policy. The plain prompt gets a reader only from a real
// terminal: /dev/null is a character device, and an EOF nobody typed would fail.
func confirmer(out say.Reporter, yes bool) asker {
	if tr, ok := live(out); ok {
		tr.SetYes(yes)
		return tr
	}
	p := &say.Plain{W: os.Stdout, Err: os.Stderr, Yes: yes, Log: runLog, LogPath: logPath(runLog)}
	if isTerminal(os.Stdin) {
		p.In = os.Stdin
	}
	return p
}

// answered is the confirm for an apply a flow already asked about: a second form
// would come after the first step had written.
type answered struct{}

func (answered) Ask(string, bool) (bool, error) { return true, nil }

// chartFlags are the two release flags every chart-loading command shares.
type chartFlags struct {
	repo    *string
	version *string
}

// --chart-version means install for bootstrap/init/update and apply for apply,
// so one flag set serves all of them and a second copy cannot drift.
const (
	// chartVersionInstalls is bootstrap's, init's and update's.
	chartVersionInstalls = "chart release to install (default: the newest released version this box can use)"
	// chartVersionApplies is apply's: the release is the box's to choose.
	chartVersionApplies = "chart release to apply (default: the one installed; the newest this box can use when nothing is installed)"
)

func addChartFlags(fs *flag.FlagSet, versionUsage string) *chartFlags {
	return &chartFlags{
		repo:    fs.String("chart-repo", release.DefaultChartRepo, "OCI reference of the chart repository"),
		version: fs.String("chart-version", "", versionUsage),
	}
}

// ref is the reference the chart source is asked for.
func (c *chartFlags) ref() release.ChartRef {
	return release.ChartRef{Repo: *c.repo, Version: *c.version}
}

// load reads the named release; init hands the same value to its apply.
func (c *chartFlags) load(ctx context.Context, src release.ChartSource) (release.Chart, error) {
	return src.Load(ctx, c.ref())
}

// chartRelease is the one release a box command works with, pulled at most
// once: bootstrap's k0s pin, the platform's chart versions, init's questions and
// apply's install must all be one version, never two.
type chartRelease struct {
	flags *chartFlags
	out   say.Reporter
	// source is the registry; nil means chartSource(out).
	source release.ChartSource
	once   sync.Once
	chart  release.Chart
	err    error
}

// load pulls once and remembers the error too.
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

// pins is that release's platform.yaml.
func (c *chartRelease) pins(ctx context.Context) (release.Versions, error) {
	chart, err := c.load(ctx)
	if err != nil {
		return release.Versions{}, err
	}
	return chart.Pins, nil
}

func runBootstrap(ctx context.Context, args []string) {
	// --acme-server is a dev switch: taking it out before parsing is what keeps
	// it off --help, which lists every registered flag.
	acmeServer, args, err := takeDevFlag(args, "acme-server")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --unit-dir writes the units and runs no systemctl; a test host sees what
	// bootstrap would install.
	unitDir, args, err := takeDevFlag(args, "unit-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// init's dev switches come out too: bootstrap runs init in this process, and
	// the DNS override lets a certificate test move a name from no record to a
	// public one.
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
	if err := parseNoArgs(fs, "bootstrap", args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}

	initFlags.takeDev(dev)
	// Hand init the record path, so root's config beside it is read and asked.
	initFlags.recordPath = *recordPath

	palette := startupTheme(*initFlags.valuesPath)
	term := terminalSet(*plain)
	out := newOutput(term, *yes, palette)
	defer out.Close()
	if err := hostFiles(out, *recordPath, *initFlags.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// Parse root's config before any probe: a bad key stops the run naming file,
	// line and key. Missing is the defaults; a dry run reports it too.
	if _, _, err := hostconf.Load(hostconf.Path(*recordPath)); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// One release for k0s, the platform and root's seedbox.
	charts := &chartRelease{flags: initFlags.chart, out: out}
	initFlags.charts = charts

	var records bootstrap.RecordStore = bootstrap.FilesystemRecords{Path: *recordPath}
	if *dryRun {
		records = bootstrap.ReadOnlyRecords{Inner: records}
	}

	// Choosing a GatewayClass needs a terminal; --yes or a dry run leaves it and
	// the decision names the setting.
	var chooser bootstrap.ClassChooser
	if !*dryRun && !*yes && isTerminal(os.Stdin) {
		chooser = classChooser{prompt: newPrompt(term, palette)}
	}

	// A dry run uses an installer that only says what it would do.
	var install bootstrap.Installer = bootstrap.K0sInstaller{Say: out}
	if *dryRun {
		install = bootstrap.DryRunInstaller{Say: out}
	}

	// The header comes before the first probe.
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
		// A fresh k0s host now has a cluster: fall through to platform and init.
	}
	if *dryRun {
		return
	}

	// The platform's versions are the same release's pins, already loaded.
	chart, err := charts.load(ctx)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// A guided fresh install asks once for the platform and seedbox together
	// (bootstrap_one.go), installing both in that order.
	one := guidedFresh(*yes, isTerminal(os.Stdin), res.Platform)
	wroteBoxConf := false
	if one {
		plan, err := platform.Preview(chart.Pins, res.Platform, res.Gateway.Class, acmeServer, res.Firewall)
		if err != nil {
			out.Error(err)
			os.Exit(1)
		}
		out.Phase("platform")
		platform.PrintPlan(out, plan)
		// Write root's config before init asks for its notice address; a cancel
		// takes it back below.
		if wroteBoxConf, err = hostconf.Ensure(hostconf.Path(*recordPath)); err != nil {
			out.Error(err)
			os.Exit(1)
		}
		oneQ := &oneQuestion{
			ask: confirmer(out, false),
			platform: func(ctx context.Context) error {
				return applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, acmeServer, chart.Pins, res.Firewall, answered{}, true, out)
			},
			out: out,
		}
		initFlags.beforeWrite = oneQ.beforeWrite
		// The release was in the agreed plan: the inner apply asks no second time.
		initFlags.confirmed = true
	} else {
		out.Phase("platform")
		if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, acmeServer, chart.Pins, res.Firewall, confirmer(out, *yes || !isTerminal(os.Stdin)), false, out); err != nil {
			if errors.Is(err, platform.ErrCancelled) {
				os.Exit(1)
			}
			out.Error(err)
			os.Exit(1)
		}
		// Write root's config before init, which asks its notice address; an
		// existing file is root's and never rewritten.
		if wroteBoxConf, err = hostconf.Ensure(hostconf.Path(*recordPath)); err != nil {
			out.Error(err)
			os.Exit(1)
		}
	}
	if wroteBoxConf {
		// A row for the file, its path in the log (rule 5).
		out.Rows([]say.Row{{Mark: say.MarkNote, Name: "config", State: "new", Facts: []string{"updates and notices"}}})
		out.Detail("wrote " + hostconf.Path(*recordPath))
	}

	// init runs here over the same cluster and flags, and stays runnable alone.
	if err := initFlags.run(ctx, res.Kubeconfig, *yes, *plain, out); err != nil {
		// A declined question stops before init writes, so take back the early
		// root's config.
		if one && errors.Is(err, setup.ErrCancelled) {
			dropBoxConf(hostconf.Path(*recordPath), wroteBoxConf)
		}
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}

	// The timer comes last, for the files this run installed; its misses and any
	// missing record are the final block.
	out.ToFinish(append(initFlags.followUps, scheduleUpdates(ctx, unitDir, *recordPath, *initFlags.grantPath, *initFlags.valuesPath, out)...))
}

// applyPlatform applies the platform layer. holdPlan means the caller showed the
// plan in its own confirm -- a migration asks once for platform and apps -- so
// this package stays quiet and the caller places the heading.
func applyPlatform(ctx context.Context, kubeconfig string, components []bootstrap.ComponentResult, gatewayClass, acmeServer string, pins release.Versions, firewall bootstrap.FirewallState, confirm platform.Confirmer, holdPlan bool, out say.Reporter) error {
	p := &platform.Decision{
		GatewayClass: gatewayClass,
		ACMEServer:   acmeServer,
		Pins:         pins,
		Open:         func() (platform.Cluster, error) { return platform.NewCluster(kubeconfig) },
		Releases:     platform.NewHelm(kubeconfig),
		// The probe is built only after the confirm, when the Gateway exists.
		Probe:   func() (platform.GatewayProbe, error) { return platform.NewGatewayProbe(kubeconfig) },
		Confirm: confirm,
		Say:     out,
		// The packet filter is bootstrap's host finding, not a cluster object.
		Firewall:       firewall,
		HostFirewall:   bootstrap.Firewalld{},
		CallerConfirms: holdPlan,
	}
	_, err := p.Apply(ctx, components)
	return err
}

// dropBoxConf removes root's config a cancelled guided install wrote early.
func dropBoxConf(path string, wrote bool) {
	if !wrote {
		return
	}
	// Best effort: the run is already stopping.
	_ = os.Remove(path)
}

// boxValues reads cluster-derived values. gatewayFrom follows the Gateway's own
// address to the Service holding it, so the namespace is a fact, never a guess
// at envoy-gateway-system.
func boxValues(kubeconfig string) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		peers, err := platform.GatewayFrom(ctx, kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("networkPolicy.gatewayFrom: %w", err)
		}
		return map[string]any{"networkPolicy": map[string]any{"gatewayFrom": peers}}, nil
	}
}

// classChooser asks bootstrap's class question with init's dialog.
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

// newPrompt is huh on a form-capable terminal, the line prompt otherwise.
func newPrompt(t tui.Terminal, palette string) setup.Prompter {
	plan := tui.Decide(t)
	if plan.Mode == tui.ModeHuh {
		return tui.New(tui.Options{In: os.Stdin, Out: os.Stdout, NoColor: plan.NoColor, Width: terminalWidth(os.Stdout), Theme: palette})
	}
	line := &setup.LinePrompt{In: os.Stdin, Out: os.Stdout}
	// Mask only on a terminal; --set-transient is the unattended form.
	line.Mask = func() (string, error) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(b), err
	}
	return line
}

// applyFlags are apply's flags, a value so the mapping is testable.
type applyFlags struct {
	chart      *chartFlags
	grantPath  *string
	valuesPath *string
	kubeconfig *string
	// pathsInNode is the dev switch, taken out before parsing.
	pathsInNode bool
	yes         *bool
}

func addApplyFlags(fs *flag.FlagSet) *applyFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &applyFlags{chart: addChartFlags(fs, chartVersionApplies)}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file")
	o.kubeconfig = fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback")
	o.yes = fs.Bool("yes", false, "apply without asking")
	return o
}

// options is what this invocation hands the Applier; BoxValues is added by the
// caller once it has a kubeconfig.
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
	// --paths-in-node is the e2e's switch, out of --help. See takePathsInNode.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --dns-file overrides the resolver the per-host step checks names with; the
	// anchor follows the Certificate, not DNS.
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
	if err := parseNoArgs(fs, "apply", args); err != nil {
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
	if err := hostFiles(out, bootstrap.RecordPath, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// The cluster decision is bootstrap's; apply reuses it.
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}

	// Read the installed release before saying what is installed; the apply
	// below loads the files again and is the one place that refuses them.
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	namespace, err := sb.Namespace()
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	installed, err := release.NewHelm(kc, namespace, release.ReleaseName)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	opts, err := applyRelease(ctx, o.options(), installed.State)
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
		OpenSecret: func(namespace string) (release.SecretKeys, error) {
			return release.NewSecret(kc, namespace)
		},
		OpenStorage: func(namespace string) (release.StorageClient, error) {
			return release.NewStorage(kc, namespace)
		},
	}

	opts.BoxValues = boxValues(kc)
	// A cluster box did not install is proved to enforce NetworkPolicy once, here:
	// no app is installed behind a policy the CNI ignores.
	opts.NetworkProof = networkPolicyProof(kc, bootstrap.RecordPath, out)
	// Per-host TLS writes a listener before the release moves, so no apply
	// leaves an app host unreachable (docs/box.md).
	opts.Hosts = applyHosts{kubeconfig: kc, out: out, resolver: resolver}
	// Render the hold that keeps a rolled-back app on its image.
	holds := newBoxHolds(out)
	opts.Holds = holds.values
	// Copy a pin-moved app before the release, as a step does: `box rollback`
	// restores it, and apply has no automatic rollback.
	opts.Snapshots = applySnapshots(kc)
	// No host-CA step: this run writes no Certificate and must not read
	// cert-manager's namespace. init keeps the anchor right.
	res, err := applyThenSettle(ctx, app.Apply, opts, holds)
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
	if res.Host != "" {
		out.Banner("seedbox ready", res.Host)
	}
	showJellyfinPassword(out, res)
}

func runInit(ctx context.Context, args []string) {
	// init's dev switches are e2e-only and stripped before parsing.
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
	if err := parseNoArgs(fs, "init", args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	initFlags.takeDev(dev)
	initFlags.recordPath = bootstrap.RecordPath
	out := newOutput(terminalSet(*plain), *yes, startupTheme(*initFlags.valuesPath))
	defer out.Close()
	if err := initFlags.run(ctx, *kubeconfig, *yes, *plain, out); err != nil {
		if errors.Is(err, release.ErrCancelled) || errors.Is(err, setup.ErrCancelled) {
			os.Exit(1)
		}
		out.Error(err)
		os.Exit(1)
	}
	out.ToFinish(initFlags.followUps)
}

// initFlags are init's questions as flags; bootstrap accepts the same ones.
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
	// dnsFile is the dev resolver override; empty is the system resolver.
	dnsFile string
	// caFile is the dev host-CA source: an ACME certificate has no ca.crt, so
	// the e2e hands box the root it would otherwise read.
	caFile string
	// pathsInNode skips the host checks for a kind node's paths.
	pathsInNode bool
	// adopting is set by migrate apply, so init does not say "nothing runs yet"
	// about a box one step from running.
	adopting bool
	// switchesOn are the apps migrate turns on afterwards, by instance name.
	switchesOn []string
	// sourceTorrentPort is the port a migration read; zero means none.
	sourceTorrentPort int
	// recordPath is bootstrap's record; init has no --record flag, so each
	// caller sets it. Root's config sits beside it.
	recordPath string
	// followUps are what the run leaves a person, each with its command.
	followUps []say.Row
	// charts is bootstrap's already-loaded release, so init asks for no second.
	charts *chartRelease
	// beforeWrite asks about platform and seedbox together; nil asks nothing.
	beforeWrite func(ctx context.Context, a setup.Answers) error
	// confirmed means the flow already asked: the apply takes that answer.
	confirmed bool
}

func addInitFlags(fs *flag.FlagSet) *initFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &initFlags{fs: fs, chart: addChartFlags(fs, chartVersionInstalls)}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file to write")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file to edit")
	o.namespace = fs.String("namespace", "", "this box's namespace (default s5n-<login>); a re-run keeps whatever the grant says")
	o.domain = fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com")
	o.dataPath = fs.String("data-path", "", "the host path downloads and media live on")
	o.configPath = fs.String("config-path", "", "the host path the apps keep their settings on")
	o.apps = fs.String("apps", "", "comma separated app names from the chart's catalog")
	o.loginName = fs.String("login", "", "the account the login page admits (default: $SUDO_USER, else the invoking user)")
	o.password = fs.String("password-file", "", "read the login's password from this file, or from stdin with - (default: ask, or generate one)")
	o.puid = fs.Int("puid", defaultPUID(), "uid every file on the data path is owned by (default: the owner of a data path that already holds files, else $SUDO_UID, else the invoking user, else 1000)")
	o.pgid = fs.Int("pgid", defaultPGID(), "gid every file on the data path is owned by (default: the group of a data path that already holds files, else $SUDO_GID, else the invoking group, else 1000)")
	o.transient = &transientAnswers{}
	fs.Var(o.transient, "set-transient", "an answer that lives in no file, as key=value (repeatable), e.g. secrets.plexClaimToken=claim-xxxx")
	return o
}

// takeDev applies the switches already taken out of argv; none is registered,
// so --help cannot offer a test-node switch.
func (o *initFlags) takeDev(d initDevFlags) {
	o.dnsFile = d.dnsFile
	o.pathsInNode = d.pathsInNode
	o.caFile = d.caFile
}

// run asks the questions, writes the files and hands over to apply. The
// kubeconfig is bootstrap's decision; apply resolves the record when empty.
func (o *initFlags) run(ctx context.Context, kubeconfig string, yes, plain bool, out say.Reporter) error {
	if *o.valuesPath == "" {
		return errors.New("no values file: pass --values")
	}
	// Check box's old file locations before the chart or the first question.
	if err := hostFiles(out, o.recordPath, *o.grantPath); err != nil {
		return err
	}

	// Read the chart before asking: its catalog is the questions and its values
	// the defaults. The same value goes to the apply, so one release is asked
	// about and installed.
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

	// huh on a form-capable terminal, the line prompt otherwise. Never huh's
	// accessible mode: it returns an empty secret without a terminal, which
	// would install a box with an empty claim token.
	unattended := yes || !isTerminal(os.Stdin)
	prompt := newPrompt(terminalSet(plain), startupTheme(*o.valuesPath))

	set := map[string]bool{}
	o.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var resolver setup.Resolver = setup.NetResolver{}
	if o.dnsFile != "" {
		resolver = setup.FileResolver{Path: o.dnsFile}
	}
	// applied carries the one Jellyfin password when this run made it.
	var applied release.Result
	d := &setup.Decision{
		Host:     setup.LinuxPathHost{},
		Prompt:   prompt,
		Catalog:  cat,
		Say:      out,
		Resolver: resolver,
		Login:    loginStep(kubeconfig, o.pathsInNode, out, chart),
		// The login defaults to the account that owns the files; this reads the
		// account database and creates nothing.
		UserLookup: osUserName,
		HasLogin:   hasLogin(o.pathsInNode),
		GatewayIP:  gatewayProxyIP(kubeconfig),
		PodCIDRs:   podCIDRs(kubeconfig),
		NodePorts:  nodePorts(kubeconfig),
		Schema:     chart.Schema,
		Apply: func(ctx context.Context, req setup.ApplyOptions) error {
			// Resolve bootstrap's cluster here, after the files are written, so a
			// host without one still gets them.
			kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
			if err != nil {
				return err
			}
			confirm := initApplyConfirmer(o.adopting || o.confirmed, out, yes)
			app := &release.Applier{
				Charts:  source,
				Confirm: confirm,
				Say:     out,
				Open: func(namespace string) (release.ReleaseClient, error) {
					return release.NewHelm(kc, namespace, release.ReleaseName)
				},
				// A re-run of init is a re-run of apply, Plex token included.
				OpenSecret: func(namespace string) (release.SecretKeys, error) {
					return release.NewSecret(kc, namespace)
				},
				OpenStorage: func(namespace string) (release.StorageClient, error) {
					return release.NewStorage(kc, namespace)
				},
			}
			holds := newBoxHolds(out)
			applied, err = applyThenSettle(ctx, app.Apply, release.Options{
				Chart:         &chart,
				GrantPath:     req.GrantPath,
				ValuesPath:    req.ValuesPath,
				PathsInNode:   req.PathsInNode,
				AfterConfirm:  afterConfirm(req),
				TransientKeys: req.TransientKeys,
				BoxValues:     boxValues(kc),
				// Without the hold, a re-run would move a rolled-back app onto
				// the image it avoided.
				Holds:  holds.values,
				Anchor: hostCA(kc, out, o.caFile),
				// The per-host step runs inside the apply: after the confirm and
				// before the release, because Helm's wait would otherwise hold an
				// app that cannot dial an issuer whose 443 listener does not
				// exist yet. installs: the login comes after the confirm.
				Hosts: applyHosts{kubeconfig: kc, out: out, resolver: resolver, installs: true},
				// Every name init decided: a serving self-signed host still has
				// work to re-issue.
				HostsAll: true,
				// The anchor follows this run's issuer; a first init has no
				// Certificate to read.
				AnchorIssuer: req.Issuer,
				// Each name's own issuer, not tls.issuer, which is empty when DNS
				// decided.
				HostIssuers: req.HostIssuers,
			}, holds)
			return portRangeHint(err)
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
		Adopting:     o.adopting,
		SwitchesOn:   o.switchesOn,
		TorrentPort:  o.sourceTorrentPort,
		PathsInNode:  o.pathsInNode,
		NoticePath:   hostconf.Path(o.recordPath),
		BeforeWrite:  o.beforeWrite,
	})
	if err != nil {
		return err
	}
	if res.Domain != "" {
		out.Banner("seedbox ready", res.Domain)
	}
	// The login is printed here and nowhere else, password only when box made it.
	if res.Login != "" {
		out.Line("login: " + res.Login + " at https://" + res.Domain + "/auth/")
	}
	if res.GeneratedPassword != "" {
		// The line is on screen; the log records that a password was shown.
		redact(res.GeneratedPassword)
		out.Line("password: " + res.GeneratedPassword + "  (shown once; box keeps only the hash)")
	}
	// The second credential, only on the run that made it.
	showJellyfinPassword(out, applied)
	// A name with no DNS record leaves the records to add and the command that
	// moves the certificates, in the run's final block.
	if row, ok := noDNSFollowUp(res.Hosts); ok {
		if warning, ok := setup.NoDNSWarning(res.Hosts, "init"); ok {
			out.Detail(warning)
		}
		o.followUps = append(o.followUps, row)
	}
	return nil
}

// showJellyfinPassword prints the password only on the run that made it; the log
// keeps the fact, not the value, because a terminal may be scrolled back.
func showJellyfinPassword(out say.Reporter, res release.Result) {
	if res.JellyfinPassword == "" {
		return
	}
	redact(res.JellyfinPassword)
	out.Line("jellyfin password: " + res.JellyfinPassword + "  (shown once; sign in to Jellyfin as " + res.JellyfinLogin + ", and change it there if you like)")
}

// noDNSFollowUp names the hosts without records and the command that moves them
// to Let's Encrypt; the why is the run log's (rule 2).
func noDNSFollowUp(hosts []setup.Host) (say.Row, bool) {
	var names []string
	for _, h := range hosts {
		if h.NoDNS && strings.TrimSpace(h.Host) != "" {
			names = append(names, h.Host)
		}
	}
	if len(names) == 0 {
		return say.Row{}, false
	}
	return say.Row{
		Mark:    say.MarkNeeds,
		Name:    "certificates",
		Facts:   []string{"add a DNS record for " + strings.Join(names, ", ")},
		Command: "box init",
	}, true
}

// initApplyConfirmer is init's own release question; when another flow runs init
// as one step it already asked once, and a second form would come too late.
func initApplyConfirmer(alreadyAnswered bool, out say.Reporter, yes bool) asker {
	if alreadyAnswered {
		return answered{}
	}
	return confirmer(out, yes)
}

// afterConfirm runs after apply's confirm and before its first write. The login
// goes first: a release behind a broken login would be served unauthenticated.
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

// gatewayProxyIP reads the Gateway's proxy address. The kubeconfig is resolved
// inside the closure: init may have none, and resolving early would fail before
// its files are written.
func gatewayProxyIP(kubeconfig string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
		if err != nil {
			return "", err
		}
		return platform.GatewayProxyIP(ctx, kc)
	}
}

// podCIDRs reads the pod network for the app trusting the forwarded address,
// resolved like the address step so a refusal opens no client.
func podCIDRs(kubeconfig string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
		if err != nil {
			return nil, err
		}
		return platform.PodCIDRs(ctx, kc)
	}
}

// loginStep installs root's login, resolving the cluster inside the hook so a
// refusal opens no client.
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

// loginOptions is init's answers and the chart's pinned login image: box writes
// the Deployment, so a bump is a chart release.
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
		OwnLogin:    l.OwnLogin,
		Namespace:   l.Namespace,
		OIDC:        l.OIDC,
	}
}

// hasLogin says whether the users database holds the account: an empty password
// keeps an existing row and makes one when absent. A kind node's file is
// invisible, so the e2e passes --password-file.
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

// osUserName names the account behind a uid for the login default; box creates
// no account, and a uid without one has no name.
func osUserName(uid int) (string, bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || strings.TrimSpace(u.Username) == "" {
		return "", false
	}
	return u.Username, true
}

// defaultLogin is the sudo or invoking user; a root shell has no answer, so init
// asks or refuses.
func defaultLogin() string {
	if user := strings.TrimSpace(os.Getenv("SUDO_USER")); user != "" && user != "root" {
		return user
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" && user != "root" {
		return user
	}
	return ""
}

// takeDevFlag removes one dev flag (either form) before parsing, so it stays
// off --help.
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

// pathsInNodeFlag is the kind e2e's switch: paths inside the node, unchecked.
const pathsInNodeFlag = "paths-in-node"

// takePathsInNode accepts every boolean spelling (one dash or two, with or
// without =true/false), last wins, so the e2e's bare switch cannot fail as
// undefined and a stray positional cannot swallow it.
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

// initDevFlags are init's dev switches, never registered so --help cannot offer
// a test-node switch. bootstrap takes these plus --acme-server; apply takes
// --dns-file and --paths-in-node.
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

// transientAnswers is a repeatable --set-transient key=value.
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

// defaultPUID and defaultPGID use sudo's invoking ids; root's are never good
// (the apps run unprivileged), so a root shell falls back to 1000.
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
