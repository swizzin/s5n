package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// `box status` is one line, as docs/box.md decided: what the box is on, the last
// update, the next check, and anything held. It reads files and Helm only, never
// a registry. The line is a contract, pinned by a test.

// statusFacts is everything the line says, read before it is written.
type statusFacts struct {
	// Name and Installed are the chart and release; Unreachable means no read.
	Name        string
	Installed   string
	Unreachable bool
	// State is update.json: last step, last check and failures.
	State update.State
	// Timer is `update.timer`; TimerInstalled says the unit is here.
	Timer          bool
	TimerInstalled bool
	Interval       time.Duration
	// HoldBox, Holds and Pins are the three things kept where they are.
	HoldBox bool
	Holds   []snapshot.Hold
	Pins    map[string]string
	// Now is the clock the next check is judged against.
	Now time.Time
}

// statusLine is the line in docs/box.md's order; the held segment is omitted
// when empty.
func statusLine(f statusFacts) string {
	segments := []string{
		statusRelease(f),
		statusLastUpdate(f.State),
		statusNextCheck(f),
	}
	if held := statusHeld(f); held != "" {
		segments = append(segments, held)
	}
	return strings.Join(segments, " · ")
}

// statusRelease is Helm's version or "?"; the rest of the line needs no cluster.
func statusRelease(f statusFacts) string {
	switch {
	case f.Unreachable:
		return f.Name + " ? (cluster not reachable)"
	case f.Installed == "":
		return f.Name + " ? (no release is deployed)"
	default:
		return f.Name + " " + f.Installed
	}
}

// statusLastUpdate is the last step in local time and how it ended.
func statusLastUpdate(state update.State) string {
	step := state.LastStep
	if step == nil {
		return "last update none"
	}
	line := fmt.Sprintf("last update %s → %s %s %s", step.From, step.To,
		step.At.Local().Format("Jan 2 15:04"), step.Outcome)
	if len(step.RolledBack) > 0 && step.Outcome == update.OutcomeOK {
		line += ", " + strings.Join(step.RolledBack, ", ") + " rolled back"
	}
	if step.Gate != "" && step.Outcome == update.OutcomeFailed {
		line += " (" + step.Gate + ")"
	}
	return line
}

// statusNextCheck is off, not installed, within the interval, or a clock time.
func statusNextCheck(f statusFacts) string {
	switch {
	case !f.Timer:
		return "timer off"
	case !f.TimerInstalled:
		return "timer not installed"
	case f.State.Due(f.Now, f.Interval):
		return fmt.Sprintf("next check within %dm", hostconf.WakeInterval)
	default:
		return "next check ~" + f.State.LastCheck.Add(f.Interval).Local().Format("15:04")
	}
}

// statusHeld names each held thing and why: "held" alone says nothing to do.
func statusHeld(f statusFacts) string {
	var parts []string
	if f.HoldBox {
		parts = append(parts, "box")
	}
	for _, version := range f.State.Failed {
		parts = append(parts, "release "+version+" (failed)")
	}
	for _, h := range f.Holds {
		parts = append(parts, fmt.Sprintf("%s (rolled back from %s)", h.App, h.Avoiding.Label()))
	}
	apps := make([]string, 0, len(f.Pins))
	for app := range f.Pins {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		parts = append(parts, app+" (pinned)")
	}
	if len(parts) == 0 {
		return ""
	}
	return "held: " + strings.Join(parts, ", ")
}

// statusFlags are status's flags, including the dev unit directory.
type statusFlags struct {
	record     *string
	grantPath  *string
	valuesPath *string
	kubeconfig *string
}

func addStatusFlags(fs *flag.FlagSet) *statusFlags {
	valuesDefault := ""
	if home, err := os.UserHomeDir(); err == nil {
		valuesDefault = seedbox.ValuesPath(home)
	}
	return &statusFlags{
		record:     fs.String("record", bootstrap.RecordPath, "where box recorded the cluster decision; update's record is read beside it"),
		grantPath:  fs.String("grant", seedbox.GrantPath("root"), "the root-owned grant file"),
		valuesPath: fs.String("values", valuesDefault, "the user's values file"),
		kubeconfig: fs.String("kubeconfig", "", "explicit kubeconfig to use, or fail with no fallback"),
	}
}

func runStatus(ctx context.Context, args []string) {
	// --unit-dir is a dev switch: the unit location on another host.
	unitDir, args, err := takeDevFlag(args, "unit-dir")
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	if unitDir == "" {
		unitDir = hostconf.UnitDir
	}
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	o := addStatusFlags(fs)
	if err := parseNoArgs(fs, "status", args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	out := &say.Plain{W: os.Stdout, Err: os.Stderr}
	if err := hostFiles(out, *o.record, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	facts, err := readStatus(ctx, o, unitDir)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	out.Line(statusLine(facts))
}

// readStatus reads files and Helm's deployed release, never a registry.
func readStatus(ctx context.Context, o *statusFlags, unitDir string) (statusFacts, error) {
	facts := statusFacts{Name: release.ReleaseName, Now: time.Now()}

	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return facts, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return facts, err
	}
	conf, _, err := hostconf.Load(hostconf.Path(*o.record))
	if err != nil {
		return facts, err
	}
	facts.Timer, facts.Interval, facts.HoldBox = conf.Timer, conf.Interval, conf.HoldBox
	facts.Pins = valuePins(sb)
	if _, err := os.Stat(unitDir + string(os.PathSeparator) + hostconf.TimerFile); err == nil {
		facts.TimerInstalled = true
	}
	state, err := update.FileState{Path: update.FilePath(*o.record)}.Load()
	if err != nil {
		return facts, err
	}
	facts.State = state

	// Holds live beside the config root; no grant holds nothing.
	if root, err := update.ConfigRoot(sb); err == nil {
		holds, err := snapshot.LoadHolds(root)
		if err != nil {
			return facts, err
		}
		facts.Holds = holds
	}

	// A failed cluster read is an answer; the other segments still print.
	deployed, unreachable, err := readDeployed(ctx, statusClusterTimeout, func(ctx context.Context) (release.ReleaseState, error) {
		kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.record})
		if err != nil {
			return release.ReleaseState{}, err
		}
		helm, err := release.NewHelmWithTimeout(kc, namespace, release.ReleaseName, statusClusterTimeout)
		if err != nil {
			return release.ReleaseState{}, err
		}
		return helm.State(ctx)
	})
	if err != nil {
		return facts, err
	}
	facts.Unreachable = unreachable
	facts.Installed = deployed.ChartVersion
	return facts, nil
}

// statusClusterTimeout bounds the line's one cluster read. Helm's
// release storage reads with a context of its own (context.Background()), so it
// is set as client-go's per-request timeout as well as this deadline: the
// deadline is what the read is judged by, the timeout is what stops a server
// that never answers.
const statusClusterTimeout = 10 * time.Second

// statusState is that read: the deployed release. It is a value so a test can
// hand it a server that never answers.
type statusState func(context.Context) (release.ReleaseState, error)

// readDeployed is the cluster read under the deadline, with the deadline's
// answer being the line's: a server that ran out of time is "cluster not
// reachable", not an error, so the other segments still print. A Ctrl-C stays
// an error -- it is not an answer about the cluster.
func readDeployed(ctx context.Context, timeout time.Duration, read statusState) (release.ReleaseState, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state, err := read(ctx)
	switch {
	case err == nil:
		return state, false, nil
	case errors.Is(err, context.Canceled) && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return release.ReleaseState{}, false, err
	default:
		return release.ReleaseState{}, true, nil
	}
}

// valuePins is the apps the user's values file pins an image for, and the label
// each pin is: the tag when it pins one, else the digest. It is read from the
// values document itself, not from a chart's effective values, because status
// asks no registry.
func valuePins(sb *seedbox.Seedbox) map[string]string {
	pins := map[string]string{}
	apps, _ := sb.Values.Map()["apps"].(map[string]any)
	for app, entry := range apps {
		fields, _ := entry.(map[string]any)
		image, _ := fields["image"].(map[string]any)
		tag, _ := image["tag"].(string)
		digest, _ := image["digest"].(string)
		switch {
		case strings.TrimSpace(tag) != "":
			pins[app] = strings.TrimSpace(tag)
		case strings.TrimSpace(digest) != "":
			pins[app] = strings.TrimSpace(digest)
		}
	}
	return pins
}
