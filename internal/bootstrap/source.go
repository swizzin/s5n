package bootstrap

import (
	"os"
	"path/filepath"
)

// FilesystemSource reads kubeconfigs from the distribution conventions in
// PLAN.md's order, plus the user's ~/.kube/config.
type FilesystemSource struct{}

// Candidates returns the candidate paths in PLAN.md's order. A candidate only
// counts after the prober validates it; most will not exist on a given host.
func (FilesystemSource) Candidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	paths := []string{
		"/etc/rancher/k3s/k3s.yaml",
		"/etc/rancher/rke2/rke2.yaml",
		"/var/lib/k0s/pki/admin.conf",
		"/etc/kubernetes/admin.conf",
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".kube", "config"))
	}
	return paths
}

// Read returns the file contents, or an error when the path does not exist.
func (FilesystemSource) Read(path string) ([]byte, error) {
	return os.ReadFile(path)
}
