package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write replaces the file, ends at the mode the caller asked for, and leaves
// nothing of the temporary file behind.
func TestWriteReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte("wiring:\n  timeout: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("wiring:\n  timeout: 900\n"), 0o640); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "wiring:\n  timeout: 900\n" {
		t.Fatalf("file = %q, want the new bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
	assertNoLeftovers(t, path)
}

// A new file gets the caller's mode too: the record is 0600 and the app configs
// are 0600, and neither may be world-readable by the umask's accident.
func TestWriteCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seedbox.yaml")
	if err := Write(path, []byte("kubeconfig: /etc/rancher/k0s/k0s.yaml\n"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// A failed rename is the case this package exists for: the target still holds
// exactly its old bytes, and the temporary file is not left beside it.
func TestFailedRenameLeavesTheTargetWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.yaml")
	before := []byte("namespace: testbox\napps: [sonarr]\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	rename = func(_, _ string) error { return errors.New("simulated rename failure") }
	t.Cleanup(func() { rename = os.Rename })

	err := Write(path, []byte("namespace: testbox\napps: []\n"), 0o600)
	if err == nil {
		t.Fatal("Write reported success after the rename failed")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("the target changed after a failed rename: %q", after)
	}
	assertNoLeftovers(t, path)
}

// A missing parent directory is refused rather than silently written somewhere
// else: the callers that need one create it with their own mode.
func TestWriteRefusesAMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "values.yaml")
	if err := Write(path, []byte("x: 1\n"), 0o600); err == nil {
		t.Fatal("Write succeeded into a directory that is not there")
	}
}

// assertNoLeftovers is the temporary-file check every failure path needs: a
// refused write leaves nothing of the file it was making.
func assertNoLeftovers(t *testing.T, path string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}
