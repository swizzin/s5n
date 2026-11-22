package seedbox

// pending_test.go is the values a chart would render for a host whose grant is
// not written yet: the chart's defaults with the user's values over them, and no
// grant at all. A migration shows its one question before it installs the box
// that writes a grant, and the summary it shows has to read the values the run
// after the install will.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPendingIsTheChartWithTheUsersValues: the user's own keys win over the
// chart's, every app stays off because no grant switched one on, and nothing of
// a grant is in the document.
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
	// The grant's own keys are box's record, and none of them exist yet: the
	// chart's defaults carry `storage` and `rehearsal` as shapes, but the paths,
	// the claim names, the namespace and the lock are all the grant's.
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

// TestPendingIsNotCheckedAgainstTheSchema: a preview must not refuse a box the
// install would have made. The chart's schema requires auth.edge, which box
// writes rather than either file, so validating the pending document would
// refuse every host before it had a chance to be installed.
func TestPendingIsNotCheckedAgainstTheSchema(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	if _, err := Pending(realChart(t), values); err != nil {
		t.Fatalf("Pending of a host with no values file: %v", err)
	}
}

// TestPendingLeavesAGrantKeyInValuesToTheLoadThatRefusesIt: a values file naming
// a key root decides is refused when the box reads its two files, and the
// preview does not carry that key into the values a release would render.
func TestPendingIgnoresBoxOnlyKeysInValues(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	// storage.data.path is box's record of where the data is; a values file that
	// names it is refused by the merge, and it must not shape a preview either.
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
