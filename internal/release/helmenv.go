package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// helmEnv is a private Helm home for one pull: the operator's Helm state must
// not affect what box installs, and the registry client reads a credentials file
// at construction. The 0700 temp dir is removed after the pull, and the
// authorizer carries no credential, so no store is read or sent.
type helmEnv struct {
	dir string
	// registryConfig is an empty private path; box never logs in.
	registryConfig string
}

func newHelmEnv() (*helmEnv, error) {
	dir, err := os.MkdirTemp("", "box-helm-")
	if err != nil {
		return nil, fmt.Errorf("private helm directory: %w", err)
	}
	return &helmEnv{dir: dir, registryConfig: filepath.Join(dir, "registry", "config.json")}, nil
}

// Close removes the home; a failed removal is not a failed pull.
func (e *helmEnv) Close() error { return os.RemoveAll(e.dir) }
