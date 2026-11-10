package bootstrap

import (
	"os"
	"path/filepath"
)

// FilesystemSource reads kubeconfigs from the distribution conventions in
// PLAN.md's order, plus the user's ~/.kube/config.
type FilesystemSource struct{}

// Candidates returns the candidate paths in PLAN.md's order, each marked with
// whether the distribution that owns this host writes it. A candidate only
// counts after the prober validates it; most will not exist on a given host.
func (FilesystemSource) Candidates() []Candidate {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	paths := []Candidate{
		{Path: "/etc/rancher/k3s/k3s.yaml", DistributionOwned: true},
		{Path: "/etc/rancher/rke2/rke2.yaml", DistributionOwned: true},
		{Path: "/var/lib/k0s/pki/admin.conf", DistributionOwned: true},
		{Path: "/etc/kubernetes/admin.conf", DistributionOwned: true},
	}
	if home != "" {
		paths = append(paths, Candidate{Path: filepath.Join(home, ".kube", "config")})
	}
	return paths
}

// Read returns the file contents, or an error when the path does not exist.
func (FilesystemSource) Read(path string) ([]byte, error) {
	return os.ReadFile(path)
}
