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

// callLog is one ordered record of everything a step did, across Helm, the
// cluster, the snapshotter and the apply. The order is part of the contract --
// stop, wait for the pods, copy, then apply -- so one log shared by the fakes
// is what lets a test assert it across three of them.
type callLog struct {
	calls []string
}

func (l *callLog) add(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

// indexFrom is the index of want at or after from, or -1.
func (l *callLog) indexFrom(from int, want string) int {
	for i := from; i < len(l.calls); i++ {
		if l.calls[i] == want {
			return i
		}
	}
	return -1
}

// fakeRelease is Helm's deployed release.
type fakeRelease struct {
	state release.ReleaseState
	// stateErr fails every read; stateErrFrom fails from that read on, so a
	// test can let the plan and the step's own read answer and fail only the
	// read box makes after a gate.
	stateErr     error
	stateErrFrom int
	calls        int
}

func (f *fakeRelease) State(context.Context) (release.ReleaseState, error) {
	f.calls++
	if f.stateErr != nil {
		return f.state, f.stateErr
	}
	if f.stateErrFrom > 0 && f.calls >= f.stateErrFrom {
		return release.ReleaseState{}, errors.New("helm: the API server did not answer")
	}
	return f.state, nil
}

// fakeCluster is the release's StatefulSets. Scaling to zero empties the app's
// pods, which is what the settle wait looks for. scaleErrs fails one replica
// count, so a test can fail the stop and still have the restart work.
type fakeCluster struct {
	replicas  map[string]int32
	pods      map[string]int
	healthy   bool
	neverGone bool
	scaleErrs map[int32]error
	healthErr error
	log       *callLog
}

func (f *fakeCluster) Scale(_ context.Context, app string, replicas int32) error {
	f.log.add("scale %s=%d", app, replicas)
	if err := f.scaleErrs[replicas]; err != nil {
		return err
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
	f.log.add("gone %s", app)
	if f.neverGone {
		return false, nil
	}
	return f.pods[app] == 0, nil
}

func (f *fakeCluster) Healthy(context.Context) (bool, error) {
	f.log.add("healthy")
	return f.healthy, f.healthErr
}

// fakeSnapshots records what was asked, copied and pruned.
type fakeSnapshots struct {
	checks   []snapshotCall
	takes    []snapshotCall
	prunes   []snapshotCall
	checkErr error
	takeErr  error
	log      *callLog
}

type snapshotCall struct {
	step Step
	apps []string
}

func (f *fakeSnapshots) Check(_ context.Context, step Step, apps []string) error {
	f.log.add("check %s -> %s", step.From, step.To)
	f.checks = append(f.checks, snapshotCall{step: step, apps: apps})
	return f.checkErr
}

func (f *fakeSnapshots) Take(_ context.Context, step Step, apps []string) error {
	f.log.add("take %s -> %s", step.From, step.To)
	f.takes = append(f.takes, snapshotCall{step: step, apps: apps})
	return f.takeErr
}

func (f *fakeSnapshots) Prune(_ context.Context, step Step, apps []string) error {
	f.log.add("prune %s -> %s", step.From, step.To)
	f.prunes = append(f.prunes, snapshotCall{step: step, apps: apps})
	return nil
}

// fakeApply stands in for release.Applier.Apply. It moves the fake release's
// state as a real apply would, so the next step sees the revision and version
// that really happened.
type fakeApply struct {
	calls   []release.Options
	errFor  map[string]error
	applied map[string]bool
	managed *fakeRelease
	nextRev int
	log     *callLog
}

func (f *fakeApply) apply(_ context.Context, opts release.Options) (release.Result, error) {
	f.log.add("apply %s", opts.ChartVersion)
	f.calls = append(f.calls, opts)
	version := opts.ChartVersion
	applied := f.applied[version]
	if applied || f.errFor[version] == nil {
		f.nextRev++
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
	log     *callLog
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
		log:    &callLog{},
		out:    &bytes.Buffer{},
		now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		grant:  "testdata/grant.yaml",
		values: "testdata/values.yaml",
	}
	h.charts = &fakeCharts{values: values, schema: schema, charts: map[string]release.Chart{}}
	h.rel = &fakeRelease{
		state: release.ReleaseState{Exists: true, ChartVersion: "0.4.0", Revision: 3, Status: "deployed"},
	}
	h.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: h.log}
	h.snaps = &fakeSnapshots{log: h.log}
	h.state = &fakeState{}
	h.app = &fakeApply{errFor: map[string]error{}, applied: map[string]bool{}, managed: h.rel, log: h.log, nextRev: 3}
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

// assertOrder fails unless every call appears, in the order given, in the one
// log the cluster, the snapshotter and the apply share.
func (h *harness) assertOrder(t *testing.T, want ...string) {
	t.Helper()
	at := 0
	for _, name := range want {
		found := h.log.indexFrom(at, name)
		if found < 0 {
			t.Fatalf("calls = %v, want %q next", h.log.calls, name)
		}
		at = found + 1
	}
}

// scales is the scale calls in order, as "app=N".
func (h *harness) scales() []string {
	var out []string
	for _, call := range h.log.calls {
		if rest, ok := strings.CutPrefix(call, "scale "); ok {
			out = append(out, rest)
		}
	}
	return out
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
	if len(h.app.calls) != 0 || len(h.scales()) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("a current box wrote: applies=%d scales=%v snapshots=%d", len(h.app.calls), h.scales(), len(h.snaps.takes))
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
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0"}) {
		t.Fatalf("scales = %v, want sonarr stopped for the copy", h.scales())
	}
	wantTake := snapshotCall{step: Step{From: "0.4.0", To: "0.5.0"}, apps: []string{"sonarr"}}
	if !reflect.DeepEqual(h.snaps.takes, []snapshotCall{wantTake}) {
		t.Fatalf("takes = %+v, want %+v", h.snaps.takes, wantTake)
	}
	if !reflect.DeepEqual(h.snaps.prunes, []snapshotCall{wantTake}) {
		t.Fatalf("prunes = %+v, want the step's two kept", h.snaps.prunes)
	}
	// The copy is taken with no writer, and the release moves only after it
	// exists. Check comes first, before any app is stopped.
	h.assertOrder(t, "check 0.4.0 -> 0.5.0", "scale sonarr=0", "gone sonarr", "take 0.4.0 -> 0.5.0", "apply 0.5.0")
	if len(h.app.calls) != 1 || h.app.calls[0].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want one at 0.5.0", h.app.calls)
	}
	if h.app.calls[0].GrantPath != h.grant || h.app.calls[0].ValuesPath != h.values {
		t.Fatalf("the step applied something other than the operator's files: %+v", h.app.calls[0])
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeOK || rec.To != "0.5.0" {
		t.Fatalf("LastStep = %+v, want a passing step to 0.5.0", rec)
	}
	if rec.Revision != 3 {
		t.Fatalf("LastStep.Revision = %d, want the revision the step began at (3)", rec.Revision)
	}
	if !reflect.DeepEqual(rec.Moved, []string{"sonarr"}) || rec.Gate != "" || rec.Error != "" {
		t.Fatalf("LastStep = %+v, want the app it moved and no gate", rec)
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
	if len(h.scales()) != 0 || len(h.snaps.takes) != 0 || len(h.snaps.prunes) != 0 {
		t.Fatalf("an image-only release change snapshotted: scales=%v takes=%+v", h.scales(), h.snaps.takes)
	}
	if len(h.app.calls) != 1 {
		t.Fatalf("applies = %d, want the release still applied", len(h.app.calls))
	}
}

// TestAHeldAppIsNamedInThePlan: an app the operator pinned runs the same image
// on both sides of the step, so nothing of it moves and the plan says why
// instead of leaving it out.
func TestAHeldAppIsNamedInThePlan(t *testing.T) {
	h := newHarness(t)
	h.values = "testdata/pinned-values.yaml"
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %+v, want one", res.Steps)
	}
	if len(res.Steps[0].Moving) != 0 {
		t.Fatalf("moving = %v, want nothing: the pin holds sonarr at the digest it runs", res.Steps[0].Moving)
	}
	if !reflect.DeepEqual(res.Steps[0].Held, []string{"sonarr"}) {
		t.Fatalf("held = %v, want sonarr", res.Steps[0].Held)
	}
	if len(h.scales()) != 0 || len(h.snaps.takes) != 0 || len(h.snaps.prunes) != 0 {
		t.Fatalf("a held app was stopped or copied: scales=%v takes=%+v", h.scales(), h.snaps.takes)
	}
	if len(h.app.calls) != 1 || h.app.calls[0].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want the release still applied at 0.5.0", h.app.calls)
	}
	for _, want := range []string{
		"0.4.0 -> 0.5.0 (no image moves)",
		"held:",
		"sonarr: held at " + digestA + " by your values",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("plan does not print %q:\n%s", want, h.out)
		}
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

// TestAWiringGateFailureStopsTheUpdate: the release moved and the wiring
// receipt says it did not work. Box leaves the release where the step put it,
// leaves the apps as the new release left them, keeps the snapshot and records
// the failed release.
func TestAWiringGateFailureStopsTheUpdate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.applied["0.5.0"] = true
	h.app.errFor["0.5.0"] = errors.New("wiring receipt says failed")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed wiring gate reported success")
	}
	for _, want := range []string{
		"update s5n to 0.5.0: the wiring gate failed (wiring receipt says failed)",
		"s5n is on 0.5.0 (revision 4, deployed)",
		"and was not rolled back",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	// Nothing touches the apps after the apply: they are the new release's.
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0"}) {
		t.Fatalf("scales = %v, want nothing after the apply", h.scales())
	}
	if len(h.snaps.takes) != 1 {
		t.Fatalf("a failed step lost its snapshot: takes=%+v", h.snaps.takes)
	}
	if len(h.snaps.prunes) != 0 {
		t.Fatalf("a failed step pruned its snapshot: %v", h.snaps.prunes)
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeFailed || rec.To != "0.5.0" {
		t.Fatalf("LastStep = %+v, want a failed step to 0.5.0", rec)
	}
	if rec.Gate != gateWiring || rec.Revision != 3 || !reflect.DeepEqual(rec.Moved, []string{"sonarr"}) {
		t.Fatalf("LastStep = %+v, want the wiring gate, the revision it began at and the apps it moved", rec)
	}
	if !strings.Contains(rec.Error, "wiring gate failed") {
		t.Fatalf("LastStep.Error = %q, want the gate and its cause", rec.Error)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
	if h.rel.state.ChartVersion != "0.5.0" {
		t.Fatalf("release = %+v, want it left on 0.5.0", h.rel.state)
	}
}

// TestAHealthGateFailureStopsTheUpdate: the release deployed but never became
// healthy. Same answer as the wiring gate -- the new release stays, nothing is
// scaled, the snapshot is kept -- because a migration may be about to finish.
func TestAHealthGateFailureStopsTheUpdate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.cluster.healthy = false

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed health gate reported success")
	}
	for _, want := range []string{
		"update s5n to 0.5.0: the health gate failed",
		"s5n is on 0.5.0 (revision 4, deployed)",
		"and was not rolled back",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0"}) {
		t.Fatalf("scales = %v, want nothing after the apply", h.scales())
	}
	if len(h.snaps.takes) != 1 || len(h.snaps.prunes) != 0 {
		t.Fatalf("takes = %+v prunes = %+v, want the failed step's snapshot kept", h.snaps.takes, h.snaps.prunes)
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeFailed || rec.Gate != gateHealth {
		t.Fatalf("LastStep = %+v, want a failed step naming the health gate", rec)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
}

// TestAnApplyThatRefusedStartsTheAppsAgain: Helm never reported the upgrade
// done, so the old release is what runs -- with the apps this run stopped
// still down unless box starts them. The release is still recorded as failed
// on this box.
func TestAnApplyThatRefusedStartsTheAppsAgain(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errFor["0.5.0"] = errors.New("values file: apps.sonarr.port is not an integer")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a refused apply reported success")
	}
	for _, want := range []string{
		"update s5n to 0.5.0: the apply failed (values file: apps.sonarr.port is not an integer)",
		"s5n is on 0.4.0 (revision 3, deployed)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error %q says the release was rolled back", err)
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0", "sonarr=1"}) {
		t.Fatalf("scales = %v, want the app started again after a refusal", h.scales())
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeFailed || rec.Gate != gateApply {
		t.Fatalf("LastStep = %+v, want a failed step naming the apply gate", rec)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded so the timer does not retry it", h.state.state.Failed)
	}
}

// TestAnApplyThatRefusedNamesARestartThatFailed: the restart is part of the
// error, not a second thing box shrugs at. The app is left down and the
// operator is told.
func TestAnApplyThatRefusedNamesARestartThatFailed(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errFor["0.5.0"] = errors.New("values file: apps.sonarr.port is not an integer")
	h.cluster.scaleErrs = map[int32]error{1: errors.New("forbidden")}

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a refused apply reported success")
	}
	if !strings.Contains(err.Error(), "; starting sonarr again failed: sonarr: forbidden") {
		t.Fatalf("error %q does not name the restart that failed", err)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release still recorded", h.state.state.Failed)
	}
}

// TestAFailedStopStartsTheAppsAgainAndRecordsNothing: the release was never
// tried, so this is not a failed release -- only a stopped app that box starts
// again.
func TestAFailedStopStartsTheAppsAgainAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.cluster.scaleErrs = map[int32]error{0: errors.New("forbidden")}

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "stopping sonarr: sonarr: forbidden") {
		t.Fatalf("error = %v, want the stop named", err)
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0", "sonarr=1"}) {
		t.Fatalf("scales = %v, want the app started again", h.scales())
	}
	if len(h.app.calls) != 0 {
		t.Fatalf("applied %d times after a failed stop", len(h.app.calls))
	}
	if h.state.state.LastStep != nil || len(h.state.state.Failed) != 0 {
		t.Fatalf("a step that never reached the release was recorded: %+v", h.state.state)
	}
}

// TestPodsThatNeverGoStartTheAppsAgain: the settle wait can time out with the
// apps still stopping. Nothing has been applied, so nothing is a failed
// release, and box puts back the replicas it took down.
func TestPodsThatNeverGoStartTheAppsAgain(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.cluster.neverGone = true

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "waiting for sonarr to stop") {
		t.Fatalf("error = %v, want the settle wait named", err)
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0", "sonarr=1"}) {
		t.Fatalf("scales = %v, want the app started again", h.scales())
	}
	if len(h.app.calls) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("applied or copied with the pods still up: applies=%d takes=%+v", len(h.app.calls), h.snaps.takes)
	}
	if h.state.state.LastStep != nil || len(h.state.state.Failed) != 0 {
		t.Fatalf("a step that never reached the release was recorded: %+v", h.state.state)
	}
}

// TestASnapshotterThatCannotCopyStopsBeforeAnythingStops: box asks whether the
// copy can be taken first, so a refusal leaves every app running and records
// nothing -- the box refused, not the release.
func TestASnapshotterThatCannotCopyStopsBeforeAnythingStops(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.checkErr = errors.New("no space left for a copy")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a box that cannot copy reported success")
	}
	if want := "update s5n to 0.5.0: sonarr cannot be snapshotted: no space left for a copy"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not say %q", err, want)
	}
	if len(h.scales()) != 0 {
		t.Fatalf("scales = %v, want nothing stopped before the copy was refused", h.scales())
	}
	if len(h.snaps.takes) != 0 || len(h.app.calls) != 0 {
		t.Fatalf("took a copy or applied after the refusal: takes=%+v applies=%d", h.snaps.takes, len(h.app.calls))
	}
	if h.state.state.LastStep != nil || len(h.state.state.Failed) != 0 {
		t.Fatalf("a refusal was recorded as a failed release: %+v", h.state.state)
	}
}

// TestAFailedSnapshotStartsTheAppsAgain: the copy failed, which is box's
// refusal rather than a release that did not work. The apps go back up and
// nothing is recorded as failed.
func TestAFailedSnapshotStartsTheAppsAgain(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.takeErr = errors.New("no space left")

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "snapshotting sonarr: no space left") {
		t.Fatalf("error = %v, want the copy named", err)
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0", "sonarr=1"}) {
		t.Fatalf("scales = %v, want the app started again", h.scales())
	}
	if len(h.app.calls) != 0 {
		t.Fatalf("applied %d times without a copy", len(h.app.calls))
	}
	if h.state.state.LastStep != nil || len(h.state.state.Failed) != 0 {
		t.Fatalf("a step with no copy was recorded as a failed release: %+v", h.state.state)
	}
}

// TestAFailedGateThatCannotReadTheStateSaysSo: the sentence is Helm's answer
// read now. When Helm cannot answer, box says that instead of naming a
// release it would be guessing at.
func TestAFailedGateThatCannotReadTheStateSaysSo(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.applied["0.5.0"] = true
	h.app.errFor["0.5.0"] = errors.New("wiring receipt says failed")
	// The plan reads once, the step reads once, and the read after the gate
	// is the third.
	h.rel.stateErrFrom = 3

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed wiring gate reported success")
	}
	if !strings.Contains(err.Error(), "the release's state could not be read (helm: the API server did not answer)") {
		t.Fatalf("error %q does not say the state could not be read", err)
	}
	if strings.Contains(err.Error(), "is on 0.5.0") {
		t.Fatalf("error %q names a release box did not read", err)
	}
	if !strings.Contains(err.Error(), "and was not rolled back") {
		t.Fatalf("error %q does not say the release stays", err)
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
	if len(h.app.calls) != 0 || len(h.scales()) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("--check wrote to the cluster/copy: applies=%d scales=%v takes=%v", len(h.app.calls), h.scales(), h.snaps.takes)
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
	if len(h.app.calls) != 0 || len(h.scales()) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("a declined update wrote: applies=%d scales=%v", len(h.app.calls), h.scales())
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
// over a migrated database cannot be put back without the copy, so box says so
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
	if h.confirm.asks != 0 || len(h.scales()) != 0 || len(h.app.calls) != 0 {
		t.Fatalf("refused after asking=%d or writing: scales=%v applies=%d", h.confirm.asks, h.scales(), len(h.app.calls))
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
