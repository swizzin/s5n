package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// helmEnv is a private Helm home for one chart pull. box runs as root, and the
// operator's Helm state -- config, cache, HELM_* and DOCKER_CONFIG -- must not
// affect what box installs: Helm's registry client reads a credentials file at
// construction and defaults it to the operator's registry/config.json. The
// path here is inside a 0700 temp dir removed after the pull, and the client's
// authorizer carries no credential (OCIChart.client), so no credentials store
// is read or sent. A private registry is out of scope (docs/box.md).
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
