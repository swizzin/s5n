package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// helmEnv is a private Helm home for one chart pull.
//
// box runs as root (docs/box.md): `box bootstrap` is the host CLI, and `box
// apply` is normally run by root too. Root's Helm state -- ~/.config/helm,
// ~/.cache/helm, HELM_REGISTRY_CONFIG, HELM_REPOSITORY_CONFIG, DOCKER_CONFIG
// -- is the operator's, not box's, and a chart box installs must not depend on
// it. Helm's registry client reads a credentials file when it is constructed
// (registry.NewClient -> oras credentials.NewStore), and defaults that path to
// helmpath.ConfigPath, i.e. the operator's registry/config.json: a file Helm
// itself would only complain about on the operator's next `helm` run stops
// box's pull at construction.
//
// box pulls public artifacts only, so it needs no credentials at all. The path
// below is inside a temporary directory box creates (mode 0700) and removes when
// the pull ends, on success and on failure; nothing exists at it, and the
// registry client is also given an authorizer with no credential (see
// OCIChart.client), so neither Helm's credentials file nor docker's is ever
// read for a secret or sent to a registry. A private registry is out of scope
// (docs/box.md), and box says so when one asks for credentials.
type helmEnv struct {
	dir string
	// registryConfig is Helm's registry credentials path: an empty, private
	// file Helm would only write if box logged in, which it never does.
	registryConfig string
}

func newHelmEnv() (*helmEnv, error) {
	dir, err := os.MkdirTemp("", "box-helm-")
	if err != nil {
		return nil, fmt.Errorf("private helm directory: %w", err)
	}
	return &helmEnv{dir: dir, registryConfig: filepath.Join(dir, "registry", "config.json")}, nil
}

// Close removes the home. A failed removal is not a failed pull: the directory
// holds nothing box needs after the chart is in memory, and a leaked temp
// directory is not worth failing an install over.
func (e *helmEnv) Close() error { return os.RemoveAll(e.dir) }
