package update

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ApplySnapshots is `box apply`'s snapshot step over the deployed release, its
// StatefulSets and pods, and the copy under the config root. An apply has no
// health gate or automatic rollback (docs/box.md, "Snapshot and rollback"): the
// copy is what the operator's `box rollback` restores.
type ApplySnapshots struct {
	// OpenRelease reads the deployed release, which names the copy's version.
	OpenRelease func(namespace string) (Release, error)
	// OpenCluster is the release's StatefulSets and pods.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots is the copy: config directories and the catalog's globs.
	OpenSnapshots func(root string, values map[string]any, chart release.Chart) (Snapshotter, error)
	// SettleWait and CleanupWait bound the stop and the restart after a
	// failure; Poll is how often either is asked. Zero means the step's
	// defaults.
	SettleWait  time.Duration
	CleanupWait time.Duration
	Poll        time.Duration
	// Now is time.Now, so a test can pin the copy's directory name.
	Now func() time.Time
}

// Pending reads the release once and decides what this apply moves: enabled
// apps whose cluster image differs from the rendered one. The comparison is the
// reference each StatefulSet asks for, read from the cluster and never from
// values, because state follows the object. A pod's digest is read only to name
// the bytes a copy records. An app with no StatefulSet has nothing to migrate.
func (s *ApplySnapshots) Pending(ctx context.Context, plan release.SnapshotPlan) (release.SnapshotWork, error) {
	work := release.SnapshotWork{}
	done := func(err error) (release.SnapshotWork, error) { return release.SnapshotWork{}, err }
	rel, err := s.OpenRelease(plan.Namespace)
	if err != nil {
		return done(err)
	}
	state, err := rel.State(ctx)
	if err != nil {
		return done(err)
	}
	if !state.Exists {
		// Helm said moments ago the release exists; nothing can move.
		return work, nil
	}
	// Both sides are the installed version: apply changes values, not the chart.
	work.From, work.To = state.ChartVersion, state.ChartVersion

	cluster, err := s.OpenCluster(plan.Namespace)
	if err != nil {
		return done(err)
	}
	running, err := cluster.Running(ctx)
	if err != nil {
		return done(err)
	}
	globs, err := ChartGlobs(plan.Chart)
	if err != nil {
		return done(err)
	}
	for _, app := range release.EnabledApps(plan.Values) {
		run, ok := running[app]
		if !ok {
			continue
		}
		// The holds are already in these values, so this is what Helm renders.
		movedTo, ok := renderedImage(plan.Chart, plan.Values, nil, app)
		if !ok {
			// Helm will refuse the upgrade anyway; stopping the app for a
			// release that will not install is worse than saying nothing.
			continue
		}
		if move, moves := decideMove(app, run, movedTo, len(globs[app]) > 0); moves {
			work.Moves = append(work.Moves, move)
		}
	}
	return work, nil
}

// Take stops the apps Pending named, waits for their pods, then copies. Any
// failure before the release moves starts them again: the release was never
// tried.
func (s *ApplySnapshots) Take(ctx context.Context, plan release.SnapshotPlan, work release.SnapshotWork) (release.SnapshotDirs, error) {
	copied := work.Copied()
	if len(copied) == 0 {
		return nil, nil
	}
	cluster, err := s.OpenCluster(plan.Namespace)
	if err != nil {
		return nil, err
	}
	snaps, err := s.OpenSnapshots(plan.ConfigRoot, plan.Values, plan.Chart)
	if err != nil {
		return nil, err
	}
	// Ask whether the copy can be taken before stopping anything: a box that
	// cannot copy refuses with the apps still running.
	if err := snaps.Check(ctx, s.step(work), copied); err != nil {
		return nil, fmt.Errorf("apply: %s cannot be snapshotted: %w", strings.Join(copied, ", "), err)
	}
	if err := scaleApps(ctx, cluster, copied, 0); err != nil {
		s.startAgain(ctx, cluster, copied)
		return nil, fmt.Errorf("stopping %s: %w", strings.Join(copied, ", "), err)
	}
	if err := waitGone(ctx, cluster, copied, s.settleWait(), s.poll()); err != nil {
		s.startAgain(ctx, cluster, copied)
		return nil, fmt.Errorf("waiting for %s to stop: %w", strings.Join(copied, ", "), err)
	}
	taken, err := snaps.Take(ctx, s.step(work), copied, s.images(work))
	if err != nil {
		s.startAgain(ctx, cluster, copied)
		return nil, fmt.Errorf("snapshotting %s: %w", strings.Join(copied, ", "), err)
	}
	return release.SnapshotDirs(taken.Dirs), nil
}

// Prune keeps the newest two copies per app once the release is up.
func (s *ApplySnapshots) Prune(ctx context.Context, plan release.SnapshotPlan, work release.SnapshotWork) error {
	copied := work.Copied()
	if len(copied) == 0 {
		return nil
	}
	snaps, err := s.OpenSnapshots(plan.ConfigRoot, plan.Values, plan.Chart)
	if err != nil {
		return err
	}
	return snaps.Prune(ctx, s.step(work), copied)
}

// step is the copy's name, as a step's would be.
func (s *ApplySnapshots) step(work release.SnapshotWork) Step {
	return Step{From: work.From, To: work.To}
}

// images is what the copy records for every moving app.
func (s *ApplySnapshots) images(work release.SnapshotWork) map[string]ImageChange {
	out := make(map[string]ImageChange, len(work.Moves))
	for _, move := range work.Moves {
		out[move.App] = ImageChange{Ran: move.Ran, MovedTo: move.MovedTo, Snapshot: move.Copied}
	}
	return out
}

func (s *ApplySnapshots) startAgain(ctx context.Context, cluster Cluster, apps []string) {
	startApps(ctx, cluster, apps, s.cleanupWait())
}

func (s *ApplySnapshots) settleWait() time.Duration { return orDuration(s.SettleWait, 5*time.Minute) }

func (s *ApplySnapshots) cleanupWait() time.Duration { return orDuration(s.CleanupWait, 5*time.Minute) }

func (s *ApplySnapshots) poll() time.Duration { return orDuration(s.Poll, 2*time.Second) }

// decideMove says whether one app's image changes and, when the catalog names
// state to copy, the bytes it ran. Whether it moves is the reference its
// StatefulSets ask for, never a pod digest: an untouched tag-only pin renders
// the tag already running, and comparing digests would make every later apply
// stop the app again. The digest is read only once a move is being copied.
//
// When those bytes cannot be named -- no pod reports one, instances disagree --
// the app moves uncopied: a refusal would lock the box out of the apply that
// pins the digest, so the plan says why and the operator's no is the safeguard.
func decideMove(app string, run Running, movedTo snapshot.ImageRef, copyable bool) (release.SnapshotMove, bool) {
	instances := sortedInstances(run.Images)
	if len(instances) == 0 {
		return release.SnapshotMove{}, false
	}
	moved := false
	for _, instance := range instances {
		if !sameImage(run.Images[instance], movedTo) {
			moved = true
			break
		}
	}
	if !moved {
		return release.SnapshotMove{}, false
	}
	move := release.SnapshotMove{App: app, Ran: run.Images[instances[0]], MovedTo: movedTo, Copied: copyable}
	if !copyable {
		return move, true
	}
	// No single reference is what the app runs, so a copy cannot name the image
	// its state belongs to. A hand `kubectl set image` on one instance does it.
	for _, instance := range instances[1:] {
		if !sameImage(move.Ran, run.Images[instance]) {
			move.Copied = false
			move.Uncopied = release.UncopiedDifferentImages
			move.Detail = imagePairs(run.Images)
			return move, true
		}
	}
	if move.Ran.Digest != "" {
		return move, true
	}
	digest, reason := ranDigest(run, instances)
	if reason != release.UncopiedNone {
		move.Copied = false
		move.Uncopied = reason
		move.Detail = move.Ran.Tag
		return move, true
	}
	move.Ran.Digest = digest
	return move, true
}

// ranDigest is the one digest an app's pods report for a tag-only reference, or
// why none: a silent instance (scaled to zero, not started) or two digests (the
// tag covered more than one image) leaves the app uncopied rather than recording
// a reference a rollback would drift from.
func ranDigest(run Running, instances []string) (string, release.UncopiedReason) {
	seen := make(map[string]bool, len(instances))
	digest := ""
	for _, instance := range instances {
		digests := run.Digests[instance]
		if len(digests) == 0 {
			return "", release.UncopiedNoDigest
		}
		for _, d := range digests {
			if seen[d] {
				continue
			}
			seen[d] = true
			digest = d
		}
	}
	if len(seen) > 1 {
		return "", release.UncopiedDifferentBytes
	}
	return digest, release.UncopiedNone
}

// sortedInstances orders names so the plan reads the same on every run.
func sortedInstances(images map[string]snapshot.ImageRef) []string {
	instances := make([]string, 0, len(images))
	for instance := range images {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	return instances
}

// imagePairs names each instance's reference for a plan row that cannot name
// one; the operator needs to see which instance is on which image.
func imagePairs(images map[string]snapshot.ImageRef) string {
	instances := sortedInstances(images)
	pairs := make([]string, 0, len(instances))
	for _, instance := range instances {
		pairs = append(pairs, instance+" "+images[instance].String())
	}
	return strings.Join(pairs, ", ")
}
