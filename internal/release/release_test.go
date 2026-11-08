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
	return f.revision, nil
}

func (f *fakeRelease) Receipt(context.Context) (Receipt, error) {
	if f.receiptErr != nil {
		return Receipt{}, f.receiptErr
	}
	return f.receipt, nil
}

type fakes struct {
	app       *Applier
	rel       *fakeRelease
	out       *bytes.Buffer
	grantPath string
	values    string
	openCalls int
	namespace string
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
		out:       &bytes.Buffer{},
		grantPath: filepath.Join(dir, "grant.yaml"),
		values:    filepath.Join(dir, "values.yaml"),
	}
	copyFile(t, "testdata/grant.yaml", f.grantPath)
	copyFile(t, "testdata/values.yaml", f.values)

	f.app = &Applier{
		Charts:  DirChart{},
		Confirm: &Terminal{Yes: true},
		Log:     f.out,
		Open: func(ns string) (ReleaseClient, error) {
			f.openCalls++
			f.namespace = ns
			return f.rel, nil
		},
	}
	return f
}

func (f *fakes) apply(t *testing.T) (Result, error) {
	t.Helper()
	return f.app.Apply(context.Background(), Options{
		ChartDir:   "testdata/chart",
		GrantPath:  f.grantPath,
		ValuesPath: f.values,
	})
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
	if got := vals["storage"].(map[string]any)["data"].(map[string]any)["size"]; got != "10Gi" {
		t.Fatalf("storage.data.size = %v, want the values file's 10Gi", got)
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
	f.writeValues(t, "storage:\n  data:\n    size: 12Gi\n")

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("changed apply: %v\n%s", err, f.out)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "storage.data.size" {
		t.Fatalf("changes = %+v, want exactly storage.data.size", res.Changes)
	}
	if !strings.Contains(f.out.String(), `~ storage.data.size: "10Gi" -> "12Gi"`) {
		t.Fatalf("the diff was not printed:\n%s", f.out)
	}
	if f.rel.upgrades != 2 {
		t.Fatalf("upgrades = %d, want 2", f.rel.upgrades)
	}
	last := f.rel.upgraded[len(f.rel.upgraded)-1]
	if got := last["storage"].(map[string]any)["data"].(map[string]any)["size"]; got != "12Gi" {
		t.Fatalf("applied storage.data.size = %v, want 12Gi", got)
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
