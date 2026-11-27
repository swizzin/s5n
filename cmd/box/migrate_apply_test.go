package main

// migrate_apply_test.go drives apply's switches, ownership record and refusals.

import (
	"bytes"
	"context"
	"fmt"
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
	if err := ed.Enable("sonarr-4k", plan.App{}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// One it does not carry is appended, with the instance's own name as its
	// category.
	if err := ed.Enable("sonarr-1080", plan.App{}); err != nil {
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

func TestLoadPlanNamesTheSnapshot(t *testing.T) {
	snapshot := "/var/lib/s5n/migrate/migrate-20261117T090000Z-final.tar.gz"
	if _, err := loadPlan(&bundle.Opened{Path: snapshot, Plan: []byte("version: 3\napps: {}\n")}); err == nil {
		t.Fatal("a plan of another version was accepted")
	} else if !strings.Contains(err.Error(), snapshot+":"+archive.PlanName) {
		t.Errorf("the refusal does not name the snapshot: %v", err)
	}
}

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
	if err := ed.Enable("sonarr-4k", plan.App{}); err != nil {
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

func TestTheSnapshotIsNamedInTheLogAndNotOnTheScreen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate-20261123T045038Z-final.tar.gz")
	planYAML := []byte(`version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths: []
`)
	if _, err := bundle.Write(path, bundle.Snapshot{Mode: archive.ModeFinal, Plan: planYAML}); err != nil {
		t.Fatalf("write the snapshot: %v", err)
	}
	var screen, log bytes.Buffer
	opened, cleanup, err := openSnapshot(path, &say.Plain{W: &screen, Log: &log})
	cleanup()
	if err != nil {
		t.Fatalf("openSnapshot: %v", err)
	}
	if opened.Path != path {
		t.Fatalf("the run read %s, want %s", opened.Path, path)
	}
	if got := screen.String(); got != "" {
		t.Fatalf("the screen says %q before the run has done anything, want nothing: a refusal opens with its fact", got)
	}
	if want := fmt.Sprintf("snapshot: %s (final)\n", path); log.String() != want {
		t.Fatalf("the log says %q, want %q", log.String(), want)
	}
}

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
	if err := ed.Enable("sonarr", plan.App{DownloadCategory: "TV"}); err != nil {
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
	if err := ed.Enable("sonarr-4k", plan.App{DownloadCategory: "4K/TV"}); err != nil {
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

// A Seerr has no library directory, so an instance switched on for it carries no category:
// its schema refuses one, and a values file the chart will not render is not a migration.
func TestSwitchingOnASeerrWritesNoCategory(t *testing.T) {
	list := []any{map[string]any{"suffix": "", "enabled": true}}
	out := switchOn(list, "jellyfin", "seerr", nil)
	if len(out) != 2 {
		t.Fatalf("instances = %v, want the default and the new one", out)
	}
	entry, _ := out[1].(map[string]any)
	if entry["suffix"] != "jellyfin" || entry["enabled"] != true {
		t.Fatalf("the new instance = %v", entry)
	}
	if _, ok := entry["category"]; ok {
		t.Fatalf("the new Seerr instance carries a category: %v", entry)
	}
	// The same call for an Arr still names the directory the instance writes.
	arr := switchOn([]any{map[string]any{"suffix": "", "category": "tv", "enabled": true}}, "4k", "sonarr", nil)
	got, _ := arr[1].(map[string]any)
	if got["category"] != "4k" {
		t.Fatalf("sonarr-4k's category = %v, want the suffix", got["category"])
	}
}

// A migrated Seerr carries the config that signs in to one media server, so the
// instance it lands as is that server's: the plan's mediaServer is written on
// whichever entry the editor switches on, and a Seerr entry never gets a category.
func TestGrantEditorGivesASeerrItsServer(t *testing.T) {
	dir := t.TempDir()
	grantPath := filepath.Join(dir, "root.yaml")
	grant := "apps:\n  seerr:\n    enabled: true\n    instances:\n      - suffix: \"\"\n        enabled: false\n"
	if err := os.WriteFile(grantPath, []byte(grant), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := map[string]any{"apps": map[string]any{"seerr": map[string]any{
		"instances": []any{map[string]any{"suffix": "", "enabled": true}},
	}}}
	ed := &grantEditor{sb: sb, catalog: []string{"seerr"}, values: chart}
	if err := ed.Enable("seerr", plan.App{MediaServer: "plex"}); err != nil {
		t.Fatalf("enable the entry the grant carries: %v", err)
	}
	if err := ed.Enable("seerr-jellyfin", plan.App{MediaServer: "jellyfin"}); err != nil {
		t.Fatalf("append one: %v", err)
	}
	back, err := seedbox.Load(grantPath, filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatalf("the edited grant does not load: %v", err)
	}
	apps, _ := back.Grant.Map()["apps"].(map[string]any)
	list, _ := apps["seerr"].(map[string]any)["instances"].([]any)
	if len(list) != 2 {
		t.Fatalf("instances = %v, want seerr and seerr-jellyfin", list)
	}
	for i, want := range []string{"plex", "jellyfin"} {
		entry, _ := list[i].(map[string]any)
		if entry["mediaServer"] != want || entry["enabled"] != true {
			t.Errorf("instance %d = %v, want it on and set up against %s", i, entry, want)
		}
		if _, ok := entry["category"]; ok {
			t.Errorf("instance %d carries a category, which Seerr's schema refuses: %v", i, entry)
		}
	}

	// A grant with no list of its own takes the chart's, with the server on the
	// entry the plan names.
	if err := os.WriteFile(grantPath, []byte("apps:\n  seerr:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sb, err = seedbox.Load(grantPath, filepath.Join(dir, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"apps": map[string]any{"seerr": map[string]any{
		"instances": []any{map[string]any{"suffix": "", "enabled": true}},
	}}}
	ed = &grantEditor{sb: sb, catalog: []string{"seerr"}, values: values}
	if err := ed.Enable("seerr", plan.App{MediaServer: "jellyfin"}); err != nil {
		t.Fatalf("enable from the chart's list: %v", err)
	}
	if back, err = seedbox.Load(grantPath, filepath.Join(dir, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	apps, _ = back.Grant.Map()["apps"].(map[string]any)
	list, _ = apps["seerr"].(map[string]any)["instances"].([]any)
	if entry, _ := list[0].(map[string]any); entry["mediaServer"] != "jellyfin" {
		t.Errorf("seerr = %v, want it set up against jellyfin", entry)
	}
	// The chart's own list is not edited in place.
	if _, ok := values["apps"].(map[string]any)["seerr"].(map[string]any)["instances"].([]any)[0].(map[string]any)["mediaServer"]; ok {
		t.Error("the editor wrote the server onto the chart's default list")
	}
}
