package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/swizzin/s5n/internal/bootstrap"
)

// ResolveKubeconfig picks the cluster `box apply` writes to, with bootstrap's
// flag semantics: --kubeconfig wins, the first KUBECONFIG entry is next, and the
// recorded cluster is the fallback only when neither was given. It never
// re-derives a cluster: bootstrap decided, and box reuses the decision.
//
// An explicit path that does not exist is an error, not a reason to fall back.
// Silently applying to a different cluster than the one named is the worst
// failure this command could have.
func ResolveKubeconfig(flag, env string, records bootstrap.RecordStore) (string, error) {
	path := flag
	if path == "" {
		for _, p := range filepath.SplitList(env) {
			if p != "" {
				path = p
				break
			}
		}
	}
	if path != "" {
		if err := readable(path); err != nil {
			return "", fmt.Errorf("explicit kubeconfig: %w", err)
		}
		return path, nil
	}

	rec, err := records.Load()
	if err != nil {
		return "", fmt.Errorf("read cluster record: %w", err)
	}
	if rec == nil {
		return "", errors.New("no cluster recorded: run `box bootstrap` first, or pass --kubeconfig")
	}
	if rec.Kubeconfig == "" {
		return "", errors.New("the cluster record names no kubeconfig: run `box bootstrap` again")
	}
	if err := readable(rec.Kubeconfig); err != nil {
		return "", fmt.Errorf("recorded kubeconfig (run `box bootstrap`): %w", err)
	}
	return rec.Kubeconfig, nil
}

func readable(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
