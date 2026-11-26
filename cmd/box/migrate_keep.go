package main

// migrate_keep.go is the keep path's pre-flight: before installing onto a
// cluster somebody else runs, ask whether bootstrap accepts it, what answers on
// ports 80/443, and whether an auto-updater watches box's namespaces. Any answer
// stops the run; box changes nothing it did not install. The cluster question is
// bootstrap's own gate chain, so both refuse identically.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/release"
)

// keepBlockers is in reading order: the cluster first, then its objects.
func keepBlockers(ctx context.Context, o cutoverOptions, read migrationRead) ([]discover.Blocker, error) {
	var blockers []discover.Blocker
	// The proof refused a saved read, so this is a live cluster.
	cluster, ok := read.src.(*discover.Cluster)
	if !ok {
		return nil, errors.New("the keep path checks the source's own cluster, and this read is not of one")
	}
	kc, err := release.ResolveKubeconfig(o.Kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(kc)
	if err != nil {
		return nil, fmt.Errorf("read the source cluster's kubeconfig %s: %w", kc, err)
	}
	if refusal := bootstrap.ExistingClusterRefusal(ctx, kc, raw); refusal != "" {
		blockers = append(blockers, discover.ClusterBlocker(refusal))
	}
	onCluster, err := cluster.Blockers(ctx)
	if err != nil {
		return nil, err
	}
	return append(blockers, onCluster...), nil
}
