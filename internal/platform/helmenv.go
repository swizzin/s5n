package platform

import (
	"fmt"
	"os"
	"path/filepath"

	"helm.sh/helm/v4/pkg/cli"
)

// helmEnv is a private Helm home for one chart fetch: every path Helm would take from
// the invoking user's environment -- repository file, repo and content caches, registry
// credentials, plugins -- points inside a temporary directory box creates (0700) and
// removes when the fetch ends.
//
// box runs as root, and root's Helm state is the operator's: a stale repository entry
// makes every chart URL fail with "no cached repo found", a cached chart is preferred
// over the pinned one, and credentials or getter plugins there would decide what box
// downloads and run code in its process. box pulls public artifacts only, so an empty
// private home is the whole state (docs/box.md).
//
// The settings are a literal rather than cli.New(), which reads the environment as it
// constructs. Only the path fields are consulted by a fetch; box passes its kubeconfig
// to Helm's action configuration separately.
type helmEnv struct {
	dir      string
	settings *cli.EnvSettings
}

// newHelmEnv creates the private home; the caller removes it with Close, one fetch
// being one operation.
func newHelmEnv() (*helmEnv, error) {
	dir, err := os.MkdirTemp("", "box-helm-")
	if err != nil {
		return nil, fmt.Errorf("private helm directory: %w", err)
	}
	return &helmEnv{
		dir: dir,
		settings: &cli.EnvSettings{
			RepositoryConfig: filepath.Join(dir, "repositories.yaml"),
			RepositoryCache:  filepath.Join(dir, "repository"),
			ContentCache:     filepath.Join(dir, "content"),
			RegistryConfig:   filepath.Join(dir, "registry", "config.json"),
			PluginsDirectory: filepath.Join(dir, "plugins"),
		},
	}, nil
}

// Close removes the home. A failed removal is not a failed install: reporting a leaked
// temp directory instead of the chart installed would be the wrong answer.
func (e *helmEnv) Close() error { return os.RemoveAll(e.dir) }
