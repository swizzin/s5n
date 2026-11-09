package platform

import (
	"fmt"
	"os"
	"path/filepath"

	"helm.sh/helm/v4/pkg/cli"
)

// helmEnv is a private Helm home for one chart fetch. Every on-disk path Helm
// would otherwise take from the invoking user's environment -- the repository
// file, the repository and content caches, the registry credentials, the
// plugins directory -- points inside a temporary directory box creates
// (mode 0700) and removes when the fetch ends, on success and on failure.
//
// Why: box runs as root, and root's Helm state is the operator's, not box's.
// helm.sh/helm/v4/pkg/cli.New() is what the helm command builds its
// environment from, and it reads HELM_* and root's ~/.config/helm and
// ~/.cache/helm. Any of that can change or break an install box makes:
//
//   - a repository entry that was added but never updated leaves no cached
//     index, and Helm's scanReposForURL then fails *every* chart URL download
//     with "no cached repo found. (try 'helm repo update')" -- a stale entry in
//     root's repositories.yaml stops a bootstrap that never used that
//     repository;
//   - a chart already in root's content cache is preferred over the one the
//     pinned version names;
//   - registry credentials and getter plugins under root's home would decide
//     what box downloads and run code in box's process.
//
// box pulls public artifacts only, so its Helm needs no repository of its own
// and no credentials at all: an empty private home is the whole state. A
// private registry is out of scope (docs/box.md); box says so when a registry
// asks for credentials.
//
// The settings are built as a literal rather than by cli.New() because cli.New
// reads the environment as it constructs. Only the path fields below are
// consulted by a chart fetch (getter.All reads PluginsDirectory; the
// ChartDownloader takes the three repository paths); box passes its own
// kubeconfig to Helm's action configuration separately, so the empty
// KubeConfig/context fields here are never used.
type helmEnv struct {
	dir      string
	settings *cli.EnvSettings
}

// newHelmEnv creates the private home. The caller removes it with Close; a
// chart fetch is one operation, so it is defers, not a field on Helm.
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

// Close removes the home. A failed removal is not a failed install: the
// directory holds nothing box needs after the fetch, and reporting a leaked
// temp directory instead of the chart box installed would be the wrong answer.
func (e *helmEnv) Close() error { return os.RemoveAll(e.dir) }
