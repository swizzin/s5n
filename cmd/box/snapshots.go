package main

import (
	"context"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/update"
)

// applySnapshots is apply's snapshot step: a pin moves an image over the same
// migration a bump does. Only apply sets it; update and rollback snapshot their
// own way.
func applySnapshots(kubeconfig string) *update.ApplySnapshots {
	return &update.ApplySnapshots{
		OpenRelease: func(namespace string) (update.Release, error) {
			return release.NewHelm(kubeconfig, namespace, release.ReleaseName)
		},
		OpenCluster: func(namespace string) (update.Cluster, error) {
			return update.NewWorkloads(kubeconfig, namespace, release.ReleaseName)
		},
		OpenSnapshots: func(root string, values map[string]any, chart release.Chart) (update.Snapshotter, error) {
			// The installed release's catalog, not this box's.
			globs, err := update.ChartGlobs(chart)
			if err != nil {
				return nil, err
			}
			return &update.ConfigSnapshots{
				ConfigRoot: root,
				Values:     values,
				Globs: func(context.Context, string) (map[string][]string, error) {
					return globs, nil
				},
				Now: time.Now,
			}, nil
		},
	}
}
