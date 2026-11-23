package main

// migrate_keep.go is the keep path's pre-flight: the check between the final
// snapshot and the install, for a cutover that keeps the source's cluster.
//
// Keeping is the default because it destroys nothing, and the one thing it must not
// do is half-take a cluster somebody else runs. So before box installs onto it, four
// questions are asked -- whether the cluster is one bootstrap accepts at all, what
// already answers on the node's ports 80 and 443, and whether an auto-updater
// watches the namespaces box is about to live in -- and any answer stops the run
// with the command to look at it. box never changes a cluster it did not install, so
// nothing here is fixed by box: the operator clears it, or takes the other road.
//
// The cluster question is bootstrap's own gate chain (internal/bootstrap), not a
// second copy of it: a cluster `box bootstrap` would refuse is one migrate must
// refuse identically.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/release"
)

// keepBlockers is the pre-flight, in the order the operator should read it: the
// cluster itself first, because a cluster bootstrap will not drive makes the other
// answers moot, then the objects on it.
func keepBlockers(ctx context.Context, o cutoverOptions, read migrationRead) ([]discover.Blocker, error) {
	var blockers []discover.Blocker
	// The proof already refused a saved read, so the source is a live cluster here.
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
