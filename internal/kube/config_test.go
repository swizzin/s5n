package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// writeKubeconfig puts a minimal, valid kubeconfig on disk. Nothing here
// contacts the server: building a client config is a file read.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := `apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: not-a-real-token
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfigInstallsTheWarningHandler is the property the five hand-written
// loaders each had to remember: client-go's deprecation notices go through
// klog, and box's output is its own. It is on the config, so a client built
// from it cannot be the one that forgot.
func TestConfigInstallsTheWarningHandler(t *testing.T) {
	cfg, err := Config(writeKubeconfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WarningHandler == nil {
		t.Fatal("Config returned a config with no warning handler: client-go warnings would print")
	}
	if _, ok := cfg.WarningHandler.(rest.NoWarnings); !ok {
		t.Fatalf("warning handler = %T, want rest.NoWarnings", cfg.WarningHandler)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Fatalf("host = %q, want the kubeconfig's server", cfg.Host)
	}
}

// TestConfigFromBytesInstallsTheWarningHandler is the probe's half: bootstrap
// has the kubeconfig's bytes, not a path.
func TestConfigFromBytesInstallsTheWarningHandler(t *testing.T) {
	raw := []byte(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context: {cluster: test, user: test}
users:
- name: test
  user: {token: not-a-real-token}
`)
	cfg, err := ConfigFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.WarningHandler.(rest.NoWarnings); !ok {
		t.Fatalf("warning handler = %T, want rest.NoWarnings", cfg.WarningHandler)
	}
}

// TestFlagsWrapHelmsConfigs is how Helm inherits the policy: it builds its own
// configs through the getter, so the getter has to wrap each one.
func TestFlagsWrapHelmsConfigs(t *testing.T) {
	flags := Flags(writeKubeconfig(t), "s5n")
	cfg, err := flags.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.WarningHandler.(rest.NoWarnings); !ok {
		t.Fatalf("Helm's config warning handler = %T, want rest.NoWarnings", cfg.WarningHandler)
	}
	ns, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		t.Fatal(err)
	}
	if ns != "s5n" {
		t.Fatalf("namespace = %q, want s5n", ns)
	}
}

// TestConfigRefusesAnEmptyPath pins the stricter behaviour both loaders used to
// have (and one lost): client-go would fall back to KUBECONFIG or
// ~/.kube/config, which is a cluster box was never told to touch.
func TestConfigRefusesAnEmptyPath(t *testing.T) {
	if _, err := Config(""); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("err = %v, want a refusal for an empty path", err)
	}
}

// TestConfigExplicitPathMustExist keeps a typo an error rather than a fallback.
func TestConfigExplicitPathMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := Config(missing); err == nil {
		t.Fatal("a missing kubeconfig was accepted")
	}
}
