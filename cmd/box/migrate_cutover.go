package main

// migrate_cutover.go is `box migrate cutover`: the window. It reads the source,
// proves every planned app stopped (Terminating counts as running), then copies
// into the final snapshot. While any app runs it keeps a survey and prints the
// stop commands; box never stops a source. --replace-cluster stops after the
// snapshot with the reset recipe; the default continues into install.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/work"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// cutoverOptions is a read's flags, the cluster choice, and the install's flags.
type cutoverOptions struct {
	discoverOptions
	// Replace is --replace-cluster: stop after the final snapshot.
	Replace bool
	// NoPriorDiscover says there is no earlier read; it skips the warning, never
	// the diff.
	NoPriorDiscover bool
	// apply is shared with `box migrate apply`, so both take one set of files.
	apply *migrateApplyFlags
}

func runMigrateCutover(ctx context.Context, args []string) {
	// --paths-in-node is the e2e's switch, as `box migrate apply` takes it.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --unit-dir names where a fresh host's timer units land.
	unitDir, args, err := takeDevFlag(args, "unit-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate cutover", flag.ExitOnError)
	o := cutoverOptions{discoverOptions: discoverOptions{Box: version}}
	ns := addDiscoverFlags(fs, &o.discoverOptions, "the final snapshot to write (default: "+bundle.Dir+"/migrate-<time>-final.tar.gz)")
	o.apply = addCutoverApplyFlags(fs, &o.Kubeconfig)
	o.apply.install.pathsInNode = pathsInNode
	o.apply.install.unitDir = unitDir
	// One release for the read and the install: apply's flags supply it.
	o.chart = o.apply.chart
	o.ValuesPath = *o.apply.valuesPath
	replace := fs.Bool("replace-cluster", false, "stop after the final snapshot, for a host whose cluster box should replace with its own")
	noPrior := fs.Bool("no-prior-discover", false, "take the first read of this source as enough to cut over from, without asking whether it was everything")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.Namespaces = *ns
	o.Replace = *replace
	o.NoPriorDiscover = *noPrior
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "box: migrate cutover takes no arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
	if *o.apply.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	// --yes answers for the defaults too; otherwise a terminal is asked and a
	// pipe takes them all, saying so.
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
			// The refusal already printed the commands; an error line would bury
			// them.
			os.Exit(1)
		}
		if errors.Is(err, errCutoverPrior) {
			// The printed read stopped it; an error line would repeat it.
			os.Exit(1)
		}
		if errors.Is(err, errCutoverStopped) || errors.Is(err, errCutoverBlocked) {
			// The snapshot is written and the rest is the operator's: not a
			// failure.
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
// already been printed with the commands that stop it.
var errCutoverStillRunning = errors.New("the source is still running")

// errCutoverStopped says the run ended by design after the snapshot (the
// replace path), so the exit is a stop and not a failure of anything.
var errCutoverStopped = errors.New("stopped after the snapshot")

// errCutoverBlocked is a finished run: the snapshot is written, the next command
// is the operator's.
var errCutoverBlocked = errors.New("the keep path's pre-flight stopped the cutover")

// cutoverSeams are the two live-cluster checks a test replaces with answers.
type cutoverSeams struct {
	proof func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error)
	keep  func(context.Context, cutoverOptions, migrationRead) ([]discover.Blocker, error)
}

// migrateCutover is the command without the exit, so a test drives its refusals.
func migrateCutover(ctx context.Context, o cutoverOptions, out say.Reporter) error {
	return cutover(ctx, o, out, cutoverSeams{proof: unstopped, keep: keepBlockers})
}

// keepSurvey keeps a refusing cutover's read, so the next run has a prior.
func keepSurvey(read *migrationRead, o cutoverOptions) (string, error) {
	if err := encodeRead(read); err != nil {
		return "", err
	}
	path := surveyPath(o)
	read.setReport(archive.ModeSurvey, path, o.discoverOptions, nil)
	survey, err := writeSnapshot(path, archive.ModeSurvey, o.discoverOptions, *read)
	if err != nil {
		return "", err
	}
	return survey.Path, nil
}

// holders is the apps holding one value: the address is in their configs.
func holders(holds []apply.SettledHold) string {
	seen := map[string]bool{}
	var apps []string
	for _, h := range holds {
		if h.App == "" || seen[h.App] {
			continue
		}
		seen[h.App] = true
		apps = append(apps, h.App)
	}
	sort.Strings(apps)
	return strings.Join(apps, ", ")
}

// showCarried is the answers carried from the prior read, one line each.
func showCarried(out say.Reporter, carried []apply.KeptAnswer) {
	if len(carried) == 0 {
		return
	}
	out.Line("connections")
	rows := make([]say.Row, 0, len(carried))
	for _, c := range carried {
		rows = append(rows, say.Row{
			Mark:  say.MarkDone,
			Name:  holders(c.Holds),
			Facts: []string{c.Value + " → " + c.Answer()},
		})
	}
	out.Rows(rows)
}

// cutover is the command with its live checks as parameters.
func cutover(ctx context.Context, o cutoverOptions, out say.Reporter, seams cutoverSeams) error {
	out.Phase("cutover")
	// Work beside the final snapshot, never $TMPDIR.
	dir, done, err := work.New(priorDir(o))
	if err != nil {
		return err
	}
	defer done()
	o.Work = dir
	read, err := readSource(ctx, o.discoverOptions, archive.ModeFinal)
	if err != nil {
		return err
	}
	// Check before writing, so a kept survey carries the plan apply would run.
	if err := settleRead(ctx, o.discoverOptions, &read); err != nil {
		read.setReport(archive.ModeSurvey, "", o.discoverOptions, err)
		return err
	}
	// Look up the prior read before writing: it is what box saw last time.
	prior, err := findPriorRead(priorDir(o), read)
	if err != nil {
		return err
	}
	running, err := seams.proof(ctx, o, read)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		// Save the read before printing stops, so the next cutover has it.
		kept, err := keepSurvey(&read, o)
		if err != nil {
			return errors.Join(errCutoverStillRunning, err)
		}
		out.Detail("survey: " + kept)
		out.Rows([]say.Row{{
			Mark:  say.MarkNeeds,
			Facts: []string{countApps(len(running)) + " running", "stop them, then run this again"},
		}})
		// One command per namespace and kind (rule 3), for the cluster found.
		nodes, _ := read.src.Nodes(ctx)
		for _, stop := range discover.Stops(running) {
			commandLines(out, stop.Command(discover.Kubectl(nodes)))
		}
		return errCutoverStillRunning
	}
	// The prior read decides whether this one is enough: an app gone missing
	// stops the run, keeping this read.
	if err := checkPriorRead(prior, &read, o, out, func() (string, error) { return keepSurvey(&read, o) }); err != nil {
		return err
	}
	// Carry the earlier read's answers and never ask again (rule 10).
	if prior != nil {
		settled, carried := apply.CarryAnswers(read.plan, read.settled, prior.plan)
		read.settled = settled
		showCarried(out, carried)
	}
	// Ask what is left; an unattended run says how many it took.
	if err := askDefaults(&read, out, o.discoverOptions); err != nil {
		return err
	}

	path := freeSnapshotPath(o.discoverOptions, archive.ModeFinal)
	// Settle the carry first: a same-host cutover writes two copies of Plex.
	if o.apply != nil && o.apply.grantPath != nil {
		o.TargetConfig = targetConfigPath(*o.apply.grantPath)
	}
	short, err := decideSnapshotCarry(&read, o.discoverOptions, path, archive.ModeFinal)
	if err != nil {
		read.setReport(archive.ModeFinal, "", o.discoverOptions, err)
		return err
	}
	// The page says what is left out, read once by the operator.
	read.report.Notes = append(read.report.Notes, carryLines(read.plan)...)
	read.report.Notes = append(read.report.Notes, short...)
	if err := encodeRead(&read); err != nil {
		return err
	}
	read.setReport(archive.ModeFinal, path, o.discoverOptions, nil)
	wrote, err := writeSnapshot(path, archive.ModeFinal, o.discoverOptions, read)
	if err != nil {
		return err
	}
	read.digest("box migrate cutover", out)
	out.Detail("wrote " + wrote.Path + " (final)")
	for _, s := range wrote.Skipped {
		out.Muted(fmt.Sprintf("%s: left out of the snapshot: %s", s.Path, s.Why))
	}
	// End with the count of decisions box took; the report names each reason.
	read.defaultsRow(out)

	if o.Replace {
		out.Phase("replace")
		// The cluster is the operator's to remove; the commands are the answer
		// (rule 11).
		out.Rows([]say.Row{{
			Mark:  say.MarkNeeds,
			Facts: []string{"this cluster is yours to replace"},
		}})
		// The teardown follows the nodes' distribution; an unread list leaves the
		// recipe saying so rather than guessing.
		nodes, _ := read.src.Nodes(ctx)
		for _, step := range resetSteps(nodes) {
			if !step.Command {
				out.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{step.Text}}})
				continue
			}
			commandLines(out, step.Text)
		}
		commandLines(out, "box migrate apply "+wrote.Path)
		out.Detail(fmt.Sprintf("the source's manifests are in the snapshot (%s) and its host directories are untouched", filepath.Base(wrote.Path)))
		return errCutoverStopped
	}
	// The kept cluster continues into install, unless the pre-flight finds
	// something box did not make and must not install over.
	if seams.keep != nil {
		blockers, err := seams.keep(ctx, o, read)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			lines := make([]string, 0, len(blockers))
			for _, b := range blockers {
				lines = append(lines, b.String())
			}
			out.Warn(blockerWarning(len(blockers)))
			out.List("check these, and clear each one yourself", lines)
			out.Line("--replace-cluster is the other road: it stops here, after this snapshot, and `box migrate apply " + wrote.Path + "` installs box's own cluster on that host")
			return errCutoverBlocked
		}
	}

	out.Phase("install")
	return migrateApply(ctx, wrote.Path, o.apply, out)
}

// addCutoverApplyFlags shares apply's flags and discover's kubeconfig: one
// cluster is read and installed into, and one values file feeds both halves.
func addCutoverApplyFlags(fs *flag.FlagSet, kubeconfig *string) *migrateApplyFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	o := &migrateApplyFlags{
		applyFlags: &applyFlags{chart: addChartFlags(fs, chartVersionApplies)},
		install:    addInstallFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
		details:    fs.Bool("details", false, "print every rewrite, column by column, instead of the grouped summary"),
		yolo:       fs.Bool("yolo", false, "run a downgrade: an older app on a config a newer one wrote (refused otherwise)"),
	}
	o.grantPath = fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file")
	o.valuesPath = fs.String("values", valuesDefault, "the user's values file")
	o.yes = fs.Bool("yes", false, "apply without asking")
	o.kubeconfig = kubeconfig
	return o
}

// targetConfigPath is the config path this host's own grant names, or "" when it
// has no box (or a grant it cannot read). It is the second place a cutover's
// snapshot is written to, on the host that is both ends of the one migration.
func targetConfigPath(grantPath string) string {
	sb, err := seedbox.Load(grantPath, "")
	if err != nil {
		// No grant is a host with no box, which writes no copy; a grant box
		// cannot read is the install's refusal to make, not this check's.
		return ""
	}
	vols, err := sb.Storage()
	if err != nil {
		return ""
	}
	for _, v := range vols {
		if v.Kind == volumeConfig {
			return v.Path
		}
	}
	return ""
}

// surveyPath is beside a named --out, else a survey name in bundle.Dir.
func surveyPath(o cutoverOptions) string {
	if strings.TrimSpace(o.Out) == "" {
		return freeSnapshotPath(o.discoverOptions, archive.ModeSurvey)
	}
	// Beside the named final, with a free name: two refusals are two reads.
	at := o.clock()
	dir := filepath.Dir(o.Out)
	for i := 0; i < 60; i++ {
		path := filepath.Join(dir, bundle.FileName(archive.ModeSurvey, at.Add(time.Duration(i)*time.Second)))
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
	}
	return filepath.Join(dir, bundle.FileName(archive.ModeSurvey, at))
}

// unstopped proves the source stopped; a saved read has no pods, so cutover
// refuses it: the proof is the reason cutover exists.
func unstopped(ctx context.Context, o cutoverOptions, read migrationRead) ([]discover.Running, error) {
	if o.FromDir != "" {
		return nil, fmt.Errorf("cutover proves the source stopped by reading the pods it still has, and --from-dir is a saved read with none: run cutover against the cluster")
	}
	return discover.Unstopped(ctx, read.src, read.plan)
}

// resetStep is one line of a teardown: a command a person runs, or a note that is
// not a command at all (a `$` line is only ever something to copy, rule 3).
type resetStep struct {
	Command bool
	Text    string
}

// resetSteps is the operator's teardown for the distribution the nodes name
// (discover.Distribution); an unknown cluster gets a line rather than a guess.
func resetSteps(nodes []discover.Node) []resetStep {
	switch discover.Distribution(nodes) {
	case discover.K0s:
		// k0s reset leaves /etc/k0s/k0s.yaml, which bootstrap refuses; remove it
		// so the next command does not fail.
		return []resetStep{
			{Command: true, Text: "sudo k0s stop; sudo k0s reset; sudo rm -f /etc/k0s/k0s.yaml"},
			{Command: true, Text: "sudo systemctl disable --now k0scontroller; sudo rm -f /etc/systemd/system/k0scontroller.service /usr/local/bin/k0s"},
		}
	case discover.K3s:
		return []resetStep{{Command: true, Text: "sudo /usr/local/bin/k3s-uninstall.sh"}}
	}
	return []resetStep{{Text: "remove this cluster the way it was installed, before anything else uses this host"}}
}

// blockerWarning counts what box found and states that it touches none of it.
func blockerWarning(n int) string {
	if n == 1 {
		return "one thing on this cluster stops box installing onto it, and box does not change a cluster it did not install"
	}
	return fmt.Sprintf("%d things on this cluster stop box installing onto it, and box does not change a cluster it did not install", n)
}

// countApps is "1 app" or "12 apps", for the line above the stop commands.
func countApps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}
