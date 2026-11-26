package seedbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The user's own keys win over the chart's, every app stays off because no grant
// switched one on, and nothing of a grant is in the document.
func TestPendingIsTheChartWithTheUsersValues(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(values, []byte("apps:\n  sonarr:\n    port: 18989\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Pending(realChart(t), values)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	apps, _ := got["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	if port, _ := sonarr["port"].(int); port != 18989 {
		t.Errorf("apps.sonarr.port = %v, want the user's 18989", sonarr["port"])
	}
	for name, raw := range apps {
		entry, _ := raw.(map[string]any)
		if on, _ := entry["enabled"].(bool); on {
			t.Errorf("apps.%s is enabled, and no grant has switched anything on", name)
		}
	}
	// The grant's keys are box's record and none exist yet: the chart carries
	// `storage` and `rehearsal` as shapes, but the paths, claim names, namespace
	// and lock are the grant's.
	if _, ok := got["namespace"]; ok {
		t.Error("the pending values carry a namespace, which is a grant key")
	}
	if storage, _ := got["storage"].(map[string]any); storage != nil {
		data, _ := storage["data"].(map[string]any)
		config, _ := storage["config"].(map[string]any)
		for key, entry := range map[string]map[string]any{"data": data, "config": config} {
			if entry == nil {
				continue
			}
			if _, ok := entry["path"]; ok {
				t.Errorf("the pending values carry storage.%s.path, which is the grant's", key)
			}
		}
	}
	if on, _ := got["rehearsal"].(bool); on {
		t.Error("the pending values are locked, and this host has no grant saying so")
	}
}

// A preview must not refuse a box the install would have made: the schema requires
// auth.edge, which box writes rather than either file.
func TestPendingIsNotCheckedAgainstTheSchema(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	if _, err := Pending(realChart(t), values); err != nil {
		t.Fatalf("Pending of a host with no values file: %v", err)
	}
}

// A values file naming a key root decides is refused when the box reads its two
// files, and the preview does not carry it either.
func TestPendingIgnoresBoxOnlyKeysInValues(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	// storage.data.path is box's record of where the data is.
	if err := os.WriteFile(values, []byte("storage:\n  data:\n    path: /srv/elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Pending(realChart(t), values)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if storage, ok := got["storage"].(map[string]any); ok {
		if data, ok := storage["data"].(map[string]any); ok && data["path"] != nil {
			t.Errorf("the pending values carry a grant path from the user's file: %v", data)
		}
	}
}
