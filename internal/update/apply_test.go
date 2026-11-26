package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
)

const (
	sonarrRepo = "registry.example.com/seedbox/sonarr"
	sonarrAlt  = "registry.example.com/seedbox/sonarr-alt"
	// qbtDigest is qbittorrent's digest in the fixture variants.yaml: the app
	// does not move unless a test says it does, so it stays out of every plan.
	qbtDigest       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sonarrAltDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
)

// applyChart's sonarr digest is the one a test moves to (digestB).
func applyChart(t *testing.T) release.Chart {
	t.Helper()
	read := func(name string) []byte {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return release.Chart{
		Name:       "s5n",
		Version:    "0.5.0",
		AppVersion: "0.5.0",
		Values:     read("chart-values.yaml"),
		Schema:     read("chart-schema.json"),
		Catalog:    read("catalog.yaml"),
		Variants:   variants(digestB),
	}
}

// snapshotHarness wires ApplySnapshots to fakes that share one call log.
type snapshotHarness struct {
	hook    *ApplySnapshots
	cluster *fakeCluster
	rel     *fakeRelease
	snaps   *fakeSnapshots
	log     *callLog
	chart   release.Chart
}

// newSnapshotHarness installs a chart pinning sonarr's digestB.
func newSnapshotHarness(t *testing.T) *snapshotHarness {
	t.Helper()
	h := &snapshotHarness{log: &callLog{}, chart: applyChart(t)}
	h.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: h.log}
	h.rel = &fakeRelease{state: release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 1, Status: "deployed"}}
	h.snaps = &fakeSnapshots{
		log: h.log,
		taken: Taken{
			Step: "0.5.0-0.5.0-20260102T030405Z",
			Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.5.0-0.5.0-20260102T030405Z/sonarr"},
		},
	}
	h.hook = &ApplySnapshots{
		OpenRelease:   func(string) (Release, error) { return h.rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return h.cluster, nil },
		OpenSnapshots: func(string, map[string]any, release.Chart) (Snapshotter, error) { return h.snaps, nil },
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
	}
	return h
}

// plan is what apply hands the step.
func (h *snapshotHarness) plan(values map[string]any) release.SnapshotPlan {
	return release.SnapshotPlan{
		Namespace:  "testbox",
		Chart:      h.chart,
		Values:     values,
		ConfigRoot: "/srv/s5n/config",
	}
}

// applyValues enables qbittorrent and sonarr with the test's image keys.
func applyValues(sonarrImage map[string]any, enable ...string) map[string]any {
	apps := map[string]any{
		"qbittorrent": map[string]any{"enabled": true},
		"sonarr":      map[string]any{"enabled": true},
		"radarr":      map[string]any{"enabled": false},
	}
	for _, app := range enable {
		apps[app] = map[string]any{"enabled": true}
	}
	if sonarrImage != nil {
		apps["sonarr"].(map[string]any)["image"] = sonarrImage
	}
	return map[string]any{"apps": apps}
}

// runningQBT is qbittorrent as the chart runs it; it never moves.
func runningQBT() Running {
	return Running{Images: map[string]snapshot.ImageRef{
		"qbittorrent": {Repository: "registry.example.com/seedbox/qbittorrent", Tag: "5.3.0", Digest: qbtDigest},
	}}
}

func TestApplySnapshotMovesADigest(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "5.0.0", Digest: digestA},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(nil)))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	want := []release.SnapshotMove{{
		App:     "sonarr",
		Ran:     snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0", Digest: digestA},
		MovedTo: snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
		Copied:  true,
	}}
	if !reflect.DeepEqual(work.Moves, want) {
		t.Fatalf("moves = %+v, want %+v", work.Moves, want)
	}
	if work.From != "0.5.0" || work.To != "0.5.0" {
		t.Fatalf("copy named %s-%s, want the installed release on both sides", work.From, work.To)
	}
}

func TestApplySnapshotMovesATagOnlyPin(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {
			Images:  map[string]snapshot.ImageRef{"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"}},
			Digests: map[string][]string{"sonarr": {digestA}},
		},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "5.0.0"})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 {
		t.Fatalf("moves = %+v, want sonarr", work.Moves)
	}
	move := work.Moves[0]
	if want := (snapshot.ImageRef{Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA}); move.Ran != want {
		t.Fatalf("ran = %+v, want %+v: the digest the pod reports", move.Ran, want)
	}
	if want := (snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0"}); move.MovedTo != want {
		t.Fatalf("movedTo = %+v, want %+v: the tag the values pin", move.MovedTo, want)
	}
	if row, want := move.Row(), "sonarr: stopped and snapshotted before it moves from 4.9.0 to 5.0.0"; row != want {
		t.Fatalf("row = %q, want %q", row, want)
	}
}

func TestApplySnapshotMovesAVariant(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"variant": "alt"})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 {
		t.Fatalf("moves = %+v, want sonarr", work.Moves)
	}
	if got := work.Moves[0].MovedTo; got.Repository != sonarrAlt || got.Tag != "5.1.0" || got.Digest != sonarrAltDigest {
		t.Fatalf("movedTo = %+v, want the alt variant", got)
	}
	if row, want := work.Moves[0].Row(), "sonarr: stopped and snapshotted before it moves from 5.0.0 to 5.1.0"; row != want {
		t.Fatalf("row = %q, want %q", row, want)
	}
}

func TestApplySnapshotSameRenderingDoesNotMove(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(nil)))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 0 {
		t.Fatalf("moves = %+v, want none: the app already runs what the chart pins", work.Moves)
	}
}

func TestApplySnapshotFirstEnableDoesNotMove(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(nil, "radarr")))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 0 {
		t.Fatalf("moves = %+v, want none: a first enable has nothing to migrate", work.Moves)
	}
}

func TestApplySnapshotSameTagOnlyPinWithADigestDoesNotMove(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {
			Images:  map[string]snapshot.ImageRef{"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"}},
			Digests: map[string][]string{"sonarr": {digestA}},
		},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "4.9.0"})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 0 {
		t.Fatalf("moves = %+v, want none: the pin names the tag the app runs", work.Moves)
	}
}

func TestApplySnapshotSameTagOnlyPinWithoutADigestDoesNotMove(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "4.9.0"})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 0 {
		t.Fatalf("moves = %+v, want none: the pin names the tag the app runs", work.Moves)
	}
}

func TestApplySnapshotMovesWithoutACopyWhenNoPodReportsTheDigest(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "5.0.0", "digest": digestB})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 {
		t.Fatalf("moves = %+v, want sonarr", work.Moves)
	}
	move := work.Moves[0]
	if want := "sonarr: moves from 4.9.0 to 5.0.0 without a snapshot (no pod reports the bytes it runs under tag 4.9.0)"; move.Row() != want {
		t.Fatalf("row = %q, want %q", move.Row(), want)
	}
	if len(work.Copied()) != 0 {
		t.Fatalf("copied = %v, want none: box cannot name the bytes it would put back", work.Copied())
	}
	for _, call := range h.log.calls {
		if strings.HasPrefix(call, "scale ") {
			t.Fatalf("an app with no copy to take was stopped: %v", h.log.calls)
		}
	}
}

func TestApplySnapshotMovesWithoutACopyOnDifferentDigests(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {
			Images: map[string]snapshot.ImageRef{
				"sonarr":    {Repository: sonarrRepo, Tag: "4.9.0"},
				"sonarr-4k": {Repository: sonarrRepo, Tag: "4.9.0"},
			},
			Digests: map[string][]string{"sonarr": {digestA}, "sonarr-4k": {digestB}},
		},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "5.0.0"})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 {
		t.Fatalf("moves = %+v, want sonarr", work.Moves)
	}
	move := work.Moves[0]
	if want := "sonarr: moves from 4.9.0 to 5.0.0 without a snapshot (its instances run different bytes under tag 4.9.0)"; move.Row() != want {
		t.Fatalf("row = %q, want %q", move.Row(), want)
	}
	if len(work.Copied()) != 0 {
		t.Fatalf("copied = %v, want none: the instances report different bytes", work.Copied())
	}
	for _, call := range h.log.calls {
		if strings.HasPrefix(call, "scale ") {
			t.Fatalf("an app with no copy to take was stopped: %v", h.log.calls)
		}
	}
}

func TestApplySnapshotMovesWithoutACopyOnDifferentInstances(t *testing.T) {
	h := newSnapshotHarness(t)
	h.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr":    {Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
			"sonarr-4k": {Repository: sonarrRepo, Tag: "4.9.0"},
		}},
	}
	work, err := h.hook.Pending(context.Background(), h.plan(applyValues(map[string]any{"tag": "5.0.0", "digest": digestB})))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 {
		t.Fatalf("moves = %+v, want sonarr", work.Moves)
	}
	move := work.Moves[0]
	want := fmt.Sprintf("sonarr: moves to 5.0.0 without a snapshot (its instances run different images: sonarr %s:5.0.0@%s, sonarr-4k %s:4.9.0)",
		sonarrRepo, digestB, sonarrRepo)
	if move.Row() != want {
		t.Fatalf("row = %q, want %q", move.Row(), want)
	}
	if len(work.Copied()) != 0 {
		t.Fatalf("copied = %v, want none: the instances run different images", work.Copied())
	}
}

func TestApplySnapshotStartsAppsAgainWhenTheCopyFails(t *testing.T) {
	h := newSnapshotHarness(t)
	h.snaps.takeErr = errors.New("disk full")
	plan := h.plan(applyValues(nil))
	work := release.SnapshotWork{
		From: "0.5.0", To: "0.5.0",
		Moves: []release.SnapshotMove{{
			App:     "sonarr",
			Ran:     snapshot.ImageRef{Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA},
			MovedTo: snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
			Copied:  true,
		}},
	}
	_, err := h.hook.Take(context.Background(), plan, work)
	if err == nil || !strings.Contains(err.Error(), "snapshotting sonarr: disk full") {
		t.Fatalf("Take = %v, want the copy's failure", err)
	}
	want := []string{"check 0.5.0 -> 0.5.0", "scale sonarr=0", "gone sonarr", "take 0.5.0 -> 0.5.0", "scale sonarr=1"}
	if !reflect.DeepEqual(h.log.calls, want) {
		t.Fatalf("calls = %v, want %v: checked, stopped, tried to copy, started again", h.log.calls, want)
	}
}

func TestApplySnapshotRefusesBeforeStoppingWhenItCannotCopy(t *testing.T) {
	h := newSnapshotHarness(t)
	h.snaps.checkErr = errors.New("/srv/s5n/config/sonarr: stat: no such file")
	work := release.SnapshotWork{
		From: "0.5.0", To: "0.5.0",
		Moves: []release.SnapshotMove{{
			App:     "sonarr",
			Ran:     snapshot.ImageRef{Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA},
			MovedTo: snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
			Copied:  true,
		}},
	}
	_, err := h.hook.Take(context.Background(), h.plan(applyValues(nil)), work)
	if err == nil || !strings.Contains(err.Error(), "cannot be snapshotted") {
		t.Fatalf("Take = %v, want the refusal", err)
	}
	for _, call := range h.log.calls {
		if strings.HasPrefix(call, "scale ") {
			t.Fatalf("the refusal stopped an app: %v", h.log.calls)
		}
	}
}

func TestApplyWithAnUnchangedTagOnlyPinSaysNothingToChange(t *testing.T) {
	log := &callLog{}
	var out bytes.Buffer
	chart := applyChart(t)

	// The fixture values with sonarr pinned to the tag it already asks for.
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	values, err := os.ReadFile("testdata/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := string(values) + "apps:\n  sonarr:\n    image:\n      tag: \"4.9.0\"\n"
	if err := os.WriteFile(valuesPath, []byte(pinned), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load("testdata/grant.yaml", valuesPath)
	if err != nil {
		t.Fatalf("values: %v", err)
	}
	deployed, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		t.Fatalf("effective values: %v", err)
	}

	cluster := &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: log}
	cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {
			Images:  map[string]snapshot.ImageRef{"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"}},
			Digests: map[string][]string{"sonarr": {digestA}},
		},
	}
	rel := &fakeRelease{state: release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 1, Status: "deployed"}}
	step := &ApplySnapshots{
		OpenRelease:   func(string) (Release, error) { return rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return cluster, nil },
		OpenSnapshots: func(string, map[string]any, release.Chart) (Snapshotter, error) { return &fakeSnapshots{log: log}, nil },
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
	}
	client := &applyClient{deployed: deployed, log: log}
	app := &release.Applier{
		Confirm: loggingConfirm{log: log},
		Say:     &say.Plain{W: &out},
		Open:    func(string) (release.ReleaseClient, error) { return client, nil },
		OpenStorage: func(string) (release.StorageClient, error) {
			return applyStorage{}, nil
		},
	}
	res, err := app.Apply(context.Background(), release.Options{
		Chart:       &chart,
		GrantPath:   "testdata/grant.yaml",
		ValuesPath:  valuesPath,
		PathsInNode: true,
		Snapshots:   step,
	})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if res.Applied {
		t.Fatalf("the unchanged apply ran an upgrade:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "nothing to change") {
		t.Fatalf("the unchanged apply did not say nothing to change:\n%s", out.String())
	}
	if len(cluster.log.calls) != 0 {
		t.Fatalf("the unchanged apply read the cluster's apps: %v", cluster.log.calls)
	}
}

// applyClient serves stored values, the upgrade, and the receipt.
type applyClient struct {
	deployed   map[string]any
	upgradeErr error
	log        *callLog
}

func (c *applyClient) Deployed(context.Context) (map[string]any, bool, error) {
	return c.deployed, true, nil
}

// State is the chart deployed, so only values and the snapshot step decide.
func (c *applyClient) State(context.Context) (release.ReleaseState, error) {
	return release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 1, Status: "deployed"}, nil
}

func (c *applyClient) Upgrade(context.Context, release.Chart, map[string]any) (int, error) {
	c.log.add("upgrade")
	if c.upgradeErr != nil {
		return 0, c.upgradeErr
	}
	return 2, nil
}

func (c *applyClient) Receipt(context.Context) (release.Receipt, error) {
	return release.Receipt{Status: "ok", StartedAt: time.Now().Add(time.Minute)}, nil
}

// applyStorage exists so the test is about the copy, not the claims.
type applyStorage struct{}

func (applyStorage) PlanNamespace(context.Context) (release.NamespacePlan, error) {
	return release.NamespacePlan{Name: "testbox", Exists: true, CanCreate: true, Enforce: "restricted"}, nil
}

func (applyStorage) Plan(context.Context, string, []seedbox.Volume) ([]release.StorageObject, error) {
	return nil, nil
}

func (applyStorage) CreateNamespace(context.Context) error { return nil }

func (applyStorage) Apply(context.Context, release.StorageObject) error { return nil }

func (applyStorage) Delete(context.Context, release.StorageObject) error { return nil }

// loggingConfirm answers yes and records it.
type loggingConfirm struct{ log *callLog }

func (c loggingConfirm) Ask(string, bool) (bool, error) {
	c.log.add("confirm")
	return true, nil
}

func TestApplyCopiesBeforeTheReleaseMoves(t *testing.T) {
	log := &callLog{}
	var out bytes.Buffer
	hook := &snapshotHarness{log: log, chart: applyChart(t)}
	hook.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: log}
	hook.cluster.running = map[string]Running{
		"qbittorrent": runningQBT(),
		"sonarr": {Images: map[string]snapshot.ImageRef{
			"sonarr": {Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA},
		}},
	}
	hook.rel = &fakeRelease{state: release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 1, Status: "deployed"}}
	hook.snaps = &fakeSnapshots{
		log: log,
		taken: Taken{
			Step: "0.5.0-0.5.0-20260102T030405Z",
			Dirs: map[string]string{"sonarr": "/srv/s5n/config/.box/snapshots/0.5.0-0.5.0-20260102T030405Z/sonarr"},
		},
	}
	step := &ApplySnapshots{
		OpenRelease:   func(string) (Release, error) { return hook.rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return hook.cluster, nil },
		OpenSnapshots: func(string, map[string]any, release.Chart) (Snapshotter, error) { return hook.snaps, nil },
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
	}
	client := &applyClient{deployed: map[string]any{"wiring": map[string]any{"timeoutSeconds": 1}}, log: log}
	chart := hook.chart
	app := &release.Applier{
		Confirm: loggingConfirm{log: log},
		Say:     &say.Plain{W: &out},
		Open:    func(string) (release.ReleaseClient, error) { return client, nil },
		OpenStorage: func(string) (release.StorageClient, error) {
			return applyStorage{}, nil
		},
	}
	_, err := app.Apply(context.Background(), release.Options{
		Chart:       &chart,
		GrantPath:   "testdata/grant.yaml",
		ValuesPath:  "testdata/values.yaml",
		PathsInNode: true,
		Snapshots:   step,
	})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}

	want := []string{"running", "confirm", "check 0.5.0 -> 0.5.0", "scale sonarr=0", "gone sonarr", "take 0.5.0 -> 0.5.0", "upgrade", "prune 0.5.0 -> 0.5.0"}
	if !reflect.DeepEqual(log.calls, want) {
		t.Fatalf("calls = %v, want %v", log.calls, want)
	}
	if len(hook.snaps.takes) != 1 {
		t.Fatalf("takes = %+v, want exactly the one app that moves", hook.snaps.takes)
	}
	take := hook.snaps.takes[0]
	if !reflect.DeepEqual(take.apps, []string{"sonarr"}) {
		t.Fatalf("took %v, want sonarr only: qbittorrent does not move", take.apps)
	}
	wantImage := ImageChange{
		Ran:      snapshot.ImageRef{Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA},
		MovedTo:  snapshot.ImageRef{Repository: sonarrRepo, Tag: "5.0.0", Digest: digestB},
		Snapshot: true,
	}
	if !reflect.DeepEqual(take.images["sonarr"], wantImage) {
		t.Fatalf("images[sonarr] = %+v, want %+v", take.images["sonarr"], wantImage)
	}
	if row, wantRow := "  sonarr: stopped and snapshotted before it moves from 4.9.0 to 5.0.0", out.String(); !strings.Contains(wantRow, row) {
		t.Fatalf("the plan does not name the copy:\n%s", wantRow)
	}
}

func TestRenderedImageKeepsAPinnedTagWithItsDigest(t *testing.T) {
	chart := applyChart(t)
	values := applyValues(map[string]any{"tag": "4.9.0", "digest": digestA})
	got, ok := renderedImage(chart, values, nil, "sonarr")
	if !ok {
		t.Fatal("renderedImage found no image for sonarr")
	}
	want := snapshot.ImageRef{Repository: sonarrRepo, Tag: "4.9.0", Digest: digestA}
	if got != want {
		t.Fatalf("rendered = %+v, want %+v", got, want)
	}
}

func TestRollbackPinAfterAnApplySnapshotWritesBothHalves(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sonarr")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sonarr.db", "config.xml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := &callLog{}
	cluster := &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: log}
	cluster.running = map[string]Running{
		"sonarr": {
			Images:  map[string]snapshot.ImageRef{"sonarr": {Repository: sonarrRepo, Tag: "4.9.0"}},
			Digests: map[string][]string{"sonarr": {digestA}},
		},
	}
	charts := &fakeCharts{charts: map[string]release.Chart{}}
	valuesBytes, err := os.ReadFile("testdata/chart-values.yaml")
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
	charts.values, charts.schema, charts.catalog = valuesBytes, schema, catalog
	charts.publishVariants("0.5.0", "", "", rollbackVariants())
	chart := charts.charts["0.5.0"]

	// The user's values pin the tag alone -- the case a rollback has to make
	// exact.
	values := map[string]any{"apps": map[string]any{
		"sonarr": map[string]any{"enabled": true, "image": map[string]any{"tag": "5.0.0"}},
	}}
	globs := map[string][]string{"sonarr": {"sonarr.db", "config.xml"}}
	snaps := &ConfigSnapshots{
		ConfigRoot: root,
		Values:     values,
		Globs:      func(context.Context, string) (map[string][]string, error) { return globs, nil },
		Now:        func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	rel := &fakeRelease{state: release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 1, Status: "deployed"}}
	step := &ApplySnapshots{
		OpenRelease:   func(string) (Release, error) { return rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return cluster, nil },
		OpenSnapshots: func(string, map[string]any, release.Chart) (Snapshotter, error) { return snaps, nil },
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
	}
	plan := release.SnapshotPlan{Namespace: "testbox", Chart: chart, Values: values, ConfigRoot: root}
	work, err := step.Pending(context.Background(), plan)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(work.Moves) != 1 || work.Moves[0].Ran.Digest != digestA {
		t.Fatalf("moves = %+v, want sonarr recorded with the digest its pod reported", work.Moves)
	}
	if _, err := step.Take(context.Background(), plan, work); err != nil {
		t.Fatalf("Take: %v", err)
	}

	// The copy the apply left is what `box rollback sonarr --pin` reads.
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	src, err := os.ReadFile("testdata/rollback-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valuesPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	holds := &fakeHolds{log: log}
	applied := &fakeApply{errFor: map[string]error{}, applied: map[string]bool{}, managed: rel, log: log, nextRev: 1}
	rb := &Rollback{
		Charts:      charts,
		Open:        func(string) (Release, error) { return rel, nil },
		OpenCluster: func(string) (Cluster, error) { return cluster, nil },
		OpenSnapshots: func(string, map[string]any) (Snapshots, error) {
			return &NewestSnapshots{ConfigRoot: root, Values: values}, nil
		},
		Apply:       applied.apply,
		Holds:       holds,
		Confirm:     &fakeConfirm{answer: true},
		Say:         &say.Plain{W: &bytes.Buffer{}},
		HealthWait:  50 * time.Millisecond,
		SettleWait:  50 * time.Millisecond,
		CleanupWait: 50 * time.Millisecond,
		Poll:        time.Millisecond,
		Now:         func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	if _, err := rb.Run(context.Background(), RollbackOptions{
		App:   "sonarr",
		Pin:   true,
		Apply: release.Options{GrantPath: "testdata/grant.yaml", ValuesPath: valuesPath},
	}); err != nil {
		t.Fatalf("rollback --pin: %v", err)
	}

	sb, err := seedbox.Load("testdata/grant.yaml", valuesPath)
	if err != nil {
		t.Fatalf("the pinned values file does not load: %v", err)
	}
	merged, err := sb.Merge()
	if err != nil {
		t.Fatalf("the pinned values file does not merge: %v", err)
	}
	image := merged["apps"].(map[string]any)["sonarr"].(map[string]any)["image"].(map[string]any)
	if image["tag"] != "4.9.0" || image["digest"] != digestA {
		t.Fatalf("pinned image = %v, want 4.9.0 at the digest the apply recorded (%s)", image, digestA)
	}
}
