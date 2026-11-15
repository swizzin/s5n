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

// `box status` is one line, and it is the line docs/box.md decided: what the box
// is on, what the last update did, when box checks again, and anything held. It
// reads what is already written -- Helm's deployed release, update.json, root's
// config, the holds -- and asks no registry, so it is cheap enough to run from a
// shell prompt.
//
// The line is built by a function of the facts, because it is a contract: the
// e2e greps it and a person reads it, so every variant is pinned by a test.

// statusFacts is everything the line says, read before it is written.
type statusFacts struct {
	// Name is the chart's name ("s5n") and Installed the release Helm has
	// deployed. Unreachable says the cluster could not be read at all.
	Name        string
	Installed   string
	Unreachable bool
	// State is update.json: the last step, the last check and the releases box
	// failed on.
	State update.State
	// Timer is root's `update.timer`, and TimerInstalled whether the unit that
	// wakes box is on this host.
	Timer          bool
	TimerInstalled bool
	Interval       time.Duration
	// HoldBox is root's `update.hold-box`, Holds are the apps a rollback holds,
	// and Pins are the apps the values file pins.
	HoldBox bool
	Holds   []snapshot.Hold
	Pins    map[string]string
	// Now is the clock the next check is judged against.
	Now time.Time
}

// statusLine is the one line, in the order docs/box.md lists the segments. The
// held segment is left out when nothing is held, so a healthy box is three
// segments and an operator can tell the fourth apart.
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

// statusRelease is what is running: Helm's deployed version, or an honest "?"
// when the cluster could not be read. The rest of the line still prints, because
// the two questions the other segments answer need no cluster.
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

// statusLastUpdate is the last step box attempted, in local time, with how it
// ended: "ok", "ok, radarr rolled back", or "failed (health)".
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

// statusNextCheck is when box next asks the registry: off, not installed, within
// the wake interval, or the clock time it is due.
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

// statusHeld is what is being kept where it is: the box itself, the releases it
// failed on, the apps a rollback holds, and the apps the values pin. Each names
// why, because "held" alone does not tell anyone what to do about it.
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

// statusFlags are `box status`'s: the record, the two seedbox files, the
// kubeconfig, and the development switch that says where the timer's unit would
// be on a host that is not this one.
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
	// --unit-dir is where the timer's unit would be, for a host whose systemd is
	// not this one. It is a development switch, out of the arguments the same
	// way --acme-server is: --help must not offer it.
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
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	out := &say.Plain{W: os.Stdout, Err: os.Stderr}

	facts, err := readStatus(ctx, o, unitDir)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	out.Line(statusLine(facts))
}

// readStatus reads every fact the line is built from. Nothing it reads asks a
// registry: the release is Helm's deployed one, and the rest are files.
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

	// The holds live beside the config root, under the grant's config path. A
	// box without one holds nothing: that is the same read a rollback makes.
	if root, err := update.ConfigRoot(sb); err == nil {
		holds, err := snapshot.LoadHolds(root)
		if err != nil {
			return facts, err
		}
		facts.Holds = holds
	}

	// The cluster is the one read that can fail without the line being wrong:
	// "cluster not reachable" is an answer, and the other three segments are
	// what someone looking at an unreachable box needs to see.
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

// statusClusterTimeout bounds the one cluster read the line makes. Helm's
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
