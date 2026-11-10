package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Records is the one thing the resolver needs from `box bootstrap`'s cluster
// record: the kubeconfig the decision wrote. bootstrap's store satisfies it, so
// `release` no longer imports bootstrap for a string, and the next command that
// resolves a cluster (root's `box adduser`) does not have to either.
type Records interface {
	// RecordedKubeconfig returns the recorded kubeconfig path and whether a
	// record exists at all.
	RecordedKubeconfig() (path string, recorded bool, err error)
}

// ResolveKubeconfig picks the cluster a command writes to: --kubeconfig wins,
// the first KUBECONFIG entry is next, and the recorded cluster is the fallback
// only when neither was given. It never re-derives a cluster: bootstrap decided,
// and box reuses the decision.
//
// An explicit path that does not exist is an error, not a reason to fall back.
// Silently writing to a different cluster than the one named is the worst
// failure these commands have.
func ResolveKubeconfig(flag, env string, records Records) (string, error) {
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

	recorded, found, err := records.RecordedKubeconfig()
	if err != nil {
		return "", fmt.Errorf("read cluster record: %w", err)
	}
	if !found {
		return "", errors.New("no cluster recorded: run `box bootstrap` first, or pass --kubeconfig")
	}
	if recorded == "" {
		return "", errors.New("the cluster record names no kubeconfig: run `box bootstrap` again")
	}
	if err := readable(recorded); err != nil {
		return "", fmt.Errorf("recorded kubeconfig (run `box bootstrap`): %w", err)
	}
	return recorded, nil
}

func readable(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
