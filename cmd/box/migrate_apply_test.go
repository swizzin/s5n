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

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
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
	if err := ed.Enable("sonarr-4k", ""); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// One it does not carry is appended, with the instance's own name as its
	// category.
	if err := ed.Enable("sonarr-1080", ""); err != nil {
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

// TestLoadPlanNamesTheSnapshot: a plan a person edited has to fail with the
// snapshot and the entry it came from, not with a bare parse error.
func TestLoadPlanNamesTheSnapshot(t *testing.T) {
	snapshot := "/var/lib/s5n/migrate/migrate-20261117T090000Z-final.tar.gz"
	if _, err := loadPlan(&bundle.Opened{Path: snapshot, Plan: []byte("version: 3\napps: {}\n")}); err == nil {
		t.Fatal("a plan of another version was accepted")
	} else if !strings.Contains(err.Error(), snapshot+":"+archive.PlanName) {
		t.Errorf("the refusal does not name the snapshot: %v", err)
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
	if err := ed.Enable("sonarr-4k", ""); err != nil {
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

// TestGrantLocked: a rehearsal box's lock is the grant's flag, and a host with
// no grant at all is not locked. A final snapshot never applies to a locked
// box, so reading this correctly is what keeps a disposable rehearsal from
// becoming a live one.
func TestGrantLocked(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")
	if on, err := grantLocked(missing); err != nil || on {
		t.Fatalf("grantLocked(absent) = %v, %v; want false, nil", on, err)
	}
	locked := filepath.Join(dir, "locked.yaml")
	if err := os.WriteFile(locked, []byte("namespace: seedbox\nrehearsal: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, err := grantLocked(locked); err != nil || !on {
		t.Errorf("grantLocked(locked) = %v, %v; want true, nil", on, err)
	}
	open := filepath.Join(dir, "open.yaml")
	if err := os.WriteFile(open, []byte("namespace: seedbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, err := grantLocked(open); err != nil || on {
		t.Errorf("grantLocked(open) = %v, %v; want false, nil", on, err)
	}
}

// TestRehearsalSourceHost: a rehearsal must not run on the host that holds the
// source's data. Both the plan's source host and a non-empty data root say so,
// and either alone is enough.
func TestRehearsalSourceHost(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname")
	}
	planFor := func(sourceHost, root string) *plan.Plan {
		var doc string
		if root != "" {
			doc = "version: 2\nsource:\n  host: " + sourceHost + "\n  taken: 2026-11-17T00:00:00Z\n  kinds: [kubernetes]\ndata:\n  root: " + root + "\n  downloads: Downloads\n  media: library\napps: {}\n"
		} else {
			doc = "version: 2\nsource:\n  host: " + sourceHost + "\n  taken: 2026-11-17T00:00:00Z\n  kinds: [kubernetes]\napps: {}\n"
		}
		p, err := plan.Parse([]byte(doc))
		if err != nil {
			t.Fatalf("parse plan: %v", err)
		}
		return p
	}

	if err := rehearsalSourceHost(planFor(host, "")); err == nil {
		t.Fatal("a plan naming this host as its source was accepted")
	} else if !strings.Contains(err.Error(), "rehearse on another host or VM") {
		t.Errorf("refusal = %v, want it to name the way forward", err)
	}
	if err := rehearsalSourceHost(planFor("elsewhere.example.com", "")); err != nil {
		t.Errorf("a plan from another host with no data here was refused: %v", err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Downloads"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rehearsalSourceHost(planFor("elsewhere.example.com", root)); err == nil {
		t.Fatal("a non-empty data root on this host was accepted")
	}
}

// TestEnabledApps: the refusal for a box that already runs something reads the
// grant, which is what decides existence, and names every app it found.
func TestEnabledApps(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant.yaml")
	doc := "namespace: seedbox\ngateway:\n  host: box.example.com\napps:\n  sonarr:\n    enabled: true\n  prowlarr:\n    enabled: false\n  qbittorrent:\n    enabled: true\n"
	if err := os.WriteFile(grant, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grant, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(enabledApps(sb), ","); got != "qbittorrent,sonarr" {
		t.Errorf("enabled apps = %q, want qbittorrent,sonarr", got)
	}
}

// TestOpenSnapshotTakesARehearsal: apply runs a rehearsal now, so the mode is
// no longer a refusal; a survey still is, because it holds nothing to rewrite.
func TestOpenSnapshotTakesARehearsal(t *testing.T) {
	dir := t.TempDir()
	planYAML := []byte(`version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths: []
`)
	for _, tc := range []struct {
		mode archive.Mode
		ok   bool
	}{
		{archive.ModeRehearsal, true},
		{archive.ModeFinal, true},
		{archive.ModeSurvey, false},
	} {
		path := filepath.Join(dir, string(tc.mode)+".tar.gz")
		if _, err := bundle.Write(path, bundle.Snapshot{Mode: tc.mode, Plan: planYAML}); err != nil {
			t.Fatalf("write %s snapshot: %v", tc.mode, err)
		}
		opened, cleanup, err := openSnapshot(path, &say.Plain{})
		cleanup()
		if tc.ok {
			if err != nil {
				t.Errorf("%s: openSnapshot: %v", tc.mode, err)
				continue
			}
			if opened.Mode != tc.mode {
				t.Errorf("%s: mode = %q", tc.mode, opened.Mode)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: openSnapshot accepted it", tc.mode)
		}
	}
}

// TestGrantEditorKeepsTheMigratedDownloadCategory: a migrated instance's library
// directory is box's own (tv), and the category its torrents are in is the
// source's (TV). The switch-on writes both, so the release renders a root folder
// at /data/media/tv and a qBittorrent category TV whose save path is where the
// torrents already are.
func TestGrantEditorKeepsTheMigratedDownloadCategory(t *testing.T) {
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
			map[string]any{"suffix": "", "category": "tv", "enabled": false},
			map[string]any{"suffix": "4k", "category": "tv-4k", "enabled": false},
		},
	}}}
	ed := &grantEditor{sb: sb, catalog: []string{"sonarr"}, values: values}
	if err := ed.Enable("sonarr", "TV"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	back, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatalf("the edited grant does not load: %v", err)
	}
	apps, _ := back.Grant.Map()["apps"].(map[string]any)
	list, _ := apps["sonarr"].(map[string]any)["instances"].([]any)
	entry, _ := list[0].(map[string]any)
	if got := entry["downloadCategory"]; got != "TV" {
		t.Errorf("the instance's downloadCategory is %v, want the source's TV", got)
	}
	if got := entry["category"]; got != "tv" {
		t.Errorf("the instance's category is %v, want box's tv: the library is renamed onto it", got)
	}
	if on, _ := entry["enabled"].(bool); !on {
		t.Error("the instance is off")
	}
	if got := list[1].(map[string]any)["downloadCategory"]; got != nil {
		t.Errorf("the instance the plan does not name was given a downloadCategory: %v", got)
	}

	// A grant that already carries its own list takes the same key, on the
	// entry the plan names.
	if err := ed.Enable("sonarr-4k", "4K/TV"); err != nil {
		t.Fatalf("enable the second: %v", err)
	}
	back, err = seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatalf("the edited grant does not load: %v", err)
	}
	apps, _ = back.Grant.Map()["apps"].(map[string]any)
	list, _ = apps["sonarr"].(map[string]any)["instances"].([]any)
	if got := list[1].(map[string]any)["downloadCategory"]; got != "4K/TV" {
		t.Errorf("the second instance's downloadCategory is %v, want 4K/TV", got)
	}
}
