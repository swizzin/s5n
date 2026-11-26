package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
)

// The step moved sonarr from 0.4.0 to 0.5.0, three days before now.
const rollbackStep = "0.4.0-0.5.0-20251230T030405Z"

const rollbackRepo = "registry.example.com/seedbox/sonarr"

// rollbackVariants pins sonarr where the step moved it, so the sentences can
// tell the put-back image from the avoided one.
func rollbackVariants() []byte {
	return []byte(fmt.Sprintf(`qbittorrent:
  default:
    repository: registry.example.com/seedbox/qbittorrent
    tag: "5.3.0"
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
sonarr:
  default:
    repository: %s
    tag: "5.1.0"
    digest: %s
radarr:
  default:
    repository: registry.example.com/seedbox/radarr
    tag: "7.0.0"
    digest: sha256:3333333333333333333333333333333333333333333333333333333333333333
`, rollbackRepo, digestB))
}

// fakeNewest is the newest-copy store, logging the restore and its globs.
type fakeNewest struct {
	found    AppSnapshot
	err      error
	restore  error
	restores int
	globs    []string
	log      *callLog
}

func (f *fakeNewest) Newest(_ context.Context, app string) (AppSnapshot, error) {
	f.log.add("newest %s", app)
	if f.err != nil {
		return AppSnapshot{}, f.err
	}
	return f.found, nil
}

func (f *fakeNewest) Restore(_ context.Context, snap AppSnapshot, globs []string) error {
	f.log.add("restore %s", snap.App)
	f.restores++
	f.globs = globs
	return f.restore
}

// rollbackHarness is one `box rollback sonarr` on 0.5.0 with a three-day copy.
type rollbackHarness struct {
	r       *Rollback
	charts  *fakeCharts
	rel     *fakeRelease
	cluster *fakeCluster
	snaps   *fakeNewest
	holds   *fakeHolds
	app     *fakeApply
	confirm *fakeConfirm
	log     *callLog
	out     *bytes.Buffer
	now     time.Time
	grant   string
	values  string
}

func newRollbackHarness(t *testing.T) *rollbackHarness {
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
	h := &rollbackHarness{
		log:    &callLog{},
		out:    &bytes.Buffer{},
		now:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		grant:  "testdata/grant.yaml",
		values: "testdata/values.yaml",
	}
	h.charts = &fakeCharts{values: values, schema: schema, catalog: catalog, charts: map[string]release.Chart{}}
	h.charts.publishVariants("0.5.0", "", "", rollbackVariants())
	h.rel = &fakeRelease{
		state: release.ReleaseState{Exists: true, ChartVersion: "0.5.0", Revision: 3, Status: "deployed"},
	}
	h.cluster = &fakeCluster{replicas: map[string]int32{}, pods: map[string]int{}, healthy: true, log: h.log}
	h.holds = &fakeHolds{log: h.log}
	h.app = &fakeApply{errFor: map[string]error{}, applied: map[string]bool{}, managed: h.rel, log: h.log, nextRev: 3}
	h.confirm = &fakeConfirm{answer: true}
	h.snaps = h.newest()
	h.r = &Rollback{
		Charts:        h.charts,
		Open:          func(string) (Release, error) { return h.rel, nil },
		OpenCluster:   func(string) (Cluster, error) { return h.cluster, nil },
		OpenSnapshots: func(string, map[string]any) (Snapshots, error) { return h.snaps, nil },
		Apply:         h.app.apply,
		Holds:         h.holds,
		Confirm:       h.confirm,
		Say:           &say.Plain{W: h.out},
		HealthWait:    50 * time.Millisecond,
		SettleWait:    50 * time.Millisecond,
		CleanupWait:   50 * time.Millisecond,
		Poll:          time.Millisecond,
		Now:           func() time.Time { return h.now },
	}
	return h
}

// newest is the copy the step left, holding both images.
func (h *rollbackHarness) newest() *fakeNewest {
	return &fakeNewest{
		log: h.log,
		found: AppSnapshot{App: "sonarr", Instances: []InstanceSnapshot{{
			Name: "sonarr",
			Dir:  filepath.Join("/srv/s5n/config/.box/snapshots", rollbackStep, "sonarr"),
			Step: rollbackStep,
			Manifest: snapshot.Manifest{
				App:     "sonarr",
				Taken:   h.now.Add(-72 * time.Hour),
				Image:   snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.0.0", Digest: digestA},
				MovedTo: snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB},
				Globs:   []string{"sonarr.db", "sonarr.db-wal"},
				Files:   []snapshot.File{{Path: "sonarr.db", Size: 4, SHA256: "00"}},
			},
		}}},
	}
}

func (h *rollbackHarness) run(t *testing.T, pin bool) (RollbackResult, error) {
	t.Helper()
	return h.r.Run(context.Background(), RollbackOptions{
		App:   "sonarr",
		Pin:   pin,
		Apply: release.Options{GrantPath: h.grant, ValuesPath: h.values},
	})
}

// assertOrder checks the shared call log's order.
func (h *rollbackHarness) assertOrder(t *testing.T, want ...string) {
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

// scales is the scale calls as "app=N", in order.
func (h *rollbackHarness) scales() []string {
	var out []string
	for _, call := range h.log.calls {
		if rest, ok := strings.CutPrefix(call, "scale "); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestRollbackSaysHowOldTheCopyIsThenHoldsTheApp(t *testing.T) {
	h := newRollbackHarness(t)

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, h.out)
	}
	announcement := "sonarr's newest snapshot is 3 days old (taken 2025-12-30 03:04 UTC, before 0.4.0 -> 0.5.0). Rolling back puts sonarr back on 5.0.0 and loses everything sonarr wrote since.\n"
	if !strings.Contains(h.out.String(), announcement) {
		t.Fatalf("output =\n%s\nwant the announcement:\n%s", h.out, announcement)
	}
	if h.confirm.asks != 1 {
		t.Fatalf("asked %d questions, want 1", h.confirm.asks)
	}
	if got := h.confirm.questions[0]; got != "roll sonarr back to 5.0.0?" {
		t.Fatalf("question = %q", got)
	}
	h.assertOrder(t, "scale sonarr=0", "gone sonarr", "restore sonarr", "holds write", "apply 0.5.0", "health")
	if got := h.scales(); len(got) != 1 || got[0] != "sonarr=0" {
		t.Fatalf("scales = %v, want one stop and no restart", got)
	}
	if len(h.app.calls) != 1 || h.app.calls[0].ChartVersion != "0.5.0" || h.app.calls[0].Chart != nil {
		t.Fatalf("applies = %+v, want one apply of the installed release", h.app.calls)
	}
	if len(h.holds.holds) != 1 {
		t.Fatalf("holds = %+v, want one", h.holds.holds)
	}
	got := h.holds.holds[0]
	want := snapshot.Hold{
		App:      "sonarr",
		At:       snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.0.0", Digest: digestA},
		Avoiding: snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB},
		Snapshot: rollbackStep,
		Since:    h.now,
		By:       snapshot.ByRollback,
	}
	if got != want {
		t.Fatalf("hold = %+v, want %+v", got, want)
	}
	if res.Pinned {
		t.Fatal("a default rollback reported a pin")
	}
	last := "sonarr is on 5.0.0. It stays there until a release pins something other than 5.1.0.\n"
	if !strings.HasSuffix(h.out.String(), last) {
		t.Fatalf("output does not end with the done line:\n%s", h.out)
	}
}

func TestRollbackPinKeepsTheValuesFileAndWritesNoHold(t *testing.T) {
	h := newRollbackHarness(t)
	h.values = filepath.Join(t.TempDir(), "values.yaml")
	src, err := os.ReadFile("testdata/rollback-values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.values, src, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := h.run(t, true)
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, h.out)
	}
	h.assertOrder(t, "scale sonarr=0", "gone sonarr", "restore sonarr", "apply 0.5.0", "health")
	if h.holds.saves != 0 {
		t.Fatalf("holds written = %d, want none with --pin", h.holds.saves)
	}
	if !res.Pinned {
		t.Fatal("a --pin rollback did not report the pin")
	}

	edited, err := os.ReadFile(h.values)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{
		"# The user's file: only keys the grant does not own.",
		"# hand, and the comments are the half of it box must not lose.",
		"  # How long the wiring Job waits for an app before it gives up.",
		"wiring:\n  # How long the wiring Job waits for an app before it gives up.\n  timeoutSeconds: 300\n",
	} {
		if !strings.Contains(string(edited), kept) {
			t.Fatalf("the edited values file lost %q:\n%s", kept, edited)
		}
	}
	// The pin is in the file as the two keys the chart renders from, and it
	// survives a re-read as a seedbox.
	sb, err := seedbox.Load(h.grant, h.values)
	if err != nil {
		t.Fatalf("the edited values file does not load: %v", err)
	}
	merged, err := sb.Merge()
	if err != nil {
		t.Fatalf("the edited values file does not merge: %v", err)
	}
	image := merged["apps"].(map[string]any)["sonarr"].(map[string]any)["image"].(map[string]any)
	if image["digest"] != digestA || image["tag"] != "5.0.0" {
		t.Fatalf("pinned image = %v, want %s at 5.0.0", image, digestA)
	}
	last := "sonarr is on 5.0.0, pinned in " + h.values + ". Remove the pin from apps.sonarr.image when you want it to move again.\n"
	if !strings.HasSuffix(h.out.String(), last) {
		t.Fatalf("output does not end with the done line:\n%s", h.out)
	}
}

func TestRollbackPinReplacesAHandPin(t *testing.T) {
	h := newRollbackHarness(t)
	h.values = filepath.Join(t.TempDir(), "values.yaml")
	src, err := os.ReadFile("testdata/rollback-values-pinned.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.values, src, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.run(t, true); err != nil {
		t.Fatalf("rollback --pin over a hand pin: %v\n%s", err, h.out)
	}
	edited, err := os.ReadFile(h.values)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(edited), "# The user's file, with sonarr held at an image by hand") {
		t.Fatalf("the edited values file lost its comment:\n%s", edited)
	}
	sb, err := seedbox.Load(h.grant, h.values)
	if err != nil {
		t.Fatalf("the edited values file does not load: %v", err)
	}
	merged, err := sb.Merge()
	if err != nil {
		t.Fatalf("the edited values file does not merge: %v", err)
	}
	image := merged["apps"].(map[string]any)["sonarr"].(map[string]any)["image"].(map[string]any)
	if image["digest"] != digestA || image["tag"] != "5.0.0" {
		t.Fatalf("pinned image = %v, want the snapshot's digest and tag", image)
	}
}

// Every refusal is byte-exact and happens before the first scale: an app
// stopped for a rollback that refuses is a seedbox down for nothing.
func TestRollbackRefusesBeforeAnythingStops(t *testing.T) {
	newerStep := "0.5.0-0.6.0-20260101T030405Z"
	cases := []struct {
		name  string
		pin   bool
		tweak func(*rollbackHarness)
		want  string
	}{
		{
			name: "no snapshot at all",
			tweak: func(h *rollbackHarness) {
				h.snaps.found = AppSnapshot{App: "sonarr", Instances: []InstanceSnapshot{{Name: "sonarr"}}}
			},
			want: "sonarr has no snapshot to roll back to",
		},
		{
			name: "an instance the newest copy does not hold",
			tweak: func(h *rollbackHarness) {
				h.snaps.found.Instances = append(h.snaps.found.Instances,
					InstanceSnapshot{Name: "sonarr-4k"})
			},
			want: "sonarr's newest snapshot is missing sonarr-4k",
		},
		{
			name: "instances from different steps",
			tweak: func(h *rollbackHarness) {
				h.snaps.found.Instances = append(h.snaps.found.Instances,
					InstanceSnapshot{
						Name: "sonarr-4k",
						Dir:  filepath.Join("/srv/s5n/config/.box/snapshots", newerStep, "sonarr-4k"),
						Step: newerStep,
					})
			},
			want: "sonarr's instances were last copied by different steps, so there is no one copy to put back: " +
				"sonarr from " + rollbackStep + ", sonarr-4k from " + newerStep,
		},
		{
			name: "a copy that names no image",
			tweak: func(h *rollbackHarness) {
				inst := &h.snaps.found.Instances[0]
				inst.Manifest.Image = snapshot.ImageRef{}
			},
			want: "sonarr's newest snapshot does not record which image wrote it, so box cannot say what to roll back to",
		},
		{
			name: "the release already renders that image",
			tweak: func(h *rollbackHarness) {
				inst := &h.snaps.found.Instances[0]
				inst.Manifest.Image = snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB}
			},
			want: "sonarr already runs 5.1.0",
		},
		{
			name: "a hold already keeps it there",
			tweak: func(h *rollbackHarness) {
				h.holds.holds = []snapshot.Hold{{
					App:      "sonarr",
					At:       snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.0.0", Digest: digestA},
					Avoiding: snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB},
					Snapshot: rollbackStep,
					Since:    h.now.Add(-time.Hour),
					By:       snapshot.ByRollback,
				}}
			},
			want: "sonarr already runs 5.0.0",
		},
		{
			name: "a pin in the values",
			tweak: func(h *rollbackHarness) {
				h.values = "testdata/rollback-values-pinned.yaml"
			},
			want: "sonarr is pinned in your values (5.0.0); remove the pin, or pass --pin to replace it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRollbackHarness(t)
			tc.tweak(h)
			_, err := h.run(t, tc.pin)
			if err == nil {
				t.Fatalf("the rollback was allowed: %v\n%s", h.log.calls, h.out)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
			if len(h.scales()) != 0 || h.snaps.restores != 0 || h.confirm.asks != 0 || len(h.app.calls) != 0 {
				t.Fatalf("a refused rollback wrote: %v", h.log.calls)
			}
		})
	}
}

func TestRollbackFailureAfterTheRestoreLeavesTheAppWhereItIs(t *testing.T) {
	h := newRollbackHarness(t)
	h.app.errFor["0.5.0"] = errors.New("helm: timed out")

	_, err := h.run(t, false)
	if err == nil {
		t.Fatal("a failed apply was reported as a finished rollback")
	}
	want := "rolling sonarr back to 5.0.0: applying 0.5.0: helm: timed out; s5n is on 0.5.0 (revision 3, deployed); " +
		"sonarr's snapshot is at " + h.snaps.found.Instances[0].Dir
	if err.Error() != want {
		t.Fatalf("error =\n%q\nwant\n%q", err, want)
	}
	if h.snaps.restores != 1 {
		t.Fatalf("restores = %d, want the one this run took", h.snaps.restores)
	}
	if got := h.scales(); len(got) != 1 {
		t.Fatalf("scales = %v, want the stop and no restart: the app stays where the run left it", got)
	}
}

func TestRollbackNewestSnapshotsReadsAndRestoresEveryInstance(t *testing.T) {
	root := t.TempDir()
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"instances": []any{
		map[string]any{"enabled": true, "suffix": ""},
		map[string]any{"enabled": true, "suffix": "4k"},
	}}}}
	images := snapshot.Images{
		Ran:     snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.0.0", Digest: digestA},
		MovedTo: snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB},
	}
	for _, instance := range []string{"sonarr", "sonarr-4k"} {
		cfg := filepath.Join(root, instance)
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "sonarr.db"), []byte("before "+instance), 0o600); err != nil {
			t.Fatal(err)
		}
		dest, err := snapshot.Dir(root, "0.4.0", "0.5.0", time.Date(2025, 12, 30, 3, 4, 5, 0, time.UTC), instance)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := snapshot.Take(cfg, []string{"sonarr.db", "sonarr.db-wal"}, dest, images); err != nil {
			t.Fatal(err)
		}
		// The failed image's database, and a -wal the copy does not hold.
		if err := os.WriteFile(filepath.Join(cfg, "sonarr.db"), []byte("after "+instance), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "sonarr.db-wal"), []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	store := &NewestSnapshots{ConfigRoot: root, Values: values}
	found, err := store.Newest(context.Background(), "sonarr")
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if len(found.Instances) != 2 {
		t.Fatalf("instances = %+v, want both", found.Instances)
	}
	for _, inst := range found.Instances {
		if inst.Dir == "" || inst.Step != "0.4.0-0.5.0-20251230T030405Z" {
			t.Fatalf("instance = %+v, want the step it came from", inst)
		}
		if inst.Manifest.Image != images.Ran {
			t.Fatalf("manifest image = %+v, want %+v", inst.Manifest.Image, images.Ran)
		}
	}

	if err := store.Restore(context.Background(), found, []string{"sonarr.db", "sonarr.db-wal"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, instance := range []string{"sonarr", "sonarr-4k"} {
		got, err := os.ReadFile(filepath.Join(root, instance, "sonarr.db"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "before "+instance {
			t.Fatalf("%s = %q, want the copy's bytes", instance, got)
		}
		if _, err := os.Stat(filepath.Join(root, instance, "sonarr.db-wal")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s kept a -wal the copy does not hold: %v", instance, err)
		}
	}
}

func TestRollbackNewestSnapshotsWithoutACopy(t *testing.T) {
	store := &NewestSnapshots{ConfigRoot: t.TempDir(), Values: map[string]any{}}
	found, err := store.Newest(context.Background(), "sonarr")
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if len(found.Instances) != 1 || found.Instances[0].Name != "sonarr" || found.Instances[0].Dir != "" {
		t.Fatalf("instances = %+v, want sonarr with no copy", found.Instances)
	}
}

func TestAgeText(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, "0 minutes"},
		{59 * time.Second, "0 minutes"},
		{90 * time.Second, "1 minute"},
		{59 * time.Minute, "59 minutes"},
		{time.Hour, "1 hour"},
		{time.Hour + 59*time.Minute, "1 hour"},
		{23*time.Hour + 59*time.Minute, "23 hours"},
		{24 * time.Hour, "1 day"},
		{72 * time.Hour, "3 days"},
		{-5 * time.Minute, "0 minutes"},
	}
	for _, tc := range cases {
		if got := ageText(tc.age); got != tc.want {
			t.Errorf("ageText(%s) = %q, want %q", tc.age, got, tc.want)
		}
	}
}

func TestStepVersions(t *testing.T) {
	cases := []struct {
		step     string
		from, to string
		ok       bool
	}{
		{"0.4.0-0.5.0-20251230T030405Z", "0.4.0", "0.5.0", true},
		{"0.4.0-0.5.0-rc1-20251230T030405Z", "0.4.0", "0.5.0-rc1", true},
		{"0.4.0-rc1-0.5.0-20251230T030405Z", "0.4.0-rc1", "0.5.0", true},
		{"not-a-step-20251230T030405Z", "", "", false},
		{"0.4.0-20251230T030405Z", "", "", false},
	}
	for _, tc := range cases {
		from, to, ok := stepVersions(tc.step)
		if ok != tc.ok || from != tc.from || to != tc.to {
			t.Errorf("stepVersions(%q) = %q, %q, %v; want %q, %q, %v", tc.step, from, to, ok, tc.from, tc.to, tc.ok)
		}
	}
}

func TestRollbackRestoresWithTheGlobsTheCopyWasTakenWith(t *testing.T) {
	root := t.TempDir()
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{}}}
	images := snapshot.Images{
		Ran:     snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.0.0", Digest: digestA},
		MovedTo: snapshot.ImageRef{Repository: rollbackRepo, Tag: "5.1.0", Digest: digestB},
	}
	cfg := filepath.Join(root, "sonarr")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sonarr.db"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.xml"), []byte("settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := snapshot.Dir(root, "0.4.0", "0.5.0", time.Date(2025, 12, 30, 3, 4, 5, 0, time.UTC), "sonarr")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Take(cfg, []string{"*.db"}, dest, images); err != nil {
		t.Fatal(err)
	}

	store := &NewestSnapshots{ConfigRoot: root, Values: values}
	found, err := store.Newest(context.Background(), "sonarr")
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	// The installed catalog now also names config.xml.
	if err := store.Restore(context.Background(), found, []string{"*.db", "*.xml"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(cfg, "config.xml"))
	if err != nil || string(got) != "settings" {
		t.Fatalf("config.xml = %q, %v: a rollback removed a file the copy never held", got, err)
	}
}

func TestRollbackNamesTheAppNotAnInstance(t *testing.T) {
	values := map[string]any{"apps": map[string]any{
		"sonarr": map[string]any{"enabled": true, "instances": []any{
			map[string]any{"enabled": true, "suffix": ""},
			map[string]any{"enabled": true, "suffix": "4k"},
		}},
		"radarr": map[string]any{"enabled": true},
		"plex":   map[string]any{"enabled": false},
	}}
	cases := []struct{ app, want string }{
		{"sonarr", ""},
		{"radarr", ""},
		{"sonarr-4k", "sonarr-4k is an instance of sonarr: every instance of an app runs one image, so `box rollback sonarr` puts all of them back together"},
		{"plex", "plex is not an app this seedbox runs: box rollback takes an app from your values (radarr, sonarr)"},
		{"lidarr", "lidarr is not an app this seedbox runs: box rollback takes an app from your values (radarr, sonarr)"},
	}
	for _, tc := range cases {
		err := checkEnabledApp("/srv/s5n/config", values, tc.app)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: error = %q, want %q", tc.app, got, tc.want)
		}
	}
}
