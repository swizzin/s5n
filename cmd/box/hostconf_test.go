package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bootstrapStrictChildEnv marks the child process
// TestBootstrapRefusesAFilesTypoBeforeTheCluster starts: bootstrap ends the
// process on a config it cannot parse, which would take the test binary with it.
const bootstrapStrictChildEnv = "BOX_BOOTSTRAP_CONF_CHILD"

func TestBootstrapRefusesAFilesTypoBeforeTheCluster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("box runs on Linux hosts")
	}
	if os.Getenv(bootstrapStrictChildEnv) != "" {
		t.Skip("child process, run by TestBootstrapRefusesAFilesTypoBeforeTheCluster")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapStrictChild$")
	// A KUBECONFIG in the test environment would take the explicit-cluster
	// branch, whose refusal is not the one this test is about.
	cmd.Env = append(withoutEnv(os.Environ(), "KUBECONFIG"), bootstrapStrictChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("bootstrap with a typo in root's config exited %v (want 1):\n%s", err, out)
	}
	if !strings.Contains(string(out), `box.yaml:2: unknown key "update.timmer"`) {
		t.Fatalf("bootstrap did not name the file, line and key:\n%s", out)
	}
	if strings.Contains(string(out), "decision:") {
		t.Fatalf("bootstrap read a config it cannot parse and went on to decide the cluster:\n%s", out)
	}
}

func TestBootstrapStrictChild(t *testing.T) {
	if os.Getenv(bootstrapStrictChildEnv) == "" {
		t.Skip("child process, run by TestBootstrapRefusesAFilesTypoBeforeTheCluster")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "box.yaml"), []byte("update:\n  timmer: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runBootstrap(context.Background(), []string{"--dry-run", "--record", filepath.Join(dir, "cluster.yaml")})
	t.Fatal("bootstrap returned without refusing the config it cannot parse")
}
