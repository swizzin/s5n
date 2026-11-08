package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/seedbox"
)

// fakeRelease stands in for the Helm client. It records what box asked it to
// do, which is how the tests prove that a refusal reached the cluster nowhere.
type fakeRelease struct {
	values      map[string]any
	exists      bool
	deployedErr error

	upgrades int
	upgraded []map[string]any
	revision int

	receipt    Receipt
	receiptErr error

	events *[]string
}

func (f *fakeRelease) Deployed(context.Context) (map[string]any, bool, error) {
	if f.deployedErr != nil {
		return nil, false, f.deployedErr
	}
	return f.values, f.exists, nil
}

func (f *fakeRelease) Upgrade(_ context.Context, _ Chart, values map[string]any) (int, error) {
	f.upgrades++
	f.revision++
	f.upgraded = append(f.upgraded, values)
	f.values, f.exists = values, true
	if f.events != nil {
		*f.events = append(*f.events, "helm")
	}
	return f.revision, nil
}

func (f *fakeRelease) Receipt(context.Context) (Receipt, error) {
	if f.receiptErr != nil {
		return Receipt{}, f.receiptErr
	}
	return f.receipt, nil
}

// fakeStorage is a tiny cluster of storage objects: a claim exists or it does
// not, and Apply records the order box wrote things in. It shares the event log
// with fakeRelease, so a test can prove storage lands before the release.
type fakeStorage struct {
	existing map[string]bool
	planErr  error
	plans    int
	applied  []StorageObject
	events   *[]string
}

func (f *fakeStorage) Plan(_ context.Context, _ string, vols []seedbox.Volume) ([]StorageObject, error) {
	f.plans++
	if f.planErr != nil {
		return nil, f.planErr
	}
	var out []StorageObject
	for _, v := range vols {
		if f.existing[v.Claim] {
			continue
		}
		out = append(out, StorageObject{Kind: "PersistentVolume", Name: v.Claim, Detail: "hostPath " + v.Path})
		out = append(out, StorageObject{Kind: "PersistentVolumeClaim", Name: v.Claim, Detail: "volume " + v.Claim})
	}
	return out, nil
}

func (f *fakeStorage) Apply(_ context.Context, obj StorageObject) error {
	f.applied = append(f.applied, obj)
	f.existing[obj.Name] = true
	if f.events != nil {
		*f.events = append(*f.events, "storage:"+obj.Name)
	}
	return nil
}

type fakes struct {
	app         *Applier
	rel         *fakeRelease
	store       *fakeStorage
	out         *bytes.Buffer
	events      []string
	grantPath   string
	values      string
	openCalls   int
	storeOpens  int
	namespace   string
	pathsInNode bool
	dataPath    string
	configPath  string
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	dir := t.TempDir()
	f := &fakes{
		rel: &fakeRelease{
			receipt: Receipt{
				Status:    "ok",
				StartedAt: time.Now().Add(time.Minute),
				Steps:     []Step{{Name: "sonarr/downloadclient", Result: "ok"}},
			},
		},
		store:     &fakeStorage{existing: map[string]bool{}, events: nil},
		out:       &bytes.Buffer{},
		grantPath: filepath.Join(dir, "grant.yaml"),
		values:    filepath.Join(dir, "values.yaml"),
	}
	copyFile(t, "testdata/grant.yaml", f.grantPath)
	copyFile(t, "testdata/values.yaml", f.values)

	// The grant's paths are real directories in the test's own temp dir: box
	// stats them, and the refusal it can make is about a path that is missing.
	data := filepath.Join(dir, "data")
	config := filepath.Join(dir, "config")
	for _, d := range []string{data, config} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(t, f.grantPath, map[string]string{
		"/srv/s5n/data":   data,
		"/srv/s5n/config": config,
	})

	f.dataPath, f.configPath = data, config
	f.rel.events = &f.events
	f.store.events = &f.events
	f.app = &Applier{
		Charts:  DirChart{},
		Confirm: &Terminal{Yes: true},
		Log:     f.out,
		Open: func(ns string) (ReleaseClient, error) {
			f.openCalls++
			f.namespace = ns
			return f.rel, nil
		},
		OpenStorage: func(string) (StorageClient, error) {
			f.storeOpens++
			return f.store, nil
		},
	}
	return f
}

func (f *fakes) apply(t *testing.T) (Result, error) {
	t.Helper()
	return f.app.Apply(context.Background(), Options{
		ChartDir:    "testdata/chart",
		GrantPath:   f.grantPath,
		ValuesPath:  f.values,
		PathsInNode: f.pathsInNode,
	})
}

// rewrite replaces fixed strings in a file, so a fixture can name paths the
// test can actually create.
func rewrite(t *testing.T, path string, repl map[string]string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for from, to := range repl {
		out = strings.ReplaceAll(out, from, to)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakes) writeValues(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.values, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFirstApplyInstallsWithTheGrantInCharge(t *testing.T) {
	f := newFakes(t)
	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.FirstInstall || !res.Applied || res.Revision != 1 {
		t.Fatalf("result = %+v, want a first install at revision 1", res)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want 1", f.rel.upgrades)
	}
	if f.namespace != "testbox" {
		t.Fatalf("namespace = %q, want the grant's testbox", f.namespace)
	}

	vals := f.rel.upgraded[0]
	if _, ok := vals["namespace"]; ok {
		t.Fatalf("namespace reached the chart values: %v", vals["namespace"])
	}
	apps, _ := vals["apps"].(map[string]any)
	on := func(name string) bool {
		entry, _ := apps[name].(map[string]any)
		v, _ := entry["enabled"].(bool)
		return v
	}
	if !on("sonarr") || !on("qbittorrent") {
		t.Fatalf("granted apps are not enabled: %v", apps)
	}
	if on("radarr") {
		t.Fatalf("radarr is enabled although the grant never mentions it: %v", apps)
	}
	// The claims are box's: named from the grant's namespace, not from either
	// file, and not from the chart's defaults.
	if got := vals["storage"].(map[string]any)["data"].(map[string]any)["existingClaim"]; got != "testbox-s5n-data" {
		t.Fatalf("storage.data.existingClaim = %v, want testbox-s5n-data", got)
	}
	if got := vals["storage"].(map[string]any)["config"].(map[string]any)["existingClaim"]; got != "testbox-s5n-config" {
		t.Fatalf("storage.config.existingClaim = %v, want testbox-s5n-config", got)
	}
	if !strings.Contains(f.out.String(), "first install") {
		t.Fatalf("output does not say first install:\n%s", f.out)
	}
}

func TestSecondApplyIsANoOp(t *testing.T) {
	f := newFakes(t)
	if _, err := f.apply(t); err != nil {
		t.Fatalf("first apply: %v\n%s", err, f.out)
	}
	upgrades := f.rel.upgrades

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("second apply: %v\n%s", err, f.out)
	}
	if f.rel.upgrades != upgrades {
		t.Fatalf("the unchanged apply made %d Helm write(s), want 0", f.rel.upgrades-upgrades)
	}
	if res.Applied {
		t.Fatalf("result says applied although nothing changed: %+v", res)
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("output does not say nothing to change:\n%s", f.out)
	}
}

func TestOneKeyChangeIsShownAndApplied(t *testing.T) {
	f := newFakes(t)
	if _, err := f.apply(t); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("changed apply: %v\n%s", err, f.out)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "wiring.timeoutSeconds" {
		t.Fatalf("changes = %+v, want exactly wiring.timeoutSeconds", res.Changes)
	}
	if !strings.Contains(f.out.String(), `~ wiring.timeoutSeconds: 300 -> 600`) {
		t.Fatalf("the diff was not printed:\n%s", f.out)
	}
	if f.rel.upgrades != 2 {
		t.Fatalf("upgrades = %d, want 2", f.rel.upgrades)
	}
	last := f.rel.upgraded[len(f.rel.upgraded)-1]
	if got := last["wiring"].(map[string]any)["timeoutSeconds"]; got != 600 {
		t.Fatalf("applied wiring.timeoutSeconds = %v, want 600", got)
	}
	if res.Revision != 2 {
		t.Fatalf("revision = %d, want 2", res.Revision)
	}
}

func TestGrantKeyInValuesRefusesBeforeTheClusterIsOpened(t *testing.T) {
	f := newFakes(t)
	f.writeValues(t, "gateway:\n  host: box.example.com\n")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although values sets a grant key")
	}
	if !strings.Contains(err.Error(), "gateway.host is set by the grant") {
		t.Fatalf("error does not name the key: %v", err)
	}
	if f.openCalls != 0 || f.rel.upgrades != 0 {
		t.Fatalf("a refusal touched the cluster: open=%d upgrades=%d", f.openCalls, f.rel.upgrades)
	}
}

func TestSchemaViolationRefusesBeforeTheClusterIsOpened(t *testing.T) {
	f := newFakes(t)
	f.writeValues(t, "wiring:\n  timeout: 5\n")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although values has a key the chart does not know")
	}
	if !strings.Contains(err.Error(), "wiring") || !strings.Contains(err.Error(), "is not a key the chart knows") {
		t.Fatalf("error does not name the key: %v", err)
	}
	if f.openCalls != 0 || f.rel.upgrades != 0 {
		t.Fatalf("a refusal touched the cluster: open=%d upgrades=%d", f.openCalls, f.rel.upgrades)
	}
}

func TestMissingNamespaceRefusesBeforeTheClusterIsOpened(t *testing.T) {
	f := newFakes(t)
	grant, err := os.ReadFile(f.grantPath)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.Replace(string(grant), "namespace: testbox\n", "", 1)
	if err := os.WriteFile(f.grantPath, []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("error = %v, want a missing-namespace refusal", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("a refusal opened a cluster client: %d", f.openCalls)
	}
}

func TestDeclinedPromptWritesNothing(t *testing.T) {
	f := newFakes(t)
	f.app.Confirm = &Terminal{
		In:    strings.NewReader("n\n"),
		Out:   f.out,
		IsTTY: func() bool { return true },
	}
	_, err := f.apply(t)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("a declined prompt made %d Helm write(s)", f.rel.upgrades)
	}
	if !strings.Contains(f.out.String(), "oh heck, nvm") {
		t.Fatalf("the voice is missing:\n%s", f.out)
	}
}

func TestNoTTYWithoutYesRefuses(t *testing.T) {
	f := newFakes(t)
	f.app.Confirm = &Terminal{Out: f.out, IsTTY: func() bool { return false }}
	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("error = %v, want a not-a-terminal refusal", err)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("a refusal made %d Helm write(s)", f.rel.upgrades)
	}
}

func TestWiringFailureFailsTheApply(t *testing.T) {
	f := newFakes(t)
	f.rel.receipt = Receipt{
		Status:    "failed",
		StartedAt: time.Now().Add(time.Minute),
		Error:     "sonarr/downloadclient: refused",
	}
	res, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "wiring failed: sonarr/downloadclient: refused") {
		t.Fatalf("error = %v, want a wiring failure", err)
	}
	if !res.Applied || res.Revision != 1 {
		t.Fatalf("result = %+v, want the release recorded despite the wiring failure", res)
	}
}

func TestReceiptIsPrintedPerStep(t *testing.T) {
	f := newFakes(t)
	f.rel.receipt = Receipt{
		Status:    "ok",
		StartedAt: time.Now().Add(time.Minute),
		Steps: []Step{
			{Name: "sonarr/downloadclient", Result: "ok"},
			{Name: "plex/claim", Result: "pending", Detail: "no Plex token"},
		},
	}
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	for _, want := range []string{
		"wiring ok",
		"  sonarr/downloadclient: ok",
		"  plex/claim: pending (no Plex token)",
	} {
		if !strings.Contains(f.out.String(), want) {
			t.Fatalf("output is missing %q:\n%s", want, f.out)
		}
	}
}

func TestStaleReceiptIsNotThisRunsReceipt(t *testing.T) {
	f := newFakes(t)
	f.rel.receipt = Receipt{Status: "ok", StartedAt: time.Now().Add(-time.Hour)}
	f.app.ReceiptWait = 20 * time.Millisecond
	f.app.Poll = 5 * time.Millisecond

	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "wiring receipt never arrived") {
		t.Fatalf("error = %v, want a stale receipt to time out", err)
	}
}

func TestNoChartIsRefused(t *testing.T) {
	f := newFakes(t)
	_, err := f.app.Apply(context.Background(), Options{GrantPath: f.grantPath, ValuesPath: f.values})
	if err == nil || !strings.Contains(err.Error(), "--chart") {
		t.Fatalf("error = %v, want a missing-chart refusal", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("a refusal opened a cluster client")
	}
}

// TestFirstApplyCreatesStorageBeforeTheRelease: the claims are not release
// objects, so box writes them itself, before Helm runs. The order matters: a
// pod cannot schedule against a claim that appears after it.
func TestFirstApplyCreatesStorageBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if res.StorageCreated != 4 {
		t.Fatalf("storage created = %d, want two PVs and two claims", res.StorageCreated)
	}
	var kinds []string
	for _, o := range f.store.applied {
		kinds = append(kinds, o.Kind+"/"+o.Name)
	}
	want := []string{"PersistentVolume/testbox-s5n-data", "PersistentVolumeClaim/testbox-s5n-data",
		"PersistentVolume/testbox-s5n-config", "PersistentVolumeClaim/testbox-s5n-config"}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("applied %v, want %v", kinds, want)
	}
	if len(f.events) == 0 || f.events[len(f.events)-1] != "helm" {
		t.Fatalf("events = %v, want the release last", f.events)
	}
	if !strings.Contains(f.out.String(), "+ PersistentVolume testbox-s5n-data (hostPath ") {
		t.Fatalf("the diff does not show the storage it creates:\n%s", f.out)
	}
	if !strings.Contains(f.out.String(), "+ PersistentVolumeClaim testbox-s5n-config (volume testbox-s5n-config)") {
		t.Fatalf("the diff does not show the config claim:\n%s", f.out)
	}
}

// TestStorageConflictRefusesBeforeAnyWrite: a PV of box's name that backs
// another path is a refusal, and it names both paths. Nothing is written -- not
// the release and not the other volume's objects.
func TestStorageConflictRefusesBeforeAnyWrite(t *testing.T) {
	f := newFakes(t)
	f.store.planErr = errors.New("PersistentVolume testbox-s5n-data already backs /srv/old, but storage.data.path is /srv/new: box never changes a volume's path in place")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although a PV backs another path")
	}
	if !strings.Contains(err.Error(), "/srv/old") || !strings.Contains(err.Error(), "/srv/new") {
		t.Fatalf("error does not name both paths: %v", err)
	}
	if f.rel.upgrades != 0 || len(f.store.applied) != 0 {
		t.Fatalf("a refusal wrote: upgrades=%d storage=%d", f.rel.upgrades, len(f.store.applied))
	}
	if strings.Contains(f.out.String(), "first install") {
		t.Fatalf("a refusal printed an apply plan:\n%s", f.out)
	}
}

// TestMissingHostPathRefusesBeforeTheClusterIsOpened: a grant pointing at a
// directory that is not there stops before box has a cluster client at all. The
// message names `box init`, which is the command that creates the directory.
func TestMissingHostPathRefusesBeforeTheClusterIsOpened(t *testing.T) {
	f := newFakes(t)
	gone := filepath.Join(t.TempDir(), "gone")
	rewrite(t, f.grantPath, map[string]string{f.dataPath: gone})

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although the data path does not exist")
	}
	if !strings.Contains(err.Error(), "storage.data.path") || !strings.Contains(err.Error(), "box init") {
		t.Fatalf("error = %v, want it to name the key and `box init`", err)
	}
	if f.openCalls != 0 || f.storeOpens != 0 {
		t.Fatalf("a refusal opened a client: release=%d storage=%d", f.openCalls, f.storeOpens)
	}
}

// TestPathsInNodeSkipsTheHostCheck: the e2e writes the directories inside the
// kind node, where this host cannot see them, so it is the one caller allowed to
// skip the stat. Nothing else changes: the objects still come from the grant.
func TestPathsInNodeSkipsTheHostCheck(t *testing.T) {
	f := newFakes(t)
	f.pathsInNode = true
	gone := filepath.Join(t.TempDir(), "only-in-the-node")
	rewrite(t, f.grantPath, map[string]string{f.dataPath: gone})

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.Applied || res.StorageCreated != 4 {
		t.Fatalf("result = %+v, want an install with four storage objects", res)
	}
	if f.store.applied[0].Detail != "hostPath "+gone {
		t.Fatalf("PV detail = %q, want the grant's path", f.store.applied[0].Detail)
	}
}
