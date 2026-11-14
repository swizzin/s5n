package main

import (
	"context"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/update"
)

// applySnapshots is `box apply`'s snapshot step: a values pin that moves an
// app's image moves it over the same forward-only migration a release bump
// does, and the copy is what `box rollback` restores afterwards.
//
// Only apply sets it. An update snapshots each step itself, and a rollback's
// apply is the move back, so neither may inherit the step.
func applySnapshots(kubeconfig string) *update.ApplySnapshots {
	return &update.ApplySnapshots{
		OpenRelease: func(namespace string) (update.Release, error) {
			return release.NewHelm(kubeconfig, namespace, release.ReleaseName)
		},
		OpenCluster: func(namespace string) (update.Cluster, error) {
			return update.NewWorkloads(kubeconfig, namespace, release.ReleaseName)
		},
		OpenSnapshots: func(root string, values map[string]any, chart release.Chart) (update.Snapshotter, error) {
			// The catalog is the release this apply installs: what an image
			// upgrade can rewrite is the chart's answer, not this box's.
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
