package main

// migrate_cutover.go is `box migrate cutover`: the window.
//
// It reads the source the way discover does, then proves every planned app
// stopped -- no replicas and no pod left, Terminating included -- and only then
// copies the configs into the final snapshot apply runs. While any app still
// runs it writes a survey first and refuses, printing the command that stops
// each one for the operator to double-check: box never stops a source.
//
// The source's cluster is kept by default, so an all-stopped cutover continues
// in the same run into the install and the adoption. --replace-cluster stops
// after the snapshot with the reset recipe and the next command, because that
// cluster is the operator's to throw away.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// cutoverOptions is cutover's flags: everything a read of the source takes, the
// one choice about the cluster, and the flags the install it continues into
// needs.
type cutoverOptions struct {
	discoverOptions
	// Replace is --replace-cluster: stop after the final snapshot.
	Replace bool
	// apply is the second half's flags, shared with `box migrate apply` so a
	// cutover and an apply of its snapshot cannot take different files -- and
	// so the answers a host with no box needs are asked for once.
	apply *migrateApplyFlags
}

func runMigrateCutover(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate cutover", flag.ExitOnError)
	o := cutoverOptions{discoverOptions: discoverOptions{Box: version}}
	ns := addDiscoverFlags(fs, &o.discoverOptions, "the final snapshot to write (default: "+bundle.Dir+"/migrate-<time>-final.tar.gz)")
	o.apply = addCutoverApplyFlags(fs, &o.Kubeconfig)
	// The read and the install name one release: --chart-repo and
	// --chart-version are apply's flags, and the plan's image variants come
	// from that same chart.
	o.chart = o.apply.chart
	replace := fs.Bool("replace-cluster", false, "stop after the final snapshot, for a host whose cluster box should replace with its own")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.Namespaces = *ns
	o.Replace = *replace
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "box: migrate cutover takes no arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
	if *o.apply.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	// --yes is this verb's "go ahead without asking", and the defaults Settle
	// takes are part of what it answers. Without it, a terminal on stdin is
	// asked; a pipe is nobody to ask, and takes them all, saying so.
	o.Plain = *o.apply.plain
	if !*o.apply.yes && isTerminal(os.Stdin) {
		o.Prompt = newPrompt(terminalSet(o.Plain), startupTheme(*o.apply.valuesPath))
	}

	out := newOutput(terminalSet(*o.apply.plain), *o.apply.yes, startupTheme(*o.apply.valuesPath))
	defer out.Close()
	if err := hostFiles(out, *o.apply.install.record, *o.apply.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}
	if err := migrateCutover(ctx, o, out); err != nil {
		if errors.Is(err, errCutoverStillRunning) {
			// The refusal already printed what is running and what stops it:
			// the commands are the point, and an error line on top of them
			// would bury them.
			os.Exit(1)
		}
		if errors.Is(err, errCutoverStopped) {
			// The replace path did what it was asked: the snapshot is written
			// and the two things left are the operator's. That is a finished
			// run, not a failed one.
			os.Exit(0)
		}
		if errors.Is(err, apply.ErrCancelled) || errors.Is(err, release.ErrCancelled) {
			out.Line("oh heck, nvm")
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
}

// errCutoverStillRunning says the source has not stopped: the refusal has
// already been printed with every app and its command.
var errCutoverStillRunning = errors.New("the source is still running")

// errCutoverStopped says the run ended by design after the snapshot (the
// replace path), so the exit is a stop and not a failure of anything.
var errCutoverStopped = errors.New("stopped after the snapshot")

// migrateCutover is the command without the exit, so a test drives its refusals.
func migrateCutover(ctx context.Context, o cutoverOptions, out say.Reporter) error {
	return cutover(ctx, o, out, unstopped)
}

// cutover is the command with the stop proof as a parameter: the proof reads a
// live source's pods, so a test hands it an answer instead of a cluster. Every
// other step is the real one.
func cutover(ctx context.Context, o cutoverOptions, out say.Reporter, proof func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error)) error {
	out.Phase("cutover")
	read, err := readSource(ctx, o.discoverOptions)
	if err != nil {
		if read.text != "" {
			out.Line(read.text)
		}
		return err
	}
	// Every rewriter's check runs before anything is written, so the survey a
	// refusal keeps carries the plan apply would run, defaults and all.
	if err := settleRead(ctx, o.discoverOptions, &read); err != nil {
		out.Line(discoveryReport(read.plan, read.report, "", read.settled, err))
		return err
	}
	running, err := proof(ctx, o, read)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		// The read is saved before any stop command is printed, so following
		// box's advice is always safe: the next cutover carries this read.
		if err := encodeRead(&read); err != nil {
			return errors.Join(errCutoverStillRunning, err)
		}
		read.text = discoveryReport(read.plan, read.report, "", read.settled, nil)
		survey, serr := writeSnapshot(surveyPath(o), archive.ModeSurvey, o.discoverOptions, read)
		if serr != nil {
			return errors.Join(errCutoverStillRunning, serr)
		}
		out.Warn(fmt.Sprintf("%s still running: cutover takes the final snapshot with every planned app stopped", countApps(len(running))))
		out.Line(fmt.Sprintf("survey: %s (the read is kept, so following the commands below is safe)", survey.Path))
		out.List("stop these, and run cutover again", stopLines(running))
		out.Muted("box never stops a source: these are commands to double-check and run yourself.")
		return errCutoverStillRunning
	}
	// The source is stopped, so the run is going ahead: this is where the
	// operator answers the defaults, and where an unattended run says it took
	// them all.
	if err := askDefaults(&read, out, o.discoverOptions); err != nil {
		return err
	}

	path := snapshotPath(o.discoverOptions, archive.ModeFinal)
	if err := encodeRead(&read); err != nil {
		return err
	}
	read.text = discoveryReport(read.plan, read.report, path, read.settled, nil)
	wrote, err := writeSnapshot(path, archive.ModeFinal, o.discoverOptions, read)
	if err != nil {
		return err
	}
	out.Line(read.text)
	out.Success("wrote " + wrote.Path + " (final)")
	for _, s := range wrote.Skipped {
		out.Muted(fmt.Sprintf("%s: left out of the snapshot: %s", s.Path, s.Why))
	}
	// The last screen names every default this run took: an address or a path
	// nobody answered is a decision box made, and the operator leaves the run
	// knowing which ones those were.
	if defaults := read.settled.Defaults(); !defaults.Empty() {
		out.List("defaults taken", defaults.Lines())
	}

	if o.Replace {
		out.Phase("replace")
		out.Line("the final snapshot is taken. This cluster is yours to replace: box does not remove a cluster it did not install.")
		// The teardown names the distribution the source's own nodes carry.
		// A node list the read could not get is already a report line, and it
		// leaves the recipe saying box cannot tell rather than guessing.
		nodes, _ := read.src.Nodes(ctx)
		out.List("to double-check before you run it", resetLines(o, nodes))
		out.Line(fmt.Sprintf("then: box migrate apply %s", wrote.Path))
		return errCutoverStopped
	}
	// The cluster is kept: the same run continues into the install and the
	// adoption, so a cutover is one command on a host box may keep.
	out.Phase("install")
	return migrateApply(ctx, wrote.Path, o.apply, out)
}

// addCutoverApplyFlags registers the half of apply's flags a cutover needs and
// points them at the kubeconfig discover's own flag holds: this verb reads one
// cluster and installs into the same one, so --kubeconfig means one thing here.
// The values file is shared the same way -- the read writes a plan, the install
// applies this file's values, and bootstrap, init and apply all take it alike.
func addCutoverApplyFlags(fs *flag.FlagSet, kubeconfig *string) *migrateApplyFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &migrateApplyFlags{
		applyFlags: &applyFlags{chart: addChartFlags(fs, chartVersionApplies)},
		install:    addInstallFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
	}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file")
	o.yes = fs.Bool("yes", false, "apply without asking")
	o.kubeconfig = kubeconfig
	return o
}

// surveyPath is where the read a refusal keeps goes: beside the final snapshot
// when --out named one, so both are in the directory the operator chose, and
// otherwise in the snapshot directory under a survey's own name.
func surveyPath(o cutoverOptions) string {
	if strings.TrimSpace(o.Out) == "" {
		return filepath.Join(bundle.Dir, bundle.FileName(archive.ModeSurvey, o.clock()))
	}
	return filepath.Join(filepath.Dir(o.Out), bundle.FileName(archive.ModeSurvey, o.clock()))
}

// unstopped is the stop proof: the source's workloads and pods, over the plan
// that was just read. A read from a saved directory has no pods behind it, and
// cutover refuses it: the proof is the whole reason cutover exists.
func unstopped(ctx context.Context, o cutoverOptions, read migrationRead) ([]discover.Running, error) {
	if o.FromDir != "" {
		return nil, fmt.Errorf("cutover proves the source stopped by reading the pods it still has, and --from-dir is a saved read with none: run cutover against the cluster")
	}
	return discover.Unstopped(ctx, read.src, read.plan)
}

// stopLines is one line per running app: what it is, and the command that stops
// it.
func stopLines(running []discover.Running) []string {
	out := make([]string, 0, len(running))
	for _, r := range running {
		out = append(out, fmt.Sprintf("%s: %s in %s/%s; %s", r.App, r.Why(), r.Namespace, r.Name, r.StopCommand()))
	}
	return out
}

// resetLines is the teardown, as the one thing left for the operator to run
// themselves before `box migrate apply` can install box's own cluster. It is
// the recipe for the distribution the source's nodes say installed it
// (discover.Distribution): the same command on another distribution removes
// nothing, so a cluster box cannot place gets a line that says so instead of a
// teardown for a guess.
func resetLines(o cutoverOptions, nodes []discover.Node) []string {
	var lines []string
	switch discover.Distribution(nodes) {
	case discover.K0s:
		lines = []string{
			"k0s stop; k0s reset",
			"then remove the unit and the binary: systemctl disable --now k0scontroller; rm -f /etc/systemd/system/k0scontroller.service /usr/local/bin/k0s",
		}
	case discover.K3s:
		lines = []string{"/usr/local/bin/k3s-uninstall.sh, which stops the server and the agent, then removes them and their data"}
	default:
		lines = []string{"box can't tell how this cluster was installed: remove it the way it was installed, before anything else uses this host"}
	}
	return append(lines, fmt.Sprintf("the source's manifests are in the snapshot (%s) and its host directories are untouched", filepath.Base(snapshotPath(o.discoverOptions, archive.ModeFinal))))
}

// countApps is "one app" or "3 apps", for a warning.
func countApps(n int) string {
	if n == 1 {
		return "one planned app is"
	}
	return fmt.Sprintf("%d planned apps are", n)
}
