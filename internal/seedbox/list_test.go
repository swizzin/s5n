package seedbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAListIsReplacedWhereItStands: a box-owned list whose value changed (a
// node added to the cluster) is rewritten at the list's own indentation, and the
// file still parses to the new list.
func TestAListIsReplacedWhereItStands(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant.yaml")
	start := "namespace: alice\napps:\n  jellyfin:\n    enabled: true\n    knownProxies:\n      - 10.244.0.0/24\n"
	if err := os.WriteFile(grant, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(grant, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("apps.jellyfin.knownProxies", []any{"10.244.0.0/24", "10.244.1.0/24"}); err != nil {
		t.Fatalf("replace the list: %v", err)
	}
	got, _ := os.ReadFile(grant)
	want := "    knownProxies:\n      - 10.244.0.0/24\n      - 10.244.1.0/24\n"
	if !strings.Contains(string(got), want) {
		t.Fatalf("grant =\n%s\nwant it to hold\n%s", got, want)
	}
}

// TestAOneEntryListIsWrittenUnderItsKey: a list value is a block, never a scalar
// on the key's line. A one-element sequence encodes as one line ("- app.db"),
// and "key: - app.db" is not YAML -- which is what a box with one proxy network,
// or any single-entry answer, would have written.
func TestAOneEntryListIsWrittenUnderItsKey(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant.yaml")
	if err := os.WriteFile(grant, []byte("namespace: alice\napps:\n  jellyfin:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(grant, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("apps.jellyfin.knownProxies", []any{"10.244.0.0/16"}); err != nil {
		t.Fatalf("one-entry list: %v", err)
	}
	got, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	want := "    knownProxies:\n      - 10.244.0.0/16\n"
	if !strings.Contains(string(got), want) {
		t.Fatalf("grant =\n%s\nwant it to hold\n%s", got, want)
	}
	// And it parses back to the list asked for.
	reloaded, err := Load(grant, "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := reloaded.SetGrant("apps.jellyfin.knownProxies", []any{"10.244.1.0/16"}); err != nil {
		t.Fatalf("replace a one-entry list: %v", err)
	}
}
