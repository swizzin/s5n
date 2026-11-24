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

// cutoverOptions is cutover's flags: everything a read of the source takes, the
// one choice about the cluster, and the flags the install it continues into
// needs.
type cutoverOptions struct {
	discoverOptions
	// Replace is --replace-cluster: stop after the final snapshot.
	Replace bool
	// NoPriorDiscover is --no-prior-discover: the operator's word that there is
	// no earlier read of this source to compare against, so a first read is
	// enough to cut over from. It skips the warning, never the diff.
	NoPriorDiscover bool
	// apply is the second half's flags, shared with `box migrate apply` so a
	// cutover and an apply of its snapshot cannot take different files -- and
	// so the answers a host with no box needs are asked for once.
	apply *migrateApplyFlags
}

func runMigrateCutover(ctx context.Context, args []string) {
	// --paths-in-node is the e2e's storage switch, taken out of the arguments
	// before the FlagSet sees them, exactly as `box migrate apply` takes it: the
	// keep path hands over to that same install, and the app's directories and the
	// login's are inside the node there.
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	// --unit-dir is bootstrap's timer switch: the keep path can install a box on
	// a host with none, and the timer that install writes lands in the directory
	// this run names.
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
	// The read and the install name one release: --chart-repo and
	// --chart-version are apply's flags, and the plan's image variants come
	// from that same chart.
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
		if errors.Is(err, errCutoverPrior) {
			// The read already on record stopped it: what it found, where the
			// survey went and what to do next are all printed, so an error line
			// on top would only repeat them.
			os.Exit(1)
		}
		if errors.Is(err, errCutoverStopped) || errors.Is(err, errCutoverBlocked) {
			// The run did what it was asked: the snapshot is written and the
			// rest is the operator's. That is a finished run, not a failed one.
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

// errCutoverBlocked says the keep path found something on the source cluster box
// will not install over, and stopped after the final snapshot. That is a finished
// run, not a failed one: the other road is the operator's next command.
var errCutoverBlocked = errors.New("the keep path's pre-flight stopped the cutover")

// cutoverSeams are the two checks that read a live cluster, so a test hands over
// an answer instead of a cluster: the stop proof, and the keep path's pre-flight.
// Everything else cutover does is the real one.
type cutoverSeams struct {
	proof func(context.Context, cutoverOptions, migrationRead) ([]discover.Running, error)
	keep  func(context.Context, cutoverOptions, migrationRead) ([]discover.Blocker, error)
}

// migrateCutover is the command without the exit, so a test drives its refusals.
func migrateCutover(ctx context.Context, o cutoverOptions, out say.Reporter) error {
	return cutover(ctx, o, out, cutoverSeams{proof: unstopped, keep: keepBlockers})
}

// keepSurvey writes this read as a survey and returns its file: it is what every
// refusing cutover does with what it read, so following box's advice is always
// safe and the next cutover has an earlier read to diff against.
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

// holders is the apps that hold one value, which is the subject a settle line is
// about: the address is in their configs, so it is their connection.
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

// showCarried is the answers a cutover did not ask for: one line each, the apps
// that hold the address and what it now points at. The heading is only there
// when there is something under it.
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

// cutover is the command with its live-cluster checks as parameters, so a test
// drives every refusal without a cluster. Every other step is the real one.
func cutover(ctx context.Context, o cutoverOptions, out say.Reporter, seams cutoverSeams) error {
	out.Phase("cutover")
	// The run's working directory is beside the final snapshot it writes: the
	// databases it copies and the copies its checks read go there, never in
	// $TMPDIR.
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
	// Every rewriter's check runs before anything is written, so the survey a
	// refusal keeps carries the plan apply would run, defaults and all.
	if err := settleRead(ctx, o.discoverOptions, &read); err != nil {
		read.setReport(archive.ModeSurvey, "", o.discoverOptions, err)
		return err
	}
	// The read already on record, looked up before this run writes anything: the
	// question is what box saw last time, not what this run is about to save.
	prior, err := findPriorRead(priorDir(o), read)
	if err != nil {
		return err
	}
	running, err := seams.proof(ctx, o, read)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		// The read is saved before any stop command is printed, so following
		// box's advice is always safe: the next cutover carries this read.
		kept, err := keepSurvey(&read, o)
		if err != nil {
			return errors.Join(errCutoverStillRunning, err)
		}
		out.Detail("survey: " + kept)
		out.Rows([]say.Row{{
			Mark:  say.MarkNeeds,
			Facts: []string{countApps(len(running)) + " running", "stop them, then run this again"},
		}})
		// One command per namespace and kind: many similar stops become one
		// (rule 3), and each is written for the cluster box found.
		nodes, _ := read.src.Nodes(ctx)
		for _, stop := range discover.Stops(running) {
			commandLines(out, stop.Command(discover.Kubectl(nodes)))
		}
		return errCutoverStillRunning
	}
	// The source is stopped. What box saw of it before decides whether this read
	// is enough to migrate from: an app gone missing since then stops the run
	// here, with this read kept.
	if err := checkPriorRead(prior, &read, o, out, func() (string, error) { return keepSurvey(&read, o) }); err != nil {
		return err
	}
	// What the earlier read already answered is carried, one line each, and
	// never asked again (rule 10): a person's answer is theirs across runs.
	if prior != nil {
		settled, carried := apply.CarryAnswers(read.plan, read.settled, prior.plan)
		read.settled = settled
		showCarried(out, carried)
	}
	// The source is stopped, so the run is going ahead: this is where the
	// operator answers what is left, and where an unattended run says how many
	// it took.
	if err := askDefaults(&read, out, o.discoverOptions); err != nil {
		return err
	}

	path := freeSnapshotPath(o.discoverOptions, archive.ModeFinal)
	// What the snapshot carries is settled before it is written: on a host that
	// already has a box this run writes the snapshot and the seedbox's copy on
	// one machine, so Plex's whole data has to fit twice.
	if o.apply != nil && o.apply.grantPath != nil {
		o.TargetConfig = targetConfigPath(*o.apply.grantPath)
	}
	short, err := decideSnapshotCarry(&read, o.discoverOptions, path, archive.ModeFinal)
	if err != nil {
		read.setReport(archive.ModeFinal, "", o.discoverOptions, err)
		return err
	}
	// What the snapshot carries and does not is part of the page: Plex's caches,
	// and its whole library when one does not fit. The operator reads that once,
	// here.
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
	// The last screen names how many decisions this run took: an address or a
	// path nobody answered is a decision box made, and the report names each one
	// with its reason.
	read.defaultsRow(out)

	if o.Replace {
		out.Phase("replace")
		// The one fact: the cluster is the operator's to remove. What box does
		// not do about it needs no sentence; the commands below are the answer
		// (rule 11).
		out.Rows([]say.Row{{
			Mark:  say.MarkNeeds,
			Facts: []string{"this cluster is yours to replace"},
		}})
		// The teardown names the distribution the source's own nodes carry.
		// A node list the read could not get is already a report line, and it
		// leaves the recipe saying box cannot tell rather than guessing.
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
	// The cluster is kept by default, so the same run continues into the install
	// and the adoption. What it must not do first is install over something on a
	// cluster box did not make: the pre-flight names each of those, and the road
	// for a cluster that has them.
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

// surveyPath is where the read a refusal keeps goes: beside the final snapshot
// when --out named one, so both are in the directory the operator chose, and
// otherwise in the snapshot directory under a survey's own name.
func surveyPath(o cutoverOptions) string {
	if strings.TrimSpace(o.Out) == "" {
		return freeSnapshotPath(o.discoverOptions, archive.ModeSurvey)
	}
	// The survey goes beside the final snapshot the operator named, and its name
	// is made free the same way: two refusals in one second are two reads.
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

// unstopped is the stop proof: the source's workloads and pods, over the plan
// that was just read. A read from a saved directory has no pods behind it, and
// cutover refuses it: the proof is the whole reason cutover exists.
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

// resetSteps is the teardown, as the one thing left for the operator to run
// themselves before `box migrate apply` can install box's own cluster. It is
// the recipe for the distribution the source's nodes say installed it
// (discover.Distribution): the same command on another distribution removes
// nothing, so a cluster box cannot place gets a line that says so instead of a
// teardown for a guess.
func resetSteps(nodes []discover.Node) []resetStep {
	switch discover.Distribution(nodes) {
	case discover.K0s:
		// `k0s reset` leaves /etc/k0s/k0s.yaml behind, and box's own bootstrap
		// refuses a k0s config it did not write: the recipe removes it, so the
		// next command is not a refusal this screen exists to save a person from.
		return []resetStep{
			{Command: true, Text: "sudo k0s stop; sudo k0s reset; sudo rm -f /etc/k0s/k0s.yaml"},
			{Command: true, Text: "sudo systemctl disable --now k0scontroller; sudo rm -f /etc/systemd/system/k0scontroller.service /usr/local/bin/k0s"},
		}
	case discover.K3s:
		return []resetStep{{Command: true, Text: "sudo /usr/local/bin/k3s-uninstall.sh"}}
	}
	return []resetStep{{Text: "remove this cluster the way it was installed, before anything else uses this host"}}
}

// blockerWarning is the line over the keep path's list: how many things box found,
// and the rule that keeps box from touching any of them.
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
