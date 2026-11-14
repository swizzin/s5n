package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// fakeCharts is the repository: one chart per version, with the two
// annotations an update reads. Load only serves a version the test published,
// so a step that names a release the repository does not hold fails the same
// way it would in the registry.
type fakeCharts struct {
	values   []byte
	schema   []byte
	charts   map[string]release.Chart
	releases []release.Published
	err      error
	loads    []string
}

func (f *fakeCharts) Load(_ context.Context, ref release.ChartRef) (release.Chart, error) {
	f.loads = append(f.loads, ref.Version)
	if f.err != nil {
		return release.Chart{}, f.err
	}
	chart, ok := f.charts[ref.Version]
	if !ok {
		return release.Chart{}, fmt.Errorf("no published release %s", ref.Version)
	}
	return chart, nil
}

func (f *fakeCharts) Releases(context.Context, release.ChartRef) ([]release.Published, error) {
	return f.releases, f.err
}

// publish adds one release. The chart's variant digests are what update
// compares; sonarrDigest is the one tests move.
func (f *fakeCharts) publish(version, minBox, upgradeFrom, sonarrDigest string) {
	f.charts[version] = release.Chart{
		Name:       "s5n",
		Version:    version,
		AppVersion: version,
		Annotations: map[string]string{
			release.MinBoxAnnotation:      minBox,
			release.UpgradeFromAnnotation: upgradeFrom,
		},
		Values:   f.values,
		Schema:   f.schema,
		Variants: variants(sonarrDigest),
	}
	f.releases = append(f.releases, release.Published{Version: version, MinBox: minBox, UpgradeFrom: upgradeFrom})
}

// fakeRelease is Helm's deployed release. A rollback returns it to the version
// the revision belonged to, which is what Helm does with the stored manifest.
type fakeRelease struct {
	state       release.ReleaseState
	stateErr    error
	versions    map[int]string
	rollbacks   []int
	rollbackErr error
}

func (f *fakeRelease) State(context.Context) (release.ReleaseState, error) {
	return f.state, f.stateErr
}

func (f *fakeRelease) Rollback(_ context.Context, revision int) error {
	f.rollbacks = append(f.rollbacks, revision)
	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	f.state.Revision = revision
	f.state.Status = "deployed"
	if version, ok := f.versions[revision]; ok {
		f.state.ChartVersion = version
	}
	return nil
}

// fakeCluster is the release's StatefulSets. Scaling to zero empties the app's
// pods, which is what the settle wait looks for.
type fakeCluster struct {
	replicas   map[string]int32
	pods       map[string]int
	scales     []string
	healthy    bool
	scaleErr   error
	healthyErr error
}

func (f *fakeCluster) Scale(_ context.Context, app string, replicas int32) error {
	f.scales = append(f.scales, fmt.Sprintf("%s=%d", app, replicas))
	if f.scaleErr != nil {
		return f.scaleErr
	}
	f.replicas[app] = replicas
	if replicas == 0 {
		f.pods[app] = 0
	} else {
		f.pods[app] = 1
	}
	return nil
}

func (f *fakeCluster) Gone(_ context.Context, app string) (bool, error) {
	return f.pods[app] == 0, nil
}

func (f *fakeCluster) Healthy(context.Context) (bool, error) {
	return f.healthy, f.healthyErr
}

// fakeSnapshots records what was copied, put back and pruned.
type fakeSnapshots struct {
	takes      []snapshotCall
	restores   []Step
	prunes     []snapshotCall
	takeErr    error
	restoreErr error
}

type snapshotCall struct {
	step Step
	apps []string
}

func (f *fakeSnapshots) Take(_ context.Context, step Step, apps []string) error {
	f.takes = append(f.takes, snapshotCall{step: step, apps: apps})
	return f.takeErr
}

func (f *fakeSnapshots) Restore(_ context.Context, step Step) error {
	f.restores = append(f.restores, step)
	return f.restoreErr
}

func (f *fakeSnapshots) Prune(_ context.Context, step Step, apps []string) error {
	f.prunes = append(f.prunes, snapshotCall{step: step, apps: apps})
	return nil
}

// fakeApply stands in for release.Applier.Apply. It moves the fake release's
// state as a real apply would, so the next step and the rollback see the
// revision and version that really happened.
type fakeApply struct {
	calls   []release.Options
	errFor  map[string]error
	applied map[string]bool
	managed *fakeRelease
	nextRev int
}

func (f *fakeApply) apply(_ context.Context, opts release.Options) (release.Result, error) {
	f.calls = append(f.calls, opts)
	version := opts.ChartVersion
	applied := f.applied[version]
	if applied || f.errFor[version] == nil {
		f.nextRev++
		f.managed.versions[f.nextRev] = version
		f.managed.state = release.ReleaseState{
			Exists:       true,
			ChartVersion: version,
			Revision:     f.nextRev,
			Status:       "deployed",
		}
	}
	if err := f.errFor[version]; err != nil {
		return release.Result{Applied: applied, Revision: f.nextRev}, err
	}
	return release.Result{Applied: true, Revision: f.nextRev}, nil
}

// fakeState is update.json in memory.
type fakeState struct {
	state   State
	loads   int
	saves   int
	saveErr error
}

func (f *fakeState) Load() (State, error) {
	f.loads++
	return f.state, nil
}

func (f *fakeState) Save(state State) error {
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.state = state
	return nil
}

// fakeConfirm answers the one question and counts it.
type fakeConfirm struct {
	answer bool
	err    error
	asks   int
}

func (f *fakeConfirm) Ask(string) (bool, error) {
	f.asks++
	return f.answer, f.err
}

// harness is one update wired to fakes, with a box on 0.4.0 at revision 3.
type harness struct {
	u       *Updater
	charts  *fakeCharts
	rel     *fakeRelease
	cluster *fakeCluster
	snaps   *fakeSnapshots
	state   *fakeState
	app     *fakeApply
	confirm *fakeConfirm
	out     *bytes.Buffer
	now     time.Time
	grant   string
	values  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	values, err := os.ReadFile("testdata/chart-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/chart-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		out:    &bytes.Buffer{},
		now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		grant:  "testdata/grant.yaml",
		values: "testdata/values.yaml",
	}
	h.charts = &fakeCharts{values: values, schema: schema, charts: map[string]release.Chart{}}
	h.rel = &fakeRelease{
		state:    release.ReleaseState{Exists: true, ChartVersion: "0.4.0", Revision: 3, Status: "deployed"},
		versions: map[int]string{3: "0.4.0"},
	}
	h.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true}
	h.snaps = &fakeSnapshots{}
	h.state = &fakeState{}
	h.app = &fakeApply{errFor: map[string]error{}, applied: map[string]bool{}, managed: h.rel}
	h.confirm = &fakeConfirm{answer: true}
	h.u = &Updater{
		Charts:        h.charts,
		Open:          func(string) (Release, error) { return h.rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return h.cluster, nil },
		OpenSnapshots: func(string) (Snapshotter, error) { return h.snaps, nil },
		Apply:         h.app.apply,
		Confirm:       h.confirm,
		State:         h.state,
		BoxVersion:    "0.4.0",
		Say:           &say.Plain{W: h.out},
		HealthWait:    50 * time.Millisecond,
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
		Now:           func() time.Time { return h.now },
	}
	return h
}

func (h *harness) run(t *testing.T, check bool) (Result, error) {
	t.Helper()
	return h.u.Run(context.Background(), Options{
		Apply: release.Options{GrantPath: h.grant, ValuesPath: h.values},
		Check: check,
	})
}

func TestAlreadyCurrentWritesOnlyTheCheckTime(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !res.Current || res.Installed != "0.4.0" || res.Target != "0.4.0" {
		t.Fatalf("result = %+v, want the box already current", res)
	}
	if len(h.app.calls) != 0 || len(h.cluster.scales) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("a current box wrote: applies=%d scales=%v snapshots=%d", len(h.app.calls), h.cluster.scales, len(h.snaps.takes))
	}
	if h.confirm.asks != 0 {
		t.Fatalf("asked %d questions on a current box", h.confirm.asks)
	}
	if !h.state.state.LastCheck.Equal(h.now) {
		t.Fatalf("LastCheck = %v, want %v", h.state.state.LastCheck, h.now)
	}
	if !strings.HasPrefix(h.out.String(), "s5n 0.4.0 in testbox is current") {
		t.Fatalf("first line does not answer:\n%s", h.out)
	}
}

func TestOneStepSnapshotsOnlyTheAppWhoseImageMoved(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %+v, want one", res.Steps)
	}
	if !reflect.DeepEqual(res.Steps[0].Moving, []string{"sonarr"}) {
		t.Fatalf("moving = %v, want only sonarr (its digest changed)", res.Steps[0].Moving)
	}
	if !reflect.DeepEqual(h.cluster.scales, []string{"sonarr=0"}) {
		t.Fatalf("scales = %v, want sonarr stopped for the copy", h.cluster.scales)
	}
	wantTake := snapshotCall{step: Step{From: "0.4.0", To: "0.5.0"}, apps: []string{"sonarr"}}
	if !reflect.DeepEqual(h.snaps.takes, []snapshotCall{wantTake}) {
		t.Fatalf("takes = %+v, want %+v", h.snaps.takes, wantTake)
	}
	if !reflect.DeepEqual(h.snaps.prunes, []snapshotCall{wantTake}) {
		t.Fatalf("prunes = %+v, want the step's two kept", h.snaps.prunes)
	}
	if len(h.snaps.restores) != 0 {
		t.Fatalf("a passing step restored %v", h.snaps.restores)
	}
	if len(h.app.calls) != 1 || h.app.calls[0].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want one at 0.5.0", h.app.calls)
	}
	if h.app.calls[0].GrantPath != h.grant || h.app.calls[0].ValuesPath != h.values {
		t.Fatalf("the step applied something other than the operator's files: %+v", h.app.calls[0])
	}
	if h.state.state.LastStep == nil || h.state.state.LastStep.Outcome != OutcomeOK || h.state.state.LastStep.To != "0.5.0" {
		t.Fatalf("LastStep = %+v, want a passing step to 0.5.0", h.state.state.LastStep)
	}
	if !strings.Contains(h.out.String(), "s5n 0.4.0 -> 0.5.0 in testbox: 1 step") {
		t.Fatalf("plan line missing:\n%s", h.out)
	}
}

func TestAStepThatMovesNoImageTakesNoSnapshot(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestA)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Moving) != 0 {
		t.Fatalf("steps = %+v, want one moving nothing", res.Steps)
	}
	if len(h.cluster.scales) != 0 || len(h.snaps.takes) != 0 || len(h.snaps.prunes) != 0 {
		t.Fatalf("an image-only release change snapshotted: scales=%v takes=%+v", h.cluster.scales, h.snaps.takes)
	}
	if len(h.app.calls) != 1 {
		t.Fatalf("applies = %d, want the release still applied", len(h.app.calls))
	}
}

func TestTwoStepsWhenTheTargetCannotFollowTheInstalledRelease(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.charts.publish("0.6.0", "0.4.0", "0.5.0", digestC)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	want := []Step{{From: "0.4.0", To: "0.5.0"}, {From: "0.5.0", To: "0.6.0"}}
	if !reflect.DeepEqual(res.Taken, want) {
		t.Fatalf("taken = %+v, want %+v", res.Taken, want)
	}
	if len(h.app.calls) != 2 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[1].ChartVersion != "0.6.0" {
		t.Fatalf("applies = %+v, want one per step in order", h.app.calls)
	}
	if len(h.snaps.takes) != 2 {
		t.Fatalf("takes = %+v, want one per step that moves an image", h.snaps.takes)
	}
	if h.rel.state.ChartVersion != "0.6.0" {
		t.Fatalf("final version = %q, want 0.6.0", h.rel.state.ChartVersion)
	}
	if h.state.state.LastStep == nil || h.state.state.LastStep.To != "0.6.0" || h.state.state.LastStep.Outcome != OutcomeOK {
		t.Fatalf("LastStep = %+v, want the last step to 0.6.0", h.state.state.LastStep)
	}
}

func TestAFailedWiringGateRestoresAndRollsBack(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.applied["0.5.0"] = true
	h.app.errFor["0.5.0"] = errors.New("wiring receipt says failed")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed wiring gate reported success")
	}
	for _, want := range []string{"to 0.5.0", "wiring gate failed", "back on 0.4.0", "revision 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if !reflect.DeepEqual(h.rel.rollbacks, []int{3}) {
		t.Fatalf("rollbacks = %v, want the revision the step began at", h.rel.rollbacks)
	}
	wantStep := Step{From: "0.4.0", To: "0.5.0"}
	if !reflect.DeepEqual(h.snaps.restores, []Step{wantStep}) {
		t.Fatalf("restores = %v, want %v", h.snaps.restores, wantStep)
	}
	if len(h.snaps.prunes) != 0 {
		t.Fatalf("a failed step pruned its snapshot: %v", h.snaps.prunes)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
	if h.state.state.LastStep == nil || h.state.state.LastStep.Outcome != OutcomeFailed || h.state.state.LastStep.Error == "" {
		t.Fatalf("LastStep = %+v, want a failed step with its reason", h.state.state.LastStep)
	}
	if h.rel.state.ChartVersion != "0.4.0" || h.rel.state.Revision != 3 {
		t.Fatalf("release = %+v, want it back on 0.4.0 at revision 3", h.rel.state)
	}
}

func TestApplyRefusedBeforeHelmStartsTheAppsAgain(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errFor["0.5.0"] = errors.New("values file: apps.sonarr.port is not an integer")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a refused apply reported success")
	}
	if !strings.Contains(err.Error(), "apply gate failed") {
		t.Fatalf("error = %v, want the apply gate named", err)
	}
	if len(h.rel.rollbacks) != 0 {
		t.Fatalf("rolled back %v although Helm never moved", h.rel.rollbacks)
	}
	if got := h.cluster.scales[len(h.cluster.scales)-1]; got != "sonarr=1" {
		t.Fatalf("scales = %v, want the app started again after a refusal", h.cluster.scales)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded so the timer does not retry it", h.state.state.Failed)
	}
}

func TestTheNextRunSkipsAFailedRelease(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.state.state.Failed = []string{"0.5.0"}

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !res.Current {
		t.Fatalf("result = %+v, want the box left current", res)
	}
	if !reflect.DeepEqual(res.Skipped, []string{"0.5.0"}) {
		t.Fatalf("Skipped = %v, want the release that failed", res.Skipped)
	}
	if !strings.Contains(h.out.String(), "0.5.0 failed to update on this box before") {
		t.Fatalf("the skipped release is not named:\n%s", h.out)
	}
	if len(h.app.calls) != 0 {
		t.Fatalf("retried a release it had already failed on")
	}
}

func TestANewerReleaseClearsTheFailure(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.charts.publish("0.6.0", "0.4.0", "0.4.0", digestC)
	h.state.state.Failed = []string{"0.5.0"}

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if res.Target != "0.6.0" {
		t.Fatalf("target = %q, want the newer release 0.6.0", res.Target)
	}
	if len(h.state.state.Failed) != 0 {
		t.Fatalf("Failed = %v after a passing step past 0.5.0", h.state.state.Failed)
	}
}

func TestAFailedRollbackStops(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.applied["0.5.0"] = true
	h.app.errFor["0.5.0"] = errors.New("health never came back")
	h.rel.rollbackErr = errors.New("helm: another operation is in progress")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed rollback reported success")
	}
	for _, want := range []string{"putting the box back failed", "will not retry", "helm: another operation is in progress"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
	if len(h.rel.rollbacks) != 1 {
		t.Fatalf("rollbacks = %v, want exactly one attempt", h.rel.rollbacks)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
}

func TestAMinBoxSkipIsNamed(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.7.0", "9.0.0", "0.4.0", digestB)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !res.Current {
		t.Fatalf("result = %+v, want the box left on 0.4.0", res)
	}
	if len(res.Skips) != 1 || res.Skips[0].Version != "0.7.0" {
		t.Fatalf("Skips = %+v, want 0.7.0", res.Skips)
	}
	if !strings.Contains(h.out.String(), "s5n 0.7.0 is out and needs a newer box") {
		t.Fatalf("the skipped release is not named:\n%s", h.out)
	}
}

func TestCheckWritesOnlyTheCheckTime(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	last := StepRecord{From: "0.3.0", To: "0.4.0", At: time.Unix(1, 0), Outcome: OutcomeOK}
	h.state.state = State{LastStep: &last, Failed: []string{"0.3.5"}}

	res, err := h.run(t, true)
	if err != nil {
		t.Fatalf("update --check: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 || !reflect.DeepEqual(res.Steps[0].Moving, []string{"sonarr"}) {
		t.Fatalf("--check planned %+v, want the step and its moving app", res.Steps)
	}
	if len(h.app.calls) != 0 || len(h.cluster.scales) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("--check wrote to the cluster/copy: applies=%d scales=%v takes=%v", len(h.app.calls), h.cluster.scales, h.snaps.takes)
	}
	if h.confirm.asks != 0 {
		t.Fatalf("--check asked a question")
	}
	if h.state.saves != 1 {
		t.Fatalf("saves = %d, want just lastCheck", h.state.saves)
	}
	if !h.state.state.LastCheck.Equal(h.now) {
		t.Fatalf("LastCheck = %v, want %v", h.state.state.LastCheck, h.now)
	}
	if h.state.state.LastStep == nil || !reflect.DeepEqual(*h.state.state.LastStep, last) {
		t.Fatalf("LastStep = %+v, --check rewrote it", h.state.state.LastStep)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.3.5"}) {
		t.Fatalf("Failed = %v, --check rewrote it", h.state.state.Failed)
	}
	if !strings.Contains(h.out.String(), "--check: nothing written but the check time") {
		t.Fatalf("--check does not say it wrote nothing:\n%s", h.out)
	}
}

func TestADeclinedUpdateWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.confirm.answer = false

	_, err := h.run(t, false)
	if !errors.Is(err, release.ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if len(h.app.calls) != 0 || len(h.cluster.scales) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("a declined update wrote: applies=%d scales=%v", len(h.app.calls), h.cluster.scales)
	}
	if h.state.state.LastStep != nil || len(h.state.state.Failed) != 0 {
		t.Fatalf("a declined update recorded a step: %+v", h.state.state)
	}
}

func TestNoDeployedReleasePointsAtInit(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.rel.state = release.ReleaseState{}

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "box init") {
		t.Fatalf("error = %v, want a refusal pointing at box init", err)
	}
}

// TestABoxWithoutSnapshotSupportRefusesBeforeItStopsAnything: an image bump
// over a migrated database cannot be undone without the copy, so box says so
// before it asks, stops an app or writes anything.
func TestABoxWithoutSnapshotSupportRefusesBeforeItStopsAnything(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.u.OpenSnapshots = nil

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "snapshot support") {
		t.Fatalf("error = %v, want a refusal naming the missing snapshotter", err)
	}
	if h.confirm.asks != 0 || len(h.cluster.scales) != 0 || len(h.app.calls) != 0 {
		t.Fatalf("refused after asking=%d or writing: scales=%v applies=%d", h.confirm.asks, h.cluster.scales, len(h.app.calls))
	}
}

// TestAPinnedPrereleaseIsTheTarget: prereleases are not releases box takes
// unasked, so the listing leaves them out; naming one with --chart-version is
// the escape hatch, and the plan has to carry its floors even though it is not
// on the published list.
func TestAPinnedPrereleaseIsTheTarget(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0-rc.1", "0.4.0", "0.4.0", digestB)
	// What OCIChart.Releases really returns: no prereleases.
	h.charts.releases = h.charts.releases[:1]

	res, err := h.u.Run(context.Background(), Options{
		Apply: release.Options{GrantPath: h.grant, ValuesPath: h.values, ChartVersion: "0.5.0-rc.1"},
	})
	if err != nil {
		t.Fatalf("update to a pinned prerelease: %v\n%s", err, h.out)
	}
	if res.Target != "0.5.0-rc.1" || len(res.Steps) != 1 || res.Steps[0].To != "0.5.0-rc.1" {
		t.Fatalf("result = %+v, want one step to the pinned prerelease", res)
	}
	if len(h.app.calls) != 1 || h.app.calls[0].ChartVersion != "0.5.0-rc.1" {
		t.Fatalf("applies = %+v, want the prerelease installed", h.app.calls)
	}
}

func TestAChartVersionOlderThanTheBoxIsRefused(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.3.0", "0.3.0", "0.3.0", digestA)

	_, err := h.u.Run(context.Background(), Options{
		Apply: release.Options{GrantPath: h.grant, ValuesPath: h.values, ChartVersion: "0.3.0"},
	})
	if err == nil || !strings.Contains(err.Error(), "never moves a release backwards") {
		t.Fatalf("error = %v, want a refusal naming the direction", err)
	}
	if len(h.app.calls) != 0 {
		t.Fatalf("applied %d times for a backwards --chart-version", len(h.app.calls))
	}
}
