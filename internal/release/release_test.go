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

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// fakeRelease stands in for the Helm client. It records what box asked it to
// do, which is how the tests prove that a refusal reached the cluster nowhere.
type fakeRelease struct {
	values      map[string]any
	exists      bool
	deployedErr error
	upgradeErr  error

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
	if f.upgradeErr != nil {
		return 0, f.upgradeErr
	}
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

// fakeStorage is a tiny cluster of storage objects: a namespace and its claims
// exist or they do not, and Apply records the order box wrote things in. It
// shares the event log with fakeRelease, so a test can prove the namespace and
// the storage land before the release.
type fakeStorage struct {
	// nsExists starts false: a fake cluster is a fresh one, which is the case
	// that used to fail. nsCanCreate is what the access review answers.
	nsExists    bool
	nsCanCreate bool
	nsEnforce   string
	nsErr       error
	nsCreateErr error
	nsCreates   int

	existing map[string]bool
	planErr  error
	plans    int
	applied  []StorageObject
	// applyErr fails the write of applyErrOn ("Kind/Name"), or any write when
	// it is empty: the failure that used to strand a volume.
	applyErr   error
	applyErrOn string
	deleteErr  error
	deleted    []StorageObject
	// deleteBounded records, per Delete, whether its context had a deadline.
	deleteBounded []bool
	events        *[]string
}

func (f *fakeStorage) PlanNamespace(context.Context) (NamespacePlan, error) {
	if f.nsErr != nil {
		return NamespacePlan{}, f.nsErr
	}
	plan := NamespacePlan{Name: "testbox", Exists: f.nsExists, CanCreate: f.nsCanCreate}
	if f.nsExists {
		plan.Enforce = f.nsEnforce
	}
	return plan, nil
}

func (f *fakeStorage) CreateNamespace(context.Context) error {
	if f.nsCreateErr != nil {
		return f.nsCreateErr
	}
	f.nsCreates++
	f.nsExists = true
	if f.events != nil {
		*f.events = append(*f.events, "namespace")
	}
	return nil
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
	if f.applyErr != nil && (f.applyErrOn == "" || f.applyErrOn == obj.Kind+"/"+obj.Name) {
		return f.applyErr
	}
	f.applied = append(f.applied, obj)
	f.existing[obj.Name] = true
	if f.events != nil {
		*f.events = append(*f.events, "storage:"+obj.Name)
	}
	return nil
}

func (f *fakeStorage) Delete(ctx context.Context, obj StorageObject) error {
	// A real client's request fails on a cancelled context; a fake that ignored
	// it would pass an undo that cannot work after Ctrl-C.
	if err := ctx.Err(); err != nil {
		return err
	}
	_, bounded := ctx.Deadline()
	f.deleteBounded = append(f.deleteBounded, bounded)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if obj.Kind != KindPersistentVolume {
		return errors.New("box only deletes a volume it created")
	}
	f.deleted = append(f.deleted, obj)
	if f.events != nil {
		*f.events = append(*f.events, "deleted:"+obj.Name)
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
	hosts       *fakeHosts
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
		store:     &fakeStorage{existing: map[string]bool{}, events: nil, nsCanCreate: true},
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
		Confirm: &say.Plain{Yes: true},
		Say:     &say.Plain{W: f.out},
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
	opts := Options{
		Chart:       testChartPtr(t),
		GrantPath:   f.grantPath,
		ValuesPath:  f.values,
		PathsInNode: f.pathsInNode,
	}
	if f.hosts != nil {
		opts.Hosts = f.hosts
	}
	return f.app.Apply(context.Background(), opts)
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
	f.app.Confirm = &say.Plain{In: strings.NewReader("n\n"), W: f.out}
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
	// No reader: this is a stdin that is not a terminal, which is the case a
	// pipe and /dev/null both are.
	f.app.Confirm = &say.Plain{W: f.out}
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
	if err == nil || !strings.Contains(err.Error(), "no chart source") {
		t.Fatalf("error = %v, want a missing-chart refusal", err)
	}
	if f.openCalls != 0 {
		t.Fatalf("a refusal opened a cluster client")
	}
}

// recordingChartSource records the reference apply asks for, so the mapping
// from Options to the source is asserted from this side too.
type recordingChartSource struct {
	ref   ChartRef
	chart Chart
}

func (r *recordingChartSource) Load(_ context.Context, ref ChartRef) (Chart, error) {
	r.ref = ref
	return r.chart, nil
}

// TestApplyAsksItsSourceForTheOptionsRef: apply loads its chart inside the
// Applier, after both files are read. A --chart-repo or --chart-version that
// stops at Options would silently pull the default release instead of the one
// the operator named, which is what the flag tests in cmd/box cannot see.
func TestApplyAsksItsSourceForTheOptionsRef(t *testing.T) {
	f := newFakes(t)
	src := &recordingChartSource{chart: testChart(t)}
	f.app.Charts = src

	_, err := f.app.Apply(context.Background(), Options{
		ChartRepo:    "oci://127.0.0.1:5000/charts/s5n",
		ChartVersion: "1.2.3",
		GrantPath:    f.grantPath,
		ValuesPath:   f.values,
		PathsInNode:  f.pathsInNode,
	})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	want := ChartRef{Repo: "oci://127.0.0.1:5000/charts/s5n", Version: "1.2.3"}
	if src.ref != want {
		t.Fatalf("the chart source was asked for %+v, want %+v", src.ref, want)
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

// TestFirstInstallCreatesTheMissingNamespaceFirst: on a fresh cluster nothing
// exists, and the namespace has to be box's first write -- before the PV, which
// is cluster-scoped and used to be left behind when the claim failed.
func TestFirstInstallCreatesTheMissingNamespaceFirst(t *testing.T) {
	f := newFakes(t)
	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.NamespaceCreated || f.store.nsCreates != 1 {
		t.Fatalf("namespace created %d times, result %+v, want one creation", f.store.nsCreates, res)
	}
	if want := "namespace"; len(f.events) == 0 || f.events[0] != want {
		t.Fatalf("events = %v, want the namespace before the storage and the release", f.events)
	}
	if !strings.Contains(f.out.String(), "  + Namespace testbox (Pod Security restricted)") {
		t.Fatalf("the plan does not show the namespace it creates:\n%s", f.out)
	}
	if strings.Index(f.out.String(), "+ Namespace testbox") > strings.Index(f.out.String(), "+ PersistentVolume testbox-s5n-data") {
		t.Fatalf("the namespace is planned after the volumes it has to precede:\n%s", f.out)
	}
}

// TestMissingNamespaceWithoutCreatePermissionRefusesBeforeAnyWrite: the gate
// fires before side effects. A user's kubeconfig is namespace-scoped, so it
// cannot create the namespace and apply says whose job that is; nothing --
// namespace, PV, claim or release -- is written.
func TestMissingNamespaceWithoutCreatePermissionRefusesBeforeAnyWrite(t *testing.T) {
	f := newFakes(t)
	f.store.nsCanCreate = false

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although the namespace is missing and cannot be created")
	}
	for _, want := range []string{"namespace testbox does not exist", "box adduser"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q: %v", want, err)
		}
	}
	if f.store.nsCreates != 0 || len(f.store.applied) != 0 || f.rel.upgrades != 0 {
		t.Fatalf("a refusal wrote: namespaces=%d storage=%d upgrades=%d",
			f.store.nsCreates, len(f.store.applied), f.rel.upgrades)
	}
	if strings.Contains(f.out.String(), "first install") {
		t.Fatalf("a refusal printed an apply plan:\n%s", f.out)
	}
}

// TestExistingNamespaceIsNeverRelabelled: root's or an admin's namespace wins,
// even when it does not enforce restricted. box says so once and writes
// nothing; a second apply says it again and still does not apply the namespace.
func TestExistingNamespaceIsNeverRelabelled(t *testing.T) {
	f := newFakes(t)
	f.store.nsExists = true
	f.store.nsEnforce = "baseline"

	for i := 0; i < 2; i++ {
		if _, err := f.apply(t); err != nil {
			t.Fatalf("apply %d: %v\n%s", i+1, err, f.out)
		}
	}
	if f.store.nsCreates != 0 {
		t.Fatalf("apply applied over an existing namespace %d times", f.store.nsCreates)
	}
	warn := "namespace testbox does not enforce Pod Security restricted (pod-security.kubernetes.io/enforce=baseline): root's labels win, box leaves it alone"
	if got := strings.Count(f.out.String(), warn); got != 2 {
		t.Fatalf("the weak-namespace warning printed %d times over two applies, want 2:\n%s", got, f.out)
	}
	if strings.Contains(f.out.String(), "+ Namespace testbox") {
		t.Fatalf("the plan offers to create a namespace that exists:\n%s", f.out)
	}
}

// TestExistingRestrictedNamespaceIsSilent: a namespace already at restricted is
// not something to warn about, and it is not in the plan either.
func TestExistingRestrictedNamespaceIsSilent(t *testing.T) {
	f := newFakes(t)
	f.store.nsExists = true
	f.store.nsEnforce = "restricted"

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if strings.Contains(f.out.String(), "does not enforce Pod Security") {
		t.Fatalf("box warned about a namespace that is already restricted:\n%s", f.out)
	}
}

// TestAFailedWriteRemovesTheVolumeThisRunCreated: the PV lands, the claim after
// it fails, and the volume goes again. A claim is namespaced and left for the
// next apply; a PV is cluster-scoped and is exactly the thing that used to be
// stranded.
func TestAFailedWriteRemovesTheVolumeThisRunCreated(t *testing.T) {
	f := newFakes(t)
	f.store.applyErr = errors.New("namespaces \"testbox\" not found")
	f.store.applyErrOn = KindPersistentVolumeClaim + "/testbox-s5n-data"

	_, err := f.apply(t)
	if err == nil {
		t.Fatalf("apply succeeded although the claim could not be written\n%s", f.out)
	}
	if !strings.Contains(err.Error(), "namespaces \"testbox\" not found") {
		t.Fatalf("error lost the cause: %v", err)
	}
	if len(f.store.deleted) != 1 || f.store.deleted[0].Name != "testbox-s5n-data" {
		t.Fatalf("deleted = %+v, want only the volume this run created", f.store.deleted)
	}
	if !strings.Contains(f.out.String(), "removed PersistentVolume testbox-s5n-data: this run created it and did not finish") {
		t.Fatalf("box did not say what it removed:\n%s", f.out)
	}
	// The claim that failed was never written, and Helm never ran.
	var claims int
	for _, o := range f.store.applied {
		if o.Kind == KindPersistentVolumeClaim {
			claims++
		}
	}
	if claims != 0 || f.rel.upgrades != 0 {
		t.Fatalf("a failed run wrote %d claims and %d releases", claims, f.rel.upgrades)
	}
}

// TestAFailedReleaseRemovesTheVolumesThisRunCreated: the storage landed and Helm
// did not, so the volumes go too. Without this, a first install that never got
// a release leaves two cluster-scoped PVs behind.
func TestAFailedReleaseRemovesTheVolumesThisRunCreated(t *testing.T) {
	f := newFakes(t)
	f.rel.upgradeErr = errors.New("apply s5n in testbox: release failed")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although the release failed")
	}
	if len(f.store.deleted) != 2 {
		t.Fatalf("deleted = %+v, want both volumes this run created", f.store.deleted)
	}
	for _, obj := range f.store.deleted {
		if obj.Kind != KindPersistentVolume {
			t.Fatalf("deleted a %s: box only removes volumes it created", obj.Kind)
		}
	}
}

// TestAFailedApplyNeverDeletesAPreExistingVolume: a volume that was there
// before this run is not box's to remove, even when the run fails after it.
// Volume "config" exists; the data volume is created and its claim fails.
func TestAFailedApplyNeverDeletesAPreExistingVolume(t *testing.T) {
	f := newFakes(t)
	f.store.existing["testbox-s5n-config"] = true
	f.store.applyErr = errors.New("claim refused")
	f.store.applyErrOn = KindPersistentVolumeClaim + "/testbox-s5n-data"

	if _, err := f.apply(t); err == nil {
		t.Fatal("apply succeeded although the claim could not be written")
	}
	for _, obj := range f.store.deleted {
		if obj.Name == "testbox-s5n-config" {
			t.Fatalf("deleted the pre-existing volume %s", obj.Name)
		}
	}
	if len(f.store.deleted) != 1 || f.store.deleted[0].Name != "testbox-s5n-data" {
		t.Fatalf("deleted = %+v, want only the data volume this run created", f.store.deleted)
	}
}

// TestAFailedUndoSaysTheVolumeIsStillThere: when the delete itself fails, the
// operator gets both the cause and the leftover, by name.
func TestAFailedUndoSaysTheVolumeIsStillThere(t *testing.T) {
	f := newFakes(t)
	f.rel.upgradeErr = errors.New("release failed")
	f.store.deleteErr = errors.New("forbidden")

	_, err := f.apply(t)
	if err == nil {
		t.Fatal("apply succeeded although the release failed")
	}
	for _, want := range []string{"release failed", "PersistentVolume testbox-s5n-data was created by this run and is still there"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q: %v", want, err)
		}
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

// Values box derives from the cluster are overlaid after both files and before
// the diff, so the release Helm stores is the same document a re-run computes.
func TestBoxValuesAreOverlaidOnTheEffectiveValues(t *testing.T) {
	f := newFakes(t)
	peers := []any{map[string]any{
		"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "s5n-gateway"}},
	}}
	_, err := f.app.Apply(context.Background(), Options{
		Chart:       testChartPtr(t),
		GrantPath:   f.grantPath,
		ValuesPath:  f.values,
		PathsInNode: f.pathsInNode,
		BoxValues: func(context.Context) (map[string]any, error) {
			return map[string]any{"networkPolicy": map[string]any{"gatewayFrom": peers}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.rel.upgraded) != 1 {
		t.Fatalf("upgrades = %d, want one", len(f.rel.upgraded))
	}
	np, _ := f.rel.upgraded[0]["networkPolicy"].(map[string]any)
	got, _ := np["gatewayFrom"].([]any)
	if len(got) != 1 {
		t.Fatalf("networkPolicy.gatewayFrom = %v, want box's peer", np["gatewayFrom"])
	}
	if enabled, _ := np["enabled"].(bool); !enabled {
		t.Fatal("the overlay dropped networkPolicy.enabled")
	}
}

// A derivation failure is a refusal before anything is opened or written: box
// will not guess a NetworkPolicy peer, and it will not install a release whose
// apps the Gateway cannot reach.
func TestBoxValuesRefusalWritesNothing(t *testing.T) {
	f := newFakes(t)
	_, err := f.app.Apply(context.Background(), Options{
		Chart:       testChartPtr(t),
		GrantPath:   f.grantPath,
		ValuesPath:  f.values,
		PathsInNode: f.pathsInNode,
		BoxValues: func(context.Context) (map[string]any, error) {
			return nil, errors.New("the Gateway's address is not held by any LoadBalancer Service")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "NetworkPolicy") && !strings.Contains(err.Error(), "Gateway") {
		t.Fatalf("want the derivation refusal, got %v", err)
	}
	if f.openCalls != 0 || f.storeOpens != 0 || len(f.rel.upgraded) != 0 || len(f.events) != 0 {
		t.Fatalf("a derivation refusal touched the cluster: opens=%d stores=%d upgrades=%d events=%v",
			f.openCalls, f.storeOpens, len(f.rel.upgraded), f.events)
	}
}

// Undo is cleanup: a run cancelled after it wrote a volume must still take the
// volume off, or the next apply refuses over a PV nobody's release owns. The
// fake fails a Delete on a cancelled context the way a real client does, so
// this test pins that undo does not hand the cancellation down.
func TestUndoRunsAfterCancellation(t *testing.T) {
	f := newFakes(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pv := StorageObject{Kind: KindPersistentVolume, Name: "seedbox-data"}
	cause := errors.New("the step was cancelled")

	err := f.app.undo(ctx, f.store, []StorageObject{pv}, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("undo = %v, want the original cause", err)
	}
	if len(f.store.deleted) != 1 {
		t.Fatalf("deleted %d volumes with a cancelled context, want the one this run wrote", len(f.store.deleted))
	}
	if !strings.Contains(f.out.String(), "removed PersistentVolume seedbox-data") {
		t.Fatalf("the transcript does not report the removal:\n%s", f.out.String())
	}
	// Detaching from the cancellation drops the deadline with it; undo must
	// carry its own, or a silent API server holds the failed run open.
	if len(f.store.deleteBounded) != 1 || !f.store.deleteBounded[0] {
		t.Fatalf("undo's Delete had no deadline: %v", f.store.deleteBounded)
	}
}
