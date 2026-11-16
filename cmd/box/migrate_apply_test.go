package main

// migrate_apply_test.go drives the pieces `box migrate apply` builds for
// internal/migrate/apply: the two switches it writes (the grant's instance keys
// and the release Secret's per-app keys), the ownership record, and the plan
// loader's refusals. The run itself is internal/migrate/apply's own tests.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/swizzin/s5n/internal/seedbox"
)

// TestGrantEditorEnablesAnInstance: the switch-on writes the grant, one atomic
// edit for both keys, and leaves the comments and the rest of the file alone.
func TestGrantEditorEnablesAnInstance(t *testing.T) {
	dir := t.TempDir()
	grantPath := filepath.Join(dir, "root.yaml")
	grant := `# root's decision for this box
namespace: seedbox
login: someone
apps:
  sonarr:
    enabled: false
    instances:
      - suffix: ""
        category: tv
        enabled: false
      - suffix: 4k
        category: tv-4k
        enabled: false
`
	if err := os.WriteFile(grantPath, []byte(grant), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{
		"instances": []any{
			map[string]any{"suffix": "", "category": "tv", "enabled": false},
			map[string]any{"suffix": "4k", "category": "tv-4k", "enabled": false},
			map[string]any{"suffix": "1080", "category": "tv-1080", "enabled": false},
		},
	}}}
	ed := &grantEditor{sb: sb, catalog: []string{"sonarr", "qbittorrent"}, values: values}

	// An entry the chart's list carries is switched on where it sits.
	if err := ed.Enable("sonarr-4k"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// One it does not carry is appended, with the instance's own name as its
	// category.
	if err := ed.Enable("sonarr-1080"); err != nil {
		t.Fatalf("enable a new instance: %v", err)
	}
	raw, err := os.ReadFile(grantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# root's decision for this box\n") {
		t.Errorf("the comment did not survive:\n%s", raw)
	}
	back, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatalf("the edited grant does not load: %v", err)
	}
	apps, _ := back.Grant.Map()["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	if on, _ := sonarr["enabled"].(bool); !on {
		t.Error("apps.sonarr.enabled is still false")
	}
	list, _ := sonarr["instances"].([]any)
	if len(list) != 3 {
		t.Fatalf("instances has %d entries, want 3", len(list))
	}
	if on, _ := list[0].(map[string]any)["enabled"].(bool); on {
		t.Error("the primary instance was switched on, and the plan does not name it")
	}
	for i, want := range []string{"", "4k", "1080"} {
		entry, _ := list[i].(map[string]any)
		if entry["suffix"] != want {
			t.Errorf("entry %d is suffix %v, want %q", i, entry["suffix"], want)
		}
		if i == 0 {
			continue
		}
		if on, _ := entry["enabled"].(bool); !on {
			t.Errorf("entry %q is still disabled", want)
		}
	}
	if got := list[2].(map[string]any)["category"]; got != "1080" {
		t.Errorf("the appended entry's category is %v, want 1080", got)
	}
}

// TestLoadPlanNamesTheFile: a plan a person edited has to fail with the file it
// came from, not with a bare parse error.
func TestLoadPlanNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := loadPlan(dir); err == nil || !strings.Contains(err.Error(), "plan.yaml") {
		t.Fatalf("an empty plan directory was refused with %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.yaml"), []byte("version: 2\napps: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadPlan(dir)
	if err == nil {
		t.Fatal("a plan of another version was accepted")
	}
	if !strings.Contains(err.Error(), "plan.yaml") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// TestGrantEditorWritesTheListWhenTheGrantHasNone: a grant written before the
// instance existed names no list at all, so the one edit writes the list the
// release would otherwise render, with the migrated instance on.
func TestGrantEditorWritesTheListWhenTheGrantHasNone(t *testing.T) {
	dir := t.TempDir()
	grantPath := filepath.Join(dir, "root.yaml")
	if err := os.WriteFile(grantPath, []byte("apps:\n  sonarr:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{
		"instances": []any{
			map[string]any{"suffix": "", "category": "tv", "enabled": true},
			map[string]any{"suffix": "4k", "category": "tv-4k", "enabled": false},
		},
	}}}
	ed := &grantEditor{sb: sb, catalog: []string{"sonarr"}, values: values}
	if err := ed.Enable("sonarr-4k"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	back, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatalf("the edited grant does not load: %v", err)
	}
	apps, _ := back.Grant.Map()["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	list, ok := sonarr["instances"].([]any)
	if !ok {
		t.Fatalf("the grant names no instances after the edit:\n%v", sonarr)
	}
	if len(list) != 2 {
		t.Fatalf("instances has %d entries, want the chart's two", len(list))
	}
	if on, _ := list[1].(map[string]any)["enabled"].(bool); !on {
		t.Error("the 4k entry the plan names is off")
	}
	if on, _ := list[0].(map[string]any)["enabled"].(bool); !on {
		t.Error("the primary instance the chart had on was switched off")
	}
}

// TestSecretStoreKeepsAKey: the release Secret is read and written one key at a
// time, with the resourceVersion the read carried, and the rest of the Secret
// is left alone.
func TestSecretStoreKeepsAKey(t *testing.T) {
	cs := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s5n-keys", Namespace: "seedbox"},
		Data:       map[string][]byte{"qbittorrent-password": []byte("generated")},
	})
	st := &secretStore{cs: cs, namespace: "seedbox", name: releaseSecretName(nil)}
	if st.name != "s5n-keys" {
		t.Fatalf("secret name is %q, want the chart's s5n-keys", st.name)
	}
	if got, err := st.Get(context.Background(), "sonarr-apikey"); err != nil || got != "" {
		t.Fatalf("Get of an absent key = %q, %v", got, err)
	}
	if err := st.Set(context.Background(), "sonarr-apikey", "0123456789abcdef"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := st.Get(context.Background(), "sonarr-apikey"); err != nil || got != "0123456789abcdef" {
		t.Errorf("the key reads back as %q, %v", got, err)
	}
	sec, err := cs.CoreV1().Secrets("seedbox").Get(context.Background(), "s5n-keys", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data["qbittorrent-password"]) != "generated" {
		t.Error("the chart's own key was dropped")
	}
	// An operator's own Secret is the one the chart was told to use.
	if got := releaseSecretName(map[string]any{"secrets": map[string]any{"existingSecret": "mine"}}); got != "mine" {
		t.Errorf("existingSecret names %q, want mine", got)
	}
}

// TestWiringRecordRoundTrips: owned.json is one ConfigMap key, and writing it
// keeps the wiring.json the chart rendered beside it.
func TestWiringRecordRoundTrips(t *testing.T) {
	cs := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "s5n-wiring", Namespace: "seedbox"},
		Data:       map[string]string{"wiring.json": "{}"},
	})
	rec := &wiringRecord{cs: cs, namespace: "seedbox", name: wiringConfigMap}
	if raw, err := rec.Read(context.Background()); err != nil || len(raw) != 0 {
		t.Fatalf("Read of an absent record = %q, %v", raw, err)
	}
	if err := rec.Write(context.Background(), []byte(`{"version":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := rec.Read(context.Background())
	if err != nil || string(raw) != `{"version":1}` {
		t.Errorf("the record reads back as %q, %v", raw, err)
	}
	cm, err := cs.CoreV1().ConfigMaps("seedbox").Get(context.Background(), "s5n-wiring", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["wiring.json"] != "{}" {
		t.Error("the chart's wiring.json was dropped")
	}
}
