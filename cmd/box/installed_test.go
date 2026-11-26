package main

import (
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

// applyOrder is one log of the steps a run reached, in the order it reached
// them: the release read, the chart load, the question and the write.
type applyOrder struct{ calls []string }

func (o *applyOrder) add(call string) { o.calls = append(o.calls, call) }

func (o *applyOrder) got() string { return strings.Join(o.calls, " ") }

// installed is the release read runApply makes before it decides anything.
func (o *applyOrder) installed(state release.ReleaseState, err error) func(context.Context) (release.ReleaseState, error) {
	return func(context.Context) (release.ReleaseState, error) {
		o.add("release")
		return state, err
	}
}

// runApplyPlan drives the steps runApply runs, in its order: the release read,
// then -- only when that did not refuse -- the chart load, the question and the
// write. A step is logged where the harness performs it, so a test can see how
// far a run got. The write is logged rather than performed: it is the Helm
// upgrade inside Applier.Apply, past the point these tests are about.
func runApplyPlan(t *testing.T, opts release.Options, order *applyOrder, charts *recordingCharts, state release.ReleaseState) (release.Options, error) {
	t.Helper()
	decided, err := applyRelease(context.Background(), opts, order.installed(state, nil))
	if err != nil {
		return decided, err
	}
	order.add("chart")
	if _, err := charts.Load(context.Background(), decided.ChartRef()); err != nil {
		t.Fatalf("load: %v", err)
	}
	order.add("confirm")
	if _, err := (answered{}).Ask("apply?", false); err != nil {
		t.Fatalf("ask: %v", err)
	}
	order.add("write")
	return decided, nil
}

func applyFlagsFor(t *testing.T, args ...string) *applyFlags {
	t.Helper()
	var flags *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { flags = addApplyFlags(fs) }, args...)
	return flags
}

func TestBareApplyKeepsTheInstalledRelease(t *testing.T) {
	flags := applyFlagsFor(t, "--chart-repo", testChartRepo)
	order := &applyOrder{}
	charts := &recordingCharts{}
	state := release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 4, Status: "deployed"}

	opts, err := runApplyPlan(t, flags.options(), order, charts, state)
	if err != nil {
		t.Fatalf("applyRelease: %v", err)
	}
	if got, want := opts.ChartRef(), (release.ChartRef{Repo: testChartRepo, Version: "0.5.0"}); got != want {
		t.Fatalf("apply asks for %+v, want the installed release %+v", got, want)
	}
	if len(charts.refs) != 1 || charts.refs[0] != opts.ChartRef() {
		t.Fatalf("the chart source was asked for %+v, want the installed release", charts.refs)
	}
	if got, want := order.got(), "release chart confirm write"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestBareApplyWithNothingInstalledAsksForTheNewest(t *testing.T) {
	flags := applyFlagsFor(t)
	order := &applyOrder{}
	charts := &recordingCharts{}

	opts, err := runApplyPlan(t, flags.options(), order, charts, release.ReleaseState{})
	if err != nil {
		t.Fatalf("applyRelease: %v", err)
	}
	if got, want := opts.ChartRef(), (release.ChartRef{Repo: release.DefaultChartRepo}); got != want {
		t.Fatalf("apply asks for %+v, want the newest release %+v", got, want)
	}
	if len(charts.refs) != 1 || charts.refs[0] != opts.ChartRef() {
		t.Fatalf("the chart source was asked for %+v, want the newest release", charts.refs)
	}
	if got, want := order.got(), "release chart confirm write"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestApplyAcceptsTheInstalledReleaseItNames(t *testing.T) {
	flags := applyFlagsFor(t, "--chart-repo", testChartRepo, "--chart-version", "0.5.0")
	order := &applyOrder{}
	charts := &recordingCharts{}
	state := release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 4, Status: "deployed"}

	opts, err := runApplyPlan(t, flags.options(), order, charts, state)
	if err != nil {
		t.Fatalf("the installed release was refused: %v", err)
	}
	if got, want := opts.ChartRef(), (release.ChartRef{Repo: testChartRepo, Version: "0.5.0"}); got != want {
		t.Fatalf("apply asks for %+v, want %+v", got, want)
	}
	if got, want := order.got(), "release chart confirm write"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestApplyRefusesAChartVersionThatWouldMoveTheRelease(t *testing.T) {
	flags := applyFlagsFor(t, "--chart-repo", testChartRepo, "--chart-version", "0.6.0")
	order := &applyOrder{}
	state := release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 4, Status: "deployed"}

	_, err := runApplyPlan(t, flags.options(), order, &recordingCharts{}, state)
	if err == nil {
		t.Fatal("a --chart-version that moves the release was accepted")
	}
	if got, want := err.Error(), "apply keeps the installed release (0.5.0); `box update` moves it to 0.6.0"; got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
	// The log is everything a run reached: the release read, and not the chart
	// load, the question or the write that sit behind the decision.
	if got, want := order.got(), "release"; got != want {
		t.Fatalf("calls = %q, want only %q", got, want)
	}
}

func TestApplyReportsAReleaseItCouldNotRead(t *testing.T) {
	flags := applyFlagsFor(t)
	order := &applyOrder{}

	_, err := applyRelease(context.Background(), flags.options(), order.installed(release.ReleaseState{}, context.DeadlineExceeded))
	if err == nil {
		t.Fatal("an unreadable release was treated as nothing installed")
	}
	if got, want := order.got(), "release"; got != want {
		t.Fatalf("calls = %q, want only %q", got, want)
	}
}

func TestChartVersionHelpIsTheCommands(t *testing.T) {
	cases := []struct {
		name string
		add  func(*flag.FlagSet)
		want string
	}{
		{"init", func(fs *flag.FlagSet) { addInitFlags(fs) }, chartVersionInstalls},
		{"bootstrap", func(fs *flag.FlagSet) { addInitFlags(fs) }, chartVersionInstalls},
		{"update", func(fs *flag.FlagSet) { addUpdateFlags(fs) }, chartVersionInstalls},
		{"apply", func(fs *flag.FlagSet) { addApplyFlags(fs) }, chartVersionApplies},
		{"rollback", func(fs *flag.FlagSet) { addRollbackFlags(fs) }, "not used: a rollback applies the release that is installed"},
	}
	if chartVersionInstalls != "chart release to install (default: the newest released version this box can use)" {
		t.Fatalf("the install wording changed: %q", chartVersionInstalls)
	}
	if chartVersionApplies != "chart release to apply (default: the one installed; the newest this box can use when nothing is installed)" {
		t.Fatalf("the apply wording changed: %q", chartVersionApplies)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			tc.add(fs)
			f := fs.Lookup("chart-version")
			if f == nil {
				t.Fatal("--chart-version is not registered")
			}
			if f.Usage != tc.want {
				t.Fatalf("%s --chart-version help = %q, want %q", tc.name, f.Usage, tc.want)
			}
			var b strings.Builder
			fs.SetOutput(&b)
			fs.PrintDefaults()
			if !strings.Contains(b.String(), tc.want) {
				t.Fatalf("%s --help does not print %q:\n%s", tc.name, tc.want, b.String())
			}
		})
	}
}

func TestUpdateAndRollbackStillChooseTheirOwnRelease(t *testing.T) {
	var updateF *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { updateF = addUpdateFlags(fs) }, "--chart-version", "0.6.0")
	if got := updateF.options().Apply.ChartVersion; got != "0.6.0" {
		t.Fatalf("update's --chart-version reached its apply as %q, want 0.6.0", got)
	}

	var rollbackF *rollbackFlags
	parse(t, "rollback", func(fs *flag.FlagSet) { rollbackF = addRollbackFlags(fs) }, "--chart-version", "0.6.0")
	if got := rollbackF.options("sonarr").Apply.ChartVersion; got != "0.6.0" {
		t.Fatalf("rollback's --chart-version reached its apply as %q: the installed release it writes is internal/update's", got)
	}
}
