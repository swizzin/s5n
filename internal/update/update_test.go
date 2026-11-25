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

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/snapshot"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	digestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	digestE = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// fakeCharts is the repository: one chart per version, with the two
// annotations an update reads. Load only serves a version the test published,
// so a step that names a release the repository does not hold fails the same
// way it would in the registry.
type fakeCharts struct {
	values   []byte
	schema   []byte
	catalog  []byte
	charts   map[string]release.Chart
	releases []release.Published
	err      error
	loads    []string
	// floors is the floor each listing asked with, in order, so a test can
	// assert what update told the repository to start from.
	floors []string
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

// Releases is the listing with OCIChart's floor: nothing below it is published
// as far as the caller is concerned, and what is left is still whatever the
// test published, because the release list here is the test's own.
func (f *fakeCharts) Releases(_ context.Context, _ release.ChartRef, floor string) ([]release.Published, error) {
	f.floors = append(f.floors, floor)
	if f.err != nil {
		return nil, f.err
	}
	from, err := semver.NewVersion(strings.TrimSpace(floor))
	if floor == "" || err != nil {
		return f.releases, nil
	}
	var out []release.Published
	for _, r := range f.releases {
		v, err := semver.NewVersion(r.Version)
		if err != nil || v.LessThan(from) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// publish adds one release. The chart's variant digests are what update
// compares; sonarrDigest is the one tests move.
func (f *fakeCharts) publish(version, minBox, upgradeFrom, sonarrDigest string) {
	f.publishVariants(version, minBox, upgradeFrom, variants(sonarrDigest))
}

// publishVariants adds one release with an explicit variants.yaml, for the tests
// whose step moves more than one app.
func (f *fakeCharts) publishVariants(version, minBox, upgradeFrom string, variants []byte) {
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
		Catalog:  f.catalog,
		Variants: variants,
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

// fakeCluster is the release's StatefulSets and pods. Scaling to zero empties
// the app's pods, which is what the settle wait looks for. scaleErrs fails one
// replica count, so a test can fail the stop and still have the restart work.
// exitingFrom makes the gate see a crashloop only from that read on, so a test
// proves box polls for it rather than assuming it.
type fakeCluster struct {
	replicas map[string]int32
	pods     map[string]int
	healthy  bool
	exiting  []Exiting
	// restarts is each moved app's container restart count, read after a
	// failed apply to decide whether the step gets the health gate's watch.
	restarts map[string]int32
	// exitingFrom reports the crashloop only from that health read on, and
	// exitingUntil stops reporting it after that read, so a test can let box
	// roll the app back and pass the second gate.
	exitingFrom  int
	exitingUntil int
	healths      int
	moving       []string
	neverGone    bool
	scaleErrs    map[int32]error
	healthErr    error
	// running is what the cluster runs, for a read of each app's image.
	running    map[string]Running
	runningErr error
	log        *callLog
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

func (f *fakeCluster) Health(_ context.Context, moving []string) (Health, error) {
	f.log.add("health")
	f.healths++
	f.moving = moving
	if f.healthErr != nil {
		return Health{}, f.healthErr
	}
	if f.exitingFrom > 0 && f.healths < f.exitingFrom {
		return Health{Ready: f.healthy, Restarts: f.restarts}, nil
	}
	if f.exitingUntil > 0 && f.healths > f.exitingUntil {
		return Health{Ready: f.healthy, Restarts: f.restarts}, nil
	}
	return Health{Ready: f.healthy, Exiting: f.exiting, Restarts: f.restarts}, nil
}

// Running is the image each app runs, as the test wrote it: what the StatefulSet
// asks for and what the pods report.
func (f *fakeCluster) Running(context.Context) (map[string]Running, error) {
	f.log.add("running")
	if f.runningErr != nil {
		return nil, f.runningErr
	}
	return f.running, nil
}

// fakeSnapshots records what was asked, copied, restored and pruned.
type fakeSnapshots struct {
	checks     []snapshotCall
	takes      []snapshotCall
	restores   []snapshotCall
	prunes     []snapshotCall
	checkErr   error
	takeErr    error
	restoreErr error
	log        *callLog
	// taken is what Take answers: a step directory and one dir per app, so a
	// test can assert what a rollback records and says.
	taken Taken
}

type snapshotCall struct {
	step   Step
	apps   []string
	images map[string]ImageChange
}

func (f *fakeSnapshots) Check(_ context.Context, step Step, apps []string) error {
	f.log.add("check %s -> %s", step.From, step.To)
	f.checks = append(f.checks, snapshotCall{step: step, apps: apps})
	return f.checkErr
}

func (f *fakeSnapshots) Take(_ context.Context, step Step, apps []string, images map[string]ImageChange) (Taken, error) {
	f.log.add("take %s -> %s", step.From, step.To)
	f.takes = append(f.takes, snapshotCall{step: step, apps: apps, images: images})
	if f.takeErr != nil {
		return Taken{}, f.takeErr
	}
	return f.taken, nil
}

func (f *fakeSnapshots) Restore(_ context.Context, step Step, apps []string) error {
	f.log.add("restore %s -> %s", step.From, step.To)
	f.restores = append(f.restores, snapshotCall{step: step, apps: apps})
	return f.restoreErr
}

func (f *fakeSnapshots) Prune(_ context.Context, step Step, apps []string) error {
	f.log.add("prune %s -> %s", step.From, step.To)
	f.prunes = append(f.prunes, snapshotCall{step: step, apps: apps})
	return nil
}

// fakeApply stands in for release.Applier.Apply. It moves the fake release's
// state as a real apply would, so the next step sees the revision and version
// that really happened. errSeq fails the next applies of a version, one error per
// call, so a test can fail the step's apply and let the rollback's re-apply
// succeed.
type fakeApply struct {
	calls  []release.Options
	errFor map[string]error
	errSeq map[string][]error
	// statusSeq is the release status each apply leaves, one per call; a
	// missing entry leaves the release deployed. A test uses it to hold Helm on
	// "failed" while the gate reads the apps.
	statusSeq []string
	applied   map[string]bool
	managed   *fakeRelease
	nextRev   int
	log       *callLog
	// receipt is what a hooks-on apply reports as its wiring receipt; a zero
	// one is an apply whose chart ran no wiring.
	receipt release.Receipt
}

func (f *fakeApply) apply(_ context.Context, opts release.Options) (release.Result, error) {
	// The log is where the step's order is read: the apply that moves the
	// release runs without the chart's hooks, and the one that follows the
	// gate runs them.
	entry := "apply " + opts.ChartVersion
	if opts.SkipHooks {
		entry += " (no hooks)"
	}
	f.log.add("%s", entry)
	f.calls = append(f.calls, opts)
	version := opts.ChartVersion
	err := f.errFor[version]
	if seq := f.errSeq[version]; len(seq) > 0 {
		err = seq[0]
		f.errSeq[version] = seq[1:]
	}
	status := "deployed"
	if len(f.statusSeq) > 0 {
		status = f.statusSeq[0]
		f.statusSeq = f.statusSeq[1:]
	}
	applied := f.applied[version]
	if applied || err == nil {
		f.nextRev++
		f.managed.state = release.ReleaseState{
			Exists:       true,
			ChartVersion: version,
			Revision:     f.nextRev,
			Status:       status,
		}
	}
	if err != nil {
		return release.Result{Applied: applied, Revision: f.nextRev}, err
	}
	res := release.Result{Applied: true, Revision: f.nextRev}
	if !opts.SkipHooks {
		res.Receipt = f.receipt
	}
	return res, nil
}

// fakeHolds is holds.json in memory: the plan reads it and a step that rolls an
// app back writes it, so a test asserts both the file's content and the order
// the write took in the step.
type fakeHolds struct {
	holds []snapshot.Hold
	root  string
	saves int
	log   *callLog
}

func (f *fakeHolds) Load(root string) ([]snapshot.Hold, error) {
	f.root = root
	f.log.add("holds read")
	return f.holds, nil
}

func (f *fakeHolds) Save(root string, holds []snapshot.Hold) error {
	f.log.add("holds write")
	f.root = root
	f.holds = holds
	f.saves++
	return nil
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
	// questions are the words asked, in order, so a test can assert box's
	// sentence rather than only that something was asked.
	questions []string
}

func (f *fakeConfirm) Ask(question string) (bool, error) {
	f.asks++
	f.questions = append(f.questions, question)
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
	holds   *fakeHolds
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
	catalog, err := os.ReadFile("testdata/catalog.yaml")
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
	h.charts = &fakeCharts{values: values, schema: schema, catalog: catalog, charts: map[string]release.Chart{}}
	h.rel = &fakeRelease{
		state: release.ReleaseState{Exists: true, ChartVersion: "0.4.0", Revision: 3, Status: "deployed"},
	}
	h.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: h.log}
	h.snaps = &fakeSnapshots{log: h.log}
	h.state = &fakeState{}
	h.holds = &fakeHolds{log: h.log}
	h.app = &fakeApply{errFor: map[string]error{}, applied: map[string]bool{}, managed: h.rel, log: h.log, nextRev: 3}
	h.confirm = &fakeConfirm{answer: true}
	h.u = &Updater{
		Charts:        h.charts,
		Open:          func(string) (Release, error) { return h.rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return h.cluster, nil },
		OpenSnapshots: func(string, map[string]any) (Snapshotter, error) { return h.snaps, nil },
		Apply:         h.app.apply,
		Confirm:       h.confirm,
		State:         h.state,
		Holds:         h.holds,
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
	if !strings.HasPrefix(h.out.String(), "· s5n 0.4.0 is current") {
		t.Fatalf("first line does not answer:\n%s", h.out)
	}
}

// TestUpdateAsksTheRepositoryFromTheInstalledReleaseUp: an update only steps
// forward, so the listing it plans with starts at the release Helm reports as
// deployed. Everything below it is history box never reads.
func TestUpdateAsksTheRepositoryFromTheInstalledReleaseUp(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(h.charts.floors, []string{"0.4.0"}) {
		t.Fatalf("the repository was listed from %v, want the installed release 0.4.0", h.charts.floors)
	}
}

// TestABoxAheadOfEveryPublishedReleaseIsCurrent: an empty listing is not an
// error. A box on a release newer than anything published -- a hand-installed
// prerelease, or the newest release with everything older behind the floor --
// has nothing to do, and says so.
func TestABoxAheadOfEveryPublishedReleaseIsCurrent(t *testing.T) {
	h := newHarness(t)
	h.rel.state.ChartVersion = "0.5.0-rc.1"
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0-rc.1", "0.4.0", "0.4.0", digestB)
	// What OCIChart.Releases really returns: no prereleases. Everything that is
	// left is below the release the box is on.
	h.charts.releases = h.charts.releases[:1]

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update on a box ahead of every published release: %v\n%s", err, h.out)
	}
	if !res.Current || res.Installed != "0.5.0-rc.1" || res.Target != "0.5.0-rc.1" {
		t.Fatalf("result = %+v, want the box already current", res)
	}
	if !strings.HasPrefix(h.out.String(), "· s5n 0.5.0-rc.1 is current") {
		t.Fatalf("first line does not answer:\n%s", h.out)
	}
	if h.confirm.asks != 0 || len(h.app.calls) != 0 || len(h.scales()) != 0 {
		t.Fatalf("a current box wrote: asks=%d applies=%d scales=%v", h.confirm.asks, len(h.app.calls), h.scales())
	}
	if !h.state.state.LastCheck.Equal(h.now) {
		t.Fatalf("LastCheck = %v, want the check this run made (%v)", h.state.state.LastCheck, h.now)
	}
}

// TestAPlanThatFailsBeforeChoosingATargetRecordsNothing: the record names a
// release box tried and failed on. A listing that could not be read is not a
// release, so nothing is recorded for the next run and the timer to pass over.
func TestAPlanThatFailsBeforeChoosingATargetRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.err = errors.New("cannot list the releases at oci://registry.example.com/charts/s5n: the registry did not answer")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatalf("update: want the listing's failure\n%s", h.out)
	}
	if len(h.state.state.Failed) != 0 {
		t.Fatalf("Failed = %v, want nothing recorded for a plan that chose no target", h.state.state.Failed)
	}
	if h.state.saves != 0 {
		t.Fatalf("a plan that chose no target wrote update.json %d times", h.state.saves)
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
	wantTake := snapshotCall{
		step: Step{From: "0.4.0", To: "0.5.0"},
		apps: []string{"sonarr"},
		images: map[string]ImageChange{"sonarr": {
			Ran:      snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestA},
			MovedTo:  snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestB},
			Snapshot: true,
		}},
	}
	if !reflect.DeepEqual(h.snaps.takes, []snapshotCall{wantTake}) {
		t.Fatalf("takes = %+v, want %+v", h.snaps.takes, wantTake)
	}
	wantPrune := snapshotCall{step: wantTake.step, apps: wantTake.apps}
	if !reflect.DeepEqual(h.snaps.prunes, []snapshotCall{wantPrune}) {
		t.Fatalf("prunes = %+v, want the step's two kept", h.snaps.prunes)
	}
	// The copy is taken with no writer, and the release moves only after it
	// exists. Check comes first, before any app is stopped; the release moves
	// without the chart's hooks, the gate reads the apps, and the hooks run
	// once, after it.
	h.assertOrder(t, "check 0.4.0 -> 0.5.0", "scale sonarr=0", "gone sonarr", "take 0.4.0 -> 0.5.0",
		"apply 0.5.0 (no hooks)", "health", "apply 0.5.0")
	if len(h.app.calls) != 2 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[1].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want 0.5.0 without hooks and then with them", h.app.calls)
	}
	if !h.app.calls[0].SkipHooks || h.app.calls[1].SkipHooks {
		t.Fatalf("applies = %+v, want the release moved without its hooks and the state that stays applied with them", h.app.calls)
	}
	if !h.app.calls[1].RunHooks {
		t.Fatalf("applies = %+v, want the closing apply to install for its hooks", h.app.calls)
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
	if !strings.Contains(h.out.String(), "s5n 0.4.0 → 0.5.0 · 1 step · sonarr moves") {
		t.Fatalf("plan line missing:\n%s", h.out)
	}
}

// TestAnAppWithNoSnapshotIsMovedWithoutACopy: the release's catalog says sonarr
// has nothing an upgrade can rewrite, so the step leaves it running, copies only
// radarr, and the plan says so after sonarr's name. A copy box never took is a
// copy box will never restore, so the operator reads it before the step moves.
func TestAnAppWithNoSnapshotIsMovedWithoutACopy(t *testing.T) {
	h := newHarness(t)
	catalog, err := os.ReadFile("testdata/catalog-nosnapshot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h.charts.catalog = catalog
	h.grant = "testdata/grant-radarr.yaml"
	h.charts.publishVariants("0.4.0", "0.4.0", "0.4.0", variantsBoth(digestA, digestD))
	h.charts.publishVariants("0.5.0", "0.4.0", "0.4.0", variantsBoth(digestB, digestE))

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Steps[0].Moving, []string{"radarr", "sonarr"}) {
		t.Fatalf("moving = %v, want both apps the release moved", res.Steps[0].Moving)
	}
	if !reflect.DeepEqual(res.Steps[0].Copied(), []string{"radarr"}) {
		t.Fatalf("copied = %v, want only radarr: the catalog gives sonarr nothing to copy", res.Steps[0].Copied())
	}
	if !reflect.DeepEqual(h.scales(), []string{"radarr=0"}) {
		t.Fatalf("scales = %v, want only the copied app stopped", h.scales())
	}
	if len(h.snaps.takes) != 1 || !reflect.DeepEqual(h.snaps.takes[0].apps, []string{"radarr"}) {
		t.Fatalf("takes = %+v, want one copy of radarr", h.snaps.takes)
	}
	if !strings.Contains(h.out.String(), "is moved without a copy of its config") {
		t.Fatalf("the plan does not mark sonarr as uncopied:\n%s", h.out)
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
	if len(h.app.calls) != 2 {
		t.Fatalf("applies = %d, want the release moved without hooks and applied once more with them", len(h.app.calls))
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
	if len(h.app.calls) != 2 || h.app.calls[0].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want the release applied at 0.5.0 and its hooks run after the gate", h.app.calls)
	}
	for _, want := range []string{
		"0.4.0 → 0.5.0 · 1 step · no image moves",
		"sonarr",
		"held at " + digestA + " by your values",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("plan does not print %q:\n%s", want, h.out)
		}
	}
}

// TestARollbackHoldIsNamedInThePlan: an app a rollback holds runs the image the
// snapshot recorded, not the one the release pins, so nothing of it moves and
// the plan says why -- and until which release that stays true.
func TestARollbackHoldIsNamedInThePlan(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.holds.holds = []snapshot.Hold{{
		App:      "sonarr",
		At:       snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0", Digest: digestC},
		Avoiding: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestB},
		Snapshot: "0.4.0-0.5.0-20260102T030405Z",
		Since:    h.now,
		By:       snapshot.ByUpdate,
	}}

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Moving) != 0 {
		t.Fatalf("steps = %+v, want nothing moving: the hold keeps the app on its image", res.Steps)
	}
	if !reflect.DeepEqual(res.Steps[0].HeldBack, []string{"sonarr"}) {
		t.Fatalf("heldBack = %v, want sonarr", res.Steps[0].HeldBack)
	}
	if len(h.scales()) != 0 || len(h.snaps.takes) != 0 {
		t.Fatalf("a held-back app was stopped or copied: scales=%v takes=%+v", h.scales(), h.snaps.takes)
	}
	for _, want := range []string{
		"0.4.0 → 0.5.0 · 1 step · no image moves",
		"held at 4.9.0 after a rollback",
		"it moves again when a release pins something other than 5.0.0",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("plan does not print %q:\n%s", want, h.out)
		}
	}
}

// TestALapsingHoldMovesTheApp: the release pins an image other than the one the
// rollback avoided, so the hold is over and the app moves -- and is copied --
// like any other app the step moves.
func TestALapsingHoldMovesTheApp(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.holds.holds = []snapshot.Hold{{
		App:      "sonarr",
		At:       snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.8.0", Digest: digestA},
		Avoiding: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0", Digest: digestC},
		Snapshot: "0.4.0-0.5.0-20260102T030405Z",
		Since:    h.now,
		By:       snapshot.ByUpdate,
	}}

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Steps[0].Moving, []string{"sonarr"}) {
		t.Fatalf("moving = %v, want sonarr: the release pins another image, so the hold has lapsed", res.Steps[0].Moving)
	}
	if len(res.Steps[0].HeldBack) != 0 {
		t.Fatalf("heldBack = %v, want nothing: the hold lapsed at this release", res.Steps[0].HeldBack)
	}
	if len(h.snaps.takes) != 1 || !reflect.DeepEqual(h.snaps.takes[0].apps, []string{"sonarr"}) {
		t.Fatalf("takes = %+v, want the lapsed app copied before it moves", h.snaps.takes)
	}
	if strings.Contains(h.out.String(), "after a rollback") {
		t.Fatalf("the plan still holds an app the release moved past:\n%s", h.out)
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
	if len(h.app.calls) != 4 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[1].ChartVersion != "0.5.0" ||
		h.app.calls[2].ChartVersion != "0.6.0" || h.app.calls[3].ChartVersion != "0.6.0" {
		t.Fatalf("applies = %+v, want each step moved without hooks and then applied with them, in order", h.app.calls)
	}
	if h.app.calls[0].SkipHooks == h.app.calls[1].SkipHooks || h.app.calls[2].SkipHooks == h.app.calls[3].SkipHooks {
		t.Fatalf("applies = %+v, want one hooks-off and one hooks-on apply per step", h.app.calls)
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

// TestAWiringGateFailureStopsTheUpdate: the release moved, the gate passed, and
// the wiring the closing apply runs says it did not work. Box leaves the release
// where the step put it, leaves the apps as the new release left them, keeps the
// snapshot and records the failed release.
func TestAWiringGateFailureStopsTheUpdate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	// The apply that moves the release is fine; the one that runs the hooks is
	// where the receipt says the release did not work.
	h.app.errSeq = map[string][]error{"0.5.0": {nil, errors.New("wiring receipt says failed")}}

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed wiring gate reported success")
	}
	for _, want := range []string{
		"update s5n to 0.5.0: the wiring gate failed (applying 0.5.0 with its hooks: wiring receipt says failed)",
		"s5n is on 0.5.0 (revision 4, deployed)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error %q still says box rolled the release back", err)
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
	const want = "update s5n to 0.5.0: the health gate failed (the release is not deployed and healthy after 50ms); " +
		"s5n is on 0.5.0 (revision 4, deployed)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if !reflect.DeepEqual(h.scales(), []string{"sonarr=0"}) {
		t.Fatalf("scales = %v, want nothing after the apply", h.scales())
	}
	if len(h.snaps.takes) != 1 || len(h.snaps.prunes) != 0 {
		t.Fatalf("takes = %+v prunes = %+v, want the failed step's snapshot kept", h.snaps.takes, h.snaps.prunes)
	}
	// A timeout leaves the app exactly where the step put it: no restore, no
	// hold, no second apply.
	if len(h.snaps.restores) != 0 || h.holds.saves != 0 || len(h.app.calls) != 1 {
		t.Fatalf("restores = %+v holds = %d applies = %d, want nothing touched after a timeout",
			h.snaps.restores, h.holds.saves, len(h.app.calls))
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeFailed || rec.Gate != gateHealth {
		t.Fatalf("LastStep = %+v, want a failed step naming the health gate", rec)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
}

// TestACrashloopRollsTheAppBackAndTheUpdateContinues: the step moves two apps,
// one of them keeps exiting. Box puts only that app back -- stop, wait, restore
// its copy, hold it, re-apply the same release -- and the step then counts as
// passed, so the run goes on to the next release.
func TestACrashloopRollsTheAppBackAndTheUpdateContinues(t *testing.T) {
	h := newHarness(t)
	h.grant = "testdata/grant-radarr.yaml"
	h.charts.publishVariants("0.4.0", "0.4.0", "0.4.0", variantsMoving(digestA, digestD, "7.0.0"))
	h.charts.publishVariants("0.5.0", "0.4.0", "0.4.0", variantsMoving(digestB, digestE, "7.1.0"))
	h.charts.publishVariants("0.6.0", "0.4.0", "0.5.0", variantsMoving(digestC, digestE, "7.1.0"))
	takenAt := h.now
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-" + takenAt.Format("20060102T150405Z"),
		At:   takenAt,
		Dirs: map[string]string{
			"radarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/radarr",
			"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr",
		},
	}
	// radarr exits on the first gate. The rollback brings it back on the image
	// it ran, and the second gate sees it ready.
	h.cluster.exiting = []Exiting{{App: "radarr", Restarts: 3}}
	h.cluster.exitingUntil = 1

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Taken, []Step{{From: "0.4.0", To: "0.5.0"}, {From: "0.5.0", To: "0.6.0"}}) {
		t.Fatalf("taken = %+v, want both steps: the rollback is what let the first one pass", res.Taken)
	}
	if h.rel.state.ChartVersion != "0.6.0" {
		t.Fatalf("release = %+v, want the run to have gone on to 0.6.0", h.rel.state)
	}

	// One ordered log across every fake: stop the copies, copy, apply without
	// the chart's hooks, watch, then the step-back's own order -- and the
	// hooks-on apply is the rollback's re-apply, after the restore.
	h.assertOrder(t,
		"check 0.4.0 -> 0.5.0", "scale radarr=0", "scale sonarr=0", "gone radarr", "gone sonarr", "take 0.4.0 -> 0.5.0",
		"apply 0.5.0 (no hooks)", "health",
		"scale radarr=0", "gone radarr", "restore 0.4.0 -> 0.5.0", "holds write", "apply 0.5.0", "health",
		"apply 0.6.0 (no hooks)", "health", "apply 0.6.0",
	)
	// Only the app that exited was put back; its sibling moved with the
	// release and stayed there.
	if want := []string{"radarr=0", "sonarr=0", "radarr=0", "sonarr=0"}; !reflect.DeepEqual(h.scales(), want) {
		t.Fatalf("scales = %v, want %v: the rollback stops radarr only, and the second step copies sonarr", h.scales(), want)
	}
	// One hold, at the image radarr ran, avoiding the one it exited on.
	if h.holds.saves != 1 || len(h.holds.holds) != 1 {
		t.Fatalf("holds = %+v (saves %d), want one hold", h.holds.holds, h.holds.saves)
	}
	hold := h.holds.holds[0]
	if hold.App != "radarr" || hold.By != snapshot.ByUpdate {
		t.Fatalf("hold = %+v, want radarr written by update", hold)
	}
	if hold.At.Digest != digestD || hold.Avoiding.Digest != digestE {
		t.Fatalf("hold = %+v, want the image it ran (%s) and the one it exited on (%s)", hold, digestD, digestE)
	}
	if hold.Snapshot != h.snaps.taken.Step || !hold.Since.Equal(h.now) {
		t.Fatalf("hold = %+v, want this step's copy and now", hold)
	}
	if !reflect.DeepEqual(h.snaps.restores[0].apps, []string{"radarr"}) {
		t.Fatalf("restored %v, want radarr alone", h.snaps.restores[0].apps)
	}
	// The re-apply is the same release, so Helm records a deployed revision
	// and the hold is what renders the old image -- and it is the step's one
	// hooks-on apply. The second step's two follow the same rule.
	if len(h.app.calls) != 4 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[1].ChartVersion != "0.5.0" ||
		h.app.calls[2].ChartVersion != "0.6.0" || h.app.calls[3].ChartVersion != "0.6.0" {
		t.Fatalf("applies = %+v, want 0.5.0 without hooks, 0.5.0 with them, then the same for 0.6.0", h.app.calls)
	}
	if !h.app.calls[0].SkipHooks || h.app.calls[1].SkipHooks || !h.app.calls[1].RunHooks {
		t.Fatalf("applies = %+v, want the first step's hooks run by its rollback's re-apply", h.app.calls)
	}
	for _, want := range []string{
		"✗ radarr         crashed 3 times on the new image",
		"✓ radarr         rolled back to 7.0.0 · its " + h.now.In(time.Local).Format("15:04") + " snapshot",
		"! radarr is held on 7.0.0 until a release ships a fixed image",
		"  $ box status",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("transcript does not say %q:\n%s", want, h.out)
		}
	}
}

// TestARolledBackStepRecordsTheAppsItPutBack: the step counts as passed and its
// record says which app was put back, so status and a later run can see it.
func TestARolledBackStepRecordsTheAppsItPutBack(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   h.now,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingUntil = 1

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Taken, []Step{{From: "0.4.0", To: "0.5.0"}}) {
		t.Fatalf("taken = %+v, want the step that was rolled back", res.Taken)
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeOK || rec.To != "0.5.0" {
		t.Fatalf("LastStep = %+v, want a passing step to 0.5.0", rec)
	}
	if !reflect.DeepEqual(rec.RolledBack, []string{"sonarr"}) || rec.Gate != "" {
		t.Fatalf("LastStep = %+v, want sonarr recorded as rolled back and no gate", rec)
	}
	if h.rel.state.ChartVersion != "0.5.0" {
		t.Fatalf("release = %+v, want the step's release", h.rel.state)
	}
}

// TestAStepRunsTheWiringOnceAfterTheGate: a step with nothing to roll back still
// has the chart's wiring to run, and it runs in one apply after the gate. The
// apply that moves the release runs with the hooks off, because the wiring Job
// waits for every app to answer and cannot pass while a moved app is still
// starting; the gate reads the apps; and the same release is applied once more
// with the hooks on, so the wiring runs against the images that stay.
func TestAStepRunsTheWiringOnceAfterTheGate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	h.assertOrder(t, "apply 0.5.0 (no hooks)", "health", "apply 0.5.0", "health")
	if len(h.app.calls) != 2 {
		t.Fatalf("applies = %+v, want the release moved and then applied once more for its hooks", h.app.calls)
	}
	if !h.app.calls[0].SkipHooks || h.app.calls[1].SkipHooks {
		t.Fatalf("applies = %+v, want the first without the chart's hooks and the second with them", h.app.calls)
	}
	if !h.app.calls[1].RunHooks {
		t.Fatalf("applies = %+v, want the closing apply to install for its hooks: the release is already deployed at this version", h.app.calls)
	}
}

// TestACrashloopingAppIsRestoredBeforeTheHooksRun: the step's one hooks-on apply
// comes after the restore, never before it. A wiring Job that runs while the app
// box is putting back is down would wait out its budget and fail, which is the
// whole reason the apply that moves the release runs without the hooks.
func TestACrashloopingAppIsRestoredBeforeTheHooksRun(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   h.now,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingUntil = 1

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	h.assertOrder(t,
		"apply 0.5.0 (no hooks)", "health",
		"scale sonarr=0", "gone sonarr", "restore 0.4.0 -> 0.5.0", "holds write", "apply 0.5.0",
	)
	if len(h.app.calls) != 2 {
		t.Fatalf("applies = %+v, want the release moved and the rollback's one re-apply", h.app.calls)
	}
	var wired int
	for _, call := range h.app.calls {
		if !call.SkipHooks {
			wired++
			if !call.RunHooks {
				t.Fatalf("applies = %+v, want the only hooks-on apply to be the one that installs for its hooks", h.app.calls)
			}
		}
	}
	if wired != 1 {
		t.Fatalf("applies = %+v, want exactly one hooks-on apply: the hold's", h.app.calls)
	}
}

// TestAnExitingAppWithoutASnapshotStopsTheUpdate: box has nothing to restore, so
// it does not scale the app or write a hold -- it stops, names the app and its
// exits, and records the release as failed.
func TestAnExitingAppWithoutASnapshotStopsTheUpdate(t *testing.T) {
	h := newHarness(t)
	catalog, err := os.ReadFile("testdata/catalog-nosnapshot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h.charts.catalog = catalog
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 4}}

	_, err = h.run(t, false)
	if err == nil {
		t.Fatalf("an exiting app box cannot restore passed the gate:\n%s", h.out)
	}
	const want = "update s5n to 0.5.0: the health gate failed (sonarr exited 4 times on its new image); " +
		"s5n is on 0.5.0 (revision 4, deployed)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if len(h.snaps.restores) != 0 || h.holds.saves != 0 {
		t.Fatalf("restores = %+v holds = %d, want nothing: the app has no copy to restore", h.snaps.restores, h.holds.saves)
	}
	// The app was never stopped: an app with nothing to copy is moved without
	// being scaled.
	if len(h.scales()) != 0 {
		t.Fatalf("scales = %v, want nothing: an uncopied app is not stopped", h.scales())
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Outcome != OutcomeFailed || rec.Gate != gateHealth {
		t.Fatalf("LastStep = %+v, want a failed health gate", rec)
	}
}

// TestTheGateReadsTheAppsBeforeHelmsStatus: an image that never starts makes
// Helm's own wait give up, so the release reads failed while the app is
// crashlooping. The gate has to see the exiting app whatever Helm says, or it
// would wait out the budget and record a timeout instead of rolling back.
func TestTheGateReadsTheAppsBeforeHelmsStatus(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   h.now,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	// The gate runs while Helm still calls the revision failed: the first apply
	// leaves the release failed, the rollback's re-apply leaves it deployed.
	h.app.statusSeq = []string{"failed"}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingUntil = 1

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Taken, []Step{{From: "0.4.0", To: "0.5.0"}}) {
		t.Fatalf("taken = %+v, want the rolled-back step", res.Taken)
	}
	if len(h.snaps.restores) != 1 {
		t.Fatalf("restores = %+v, want the app put back: the gate saw the crashloop although Helm said failed", h.snaps.restores)
	}
}

// TestARestoreThatFailsLeavesTheAppStopped: the copy is the one thing box must
// not get wrong, so a restore that refuses stops the step with the app still
// scaled to zero and says both things the operator needs: that it is stopped and
// where its copy is.
func TestARestoreThatFailsLeavesTheAppStopped(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	takenAt := h.now
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   takenAt,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	h.snaps.restoreErr = errors.New("restore: no readable manifest.json, so it is not a snapshot box took")
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed restore reported success")
	}
	const want = "update s5n to 0.5.0: the rollback gate failed (restore: no readable manifest.json, so it is not a snapshot box took); " +
		"s5n is on 0.5.0 (revision 4, deployed); sonarr is stopped; sonarr's snapshot is at " +
		"/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if h.holds.saves != 0 {
		t.Fatalf("holds written = %d, want none: the app was never put back", h.holds.saves)
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Gate != gateRollback || rec.Outcome != OutcomeFailed {
		t.Fatalf("LastStep = %+v, want a failed rollback gate", rec)
	}
}

// TestASecondGateThatFailsRecordsTheRollbackGate: the app was restored and held,
// but it keeps exiting on the image it was put back on. The step stops with the
// rollback gate, keeps the copy and points the operator at it.
func TestASecondGateThatFailsRecordsTheRollbackGate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   h.now,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a second gate that still sees the app exiting reported success")
	}
	const want = "update s5n to 0.5.0: the rollback gate failed (sonarr exited 3 times on its new image); " +
		"s5n is on 0.5.0 (revision 5, deployed); sonarr's snapshot is at " +
		"/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if h.holds.saves != 1 {
		t.Fatalf("holds written = %d, want the one the rollback made", h.holds.saves)
	}
	rec := h.state.state.LastStep
	if rec == nil || rec.Gate != gateRollback {
		t.Fatalf("LastStep = %+v, want the rollback gate", rec)
	}
	if !reflect.DeepEqual(h.state.state.Failed, []string{"0.5.0"}) {
		t.Fatalf("Failed = %v, want the release recorded", h.state.state.Failed)
	}
}

// TestAFailedApplyAtOneRestartRollsBackWhenItReachesThree: Helm's own wait gives
// up on the first CrashLoopBackOff, so the step's apply is where a never-starting
// image fails. A moved app that has restarted puts the step into the health
// gate's watch, which finds the crashloop and rolls the app back.
func TestAFailedApplyAtOneRestartRollsBackWhenItReachesThree(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{
		Step: "0.4.0-0.5.0-20260102T030405Z",
		At:   h.now,
		Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"},
	}
	h.app.errSeq = map[string][]error{"0.5.0": {errors.New("release s5n failed: pod sonarr is in CrashLoopBackOff")}}
	h.cluster.restarts = map[string]int32{"sonarr": 1}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingFrom = 2
	h.cluster.exitingUntil = 2

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Taken, []Step{{From: "0.4.0", To: "0.5.0"}}) {
		t.Fatalf("taken = %+v, want the step that was rolled back", res.Taken)
	}
	if len(h.snaps.restores) != 1 || h.holds.saves != 1 {
		t.Fatalf("restores = %+v holds = %d, want the app put back and held", h.snaps.restores, h.holds.saves)
	}
	// The failed apply, the watch's reading, then the re-apply.
	if len(h.app.calls) != 2 {
		t.Fatalf("applies = %+v, want the failed one and the re-apply", h.app.calls)
	}
}

// TestAFailedApplyThatRecoversReappliesOnce: the moved app restarted once and
// then came up, so the image was slow rather than broken. Helm's revision never
// reached deployed, so box applies the same release once more and the normal gate
// decides.
func TestAFailedApplyThatRecoversReappliesOnce(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errSeq = map[string][]error{"0.5.0": {errors.New("release s5n failed: pod sonarr was restarted")}}
	h.cluster.restarts = map[string]int32{"sonarr": 1}

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if !reflect.DeepEqual(res.Taken, []Step{{From: "0.4.0", To: "0.5.0"}}) {
		t.Fatalf("taken = %+v, want the step", res.Taken)
	}
	// The failed apply, the watch's reading, the retry, and the step's one
	// hooks-on apply.
	if len(h.app.calls) != 3 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[1].ChartVersion != "0.5.0" || h.app.calls[2].ChartVersion != "0.5.0" {
		t.Fatalf("applies = %+v, want 0.5.0 three times: the failed one, the re-run, and the hooks", h.app.calls)
	}
	if !h.app.calls[0].SkipHooks || !h.app.calls[1].SkipHooks || h.app.calls[2].SkipHooks || !h.app.calls[2].RunHooks {
		t.Fatalf("applies = %+v, want the retry without hooks and the last one with them", h.app.calls)
	}
	if len(h.snaps.restores) != 0 || h.holds.saves != 0 {
		t.Fatalf("restores = %+v holds = %d, want nothing rolled back", h.snaps.restores, h.holds.saves)
	}
	if h.state.state.LastStep == nil || h.state.state.LastStep.Outcome != OutcomeOK {
		t.Fatalf("LastStep = %+v, want a passing step", h.state.state.LastStep)
	}
}

// TestAFailedApplyWithNoMovedAppRestartingStopsAtTheApplyGate: an apply can fail
// for reasons that are not the image, and box must not spend the health budget
// on one. With no moved app restarting, the apply's failure is the answer.
func TestAFailedApplyWithNoMovedAppRestartingStopsAtTheApplyGate(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errFor["0.5.0"] = errors.New("values file: apps.sonarr.port is not an integer")
	h.u.HealthWait = time.Hour

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed apply reported success")
	}
	if h.cluster.healths != 1 {
		t.Fatalf("health was read %d times, want one: the apply's cause is read once, not waited out", h.cluster.healths)
	}
	if !strings.Contains(err.Error(), "the apply failed (values file: apps.sonarr.port is not an integer)") {
		t.Fatalf("error = %q, want the apply's own failure", err)
	}
}

// TestTheExitingCauseNamesEveryApp: the gate reports every moved app that is
// exiting, one clause each, so a record naming one app does not hide a second.
func TestTheExitingCauseNamesEveryApp(t *testing.T) {
	got := exitingText([]Exiting{{App: "radarr", Restarts: 5}, {App: "sonarr", Restarts: 3}})
	want := "radarr exited 5 times on its new image; sonarr exited 3 times on its new image"
	if got != want {
		t.Fatalf("exitingText = %q, want %q", got, want)
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
	if strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error %q still says box rolled the release back", err)
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
	if !strings.Contains(h.out.String(), "s5n 0.5.0 failed to update on this box before") {
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
	if !strings.Contains(h.out.String(), "s5n 0.7.0 is out and needs box 9.0.0 or newer") {
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
	if !strings.Contains(h.out.String(), "--check: nothing was written but the check time") {
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
	if len(h.app.calls) != 2 || h.app.calls[0].ChartVersion != "0.5.0-rc.1" || h.app.calls[1].ChartVersion != "0.5.0-rc.1" {
		t.Fatalf("applies = %+v, want the prerelease installed and its hooks run", h.app.calls)
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

// notices records what the run told it: the events a person who is not watching
// would read, in the order they happened.
type notices struct{ got []Notice }

func (n *notices) add(ctx context.Context, notice Notice) { n.got = append(n.got, notice) }

// TestAPassingStepNotifiesWhatMoved: the notice a step posts names the releases
// and every app it moved, with the images on either side -- but not an app it
// put back, because that app did not stay on the new image.
func TestAPassingStepNotifiesWhatMoved(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	seen := &notices{}
	h.u.Notify = seen.add

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(seen.got) != 1 {
		t.Fatalf("notices = %+v, want one for the step", seen.got)
	}
	got := seen.got[0]
	if got.Event != EventUpdated || got.Name != "s5n" || got.From != "0.4.0" || got.To != "0.5.0" {
		t.Fatalf("notice = %+v, want the step that passed", got)
	}
	if len(got.Apps) != 1 || got.Apps[0].App != "sonarr" || got.Apps[0].From == "" || got.Apps[0].To == "" {
		t.Fatalf("Apps = %+v, want sonarr with both its images", got.Apps)
	}
}

// TestARolledBackAppIsNotifiedBeforeTheStep: the app box put back is its own
// event, said first, and the step that passed does not claim it moved.
func TestARolledBackAppIsNotifiedBeforeTheStep(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.snaps.taken = Taken{Step: "0.4.0-0.5.0-20260102T030405Z", At: h.now, Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.4.0-0.5.0-20260102T030405Z/sonarr"}}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}
	h.cluster.exitingUntil = 1
	seen := &notices{}
	h.u.Notify = seen.add

	if _, err := h.run(t, false); err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(seen.got) != 2 {
		t.Fatalf("notices = %+v, want the rollback and the step", seen.got)
	}
	if seen.got[0].Event != EventRolledBack || seen.got[0].Apps[0].App != "sonarr" {
		t.Fatalf("first notice = %+v, want sonarr rolled back", seen.got[0])
	}
	if seen.got[1].Event != EventUpdated {
		t.Fatalf("second notice = %+v, want the step that passed", seen.got[1])
	}
	if len(seen.got[1].Apps) != 0 {
		t.Fatalf("the step claims it moved %+v, but that app was rolled back", seen.got[1].Apps)
	}
}

// TestAFailedStepNotifiesTheGateAndTheRelease: someone who is not watching needs
// to know where the box was left, so a stopped notice carries the gate, the
// reason, and what Helm says is deployed now.
func TestAFailedStepNotifiesTheGateAndTheRelease(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	h.app.errFor["0.5.0"] = errors.New("helm said no")
	seen := &notices{}
	h.u.Notify = seen.add

	if _, err := h.run(t, false); err == nil {
		t.Fatal("a failed apply passed the update")
	}
	if len(seen.got) != 1 {
		t.Fatalf("notices = %+v, want one for the stop", seen.got)
	}
	got := seen.got[0]
	if got.Event != EventStopped || got.Gate != gateApply || got.From != "0.4.0" || got.To != "0.5.0" {
		t.Fatalf("notice = %+v, want the apply gate named", got)
	}
	if !strings.Contains(got.Error, "helm said no") {
		t.Fatalf("Error = %q, want what stopped the step", got.Error)
	}
	if got.On != "0.4.0" {
		t.Fatalf("On = %q, want the release Helm still reports", got.On)
	}
}

// TestACheckNotifiesNothing: asking what would happen is not something that
// happened, so nobody is told about it.
func TestACheckNotifiesNothing(t *testing.T) {
	h := newHarness(t)
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestA)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestB)
	seen := &notices{}
	h.u.Notify = seen.add

	if _, err := h.run(t, true); err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(seen.got) != 0 {
		t.Fatalf("a --check posted %+v", seen.got)
	}
}
