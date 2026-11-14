package selfupdate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRollbackPutsThePreviousBinaryBack(t *testing.T) {
	path := installDir(t, "new box")
	previous := path + ".previous"
	if err := os.WriteFile(previous, []byte("old box"), 0o755); err != nil {
		t.Fatalf("writing the previous box: %v", err)
	}

	if err := Rollback(path); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	assertContent(t, path, []byte("old box"))
	// One step back only: the binary that was replaced is gone, and there is
	// nothing left to go back to.
	assertDir(t, path, "box")
	err := Rollback(path)
	if err == nil {
		t.Fatal("Rollback went back a second time")
	}
	if !strings.Contains(err.Error(), "no box.previous") {
		t.Fatalf("Rollback refused with %q, want the missing-previous refusal", err)
	}
}

func TestRollbackWithoutAPreviousBinary(t *testing.T) {
	path := installDir(t, "only box")

	err := Rollback(path)
	if err == nil {
		t.Fatal("Rollback put back a binary that was never kept")
	}
	if !strings.Contains(err.Error(), "box has not replaced itself here") {
		t.Fatalf("Rollback refused with %q, want the never-replaced refusal", err)
	}
	assertContent(t, path, []byte("only box"))
}

func TestReexecMarksTheNewVersion(t *testing.T) {
	previous := execProcess
	var path string
	var argv, env []string
	execProcess = func(called string, args, environ []string) error {
		path, argv, env = called, args, environ
		return nil
	}
	t.Cleanup(func() { execProcess = previous })

	// The old mark is the version this process was re-executed with; the new
	// one has to replace it, not sit beside it.
	supplied := []string{"PATH=/usr/bin", ReexecEnv + "=0.4.0", "HOME=/root"}
	if err := Reexec("/usr/local/bin/box", []string{"update", "--yes"}, supplied, "0.5.0"); err != nil {
		t.Fatalf("Reexec: %v", err)
	}
	if path != "/usr/local/bin/box" {
		t.Fatalf("Reexec exec'd %s, want the binary it was given", path)
	}
	if want := []string{"/usr/local/bin/box", "update", "--yes"}; !slices.Equal(argv, want) {
		t.Fatalf("Reexec exec'd with argv %v, want %v", argv, want)
	}
	if want := []string{"PATH=/usr/bin", "HOME=/root", ReexecEnv + "=0.5.0"}; !slices.Equal(env, want) {
		t.Fatalf("Reexec exec'd with env %v, want %v", env, want)
	}
	// The argument the caller was given is not modified in place.
	if want := []string{"PATH=/usr/bin", ReexecEnv + "=0.4.0", "HOME=/root"}; !slices.Equal(supplied, want) {
		t.Fatalf("Reexec changed the caller's environment to %v", supplied)
	}
}

func TestReexecRefusesWithoutAVersion(t *testing.T) {
	previous := execProcess
	execProcess = func(string, []string, []string) error {
		t.Fatal("Reexec exec'd without a version to mark the process with")
		return nil
	}
	t.Cleanup(func() { execProcess = previous })

	if err := Reexec(filepath.Join(t.TempDir(), "box"), nil, nil, " "); err == nil {
		t.Fatal("Reexec ran with an empty version")
	}
}
