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

// ApplySnapshots is `box apply`'s snapshot step: release.Snapshotter over the
// release Helm says is deployed, the StatefulSets and pods of it, and the copy
// ConfigSnapshots takes under the grant's config root.
//
// It is the hand-run command's half of what a step does by itself. A values pin
// -- a tag, a digest, a variant -- moves an app's image over the same
// forward-only migration a release bump does, and an apply has no health gate
// and no automatic rollback (docs/box.md, "Snapshot and rollback"): the copy is
// what the operator's `box rollback` restores afterwards.
//
// The decision is made over interfaces for the same reason the rest of the
// package is: a test drives what a fake cluster runs, what a fake copy was
// asked for, and the order through one shared log.
type ApplySnapshots struct {
	// OpenRelease reads the deployed release, which names the copy's version:
	// an apply moves a values pin, not the release it is on.
	OpenRelease func(namespace string) (Release, error)
	// OpenCluster is the release's StatefulSets and pods: the image they run
	// and the scale-down a copy needs.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots is the copy itself: the app config directories under the
	// config root this apply names, and the files the chart's catalog says an
	// image upgrade can rewrite.
	OpenSnapshots func(root string, values map[string]any, chart release.Chart) (Snapshotter, error)
	// SettleWait is how long a stopped app's pods get to go, and CleanupWait
	// how long the restart after a failure gets. Poll is how often either is
	// asked. Zero means the update step's own defaults.
	SettleWait  time.Duration
	CleanupWait time.Duration
	Poll        time.Duration
	// Now is time.Now, so a test can pin the copy's directory name.
	Now func() time.Time
}

// Pending reads the release once and decides what this apply moves: every
// enabled app whose image in the cluster differs from the one the final values
// render, and whether the release's catalog gives it files to copy.
//
// The comparison is over the reference each instance's StatefulSet asks for,
// read from the cluster and never from the values file or Helm's stored values:
// state follows the object. The digest a pod reports is what ran, not what the
// app asks for, and is read only to name the bytes a copy records -- see
// decideMove. An app with no StatefulSet here (the values just enabled it) has
// nothing to migrate and does not move.
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
		// apply asks about a release Helm says is deployed, so this is a read
		// that disagrees with the one made moments ago: nothing runs, and
		// nothing can move.
		return work, nil
	}
	// The copy is named like a step's. An apply that only changes values
	// installs the release it is on, so both sides are the installed version.
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
		// The holds are already in the values apply hands over, so the
		// resolution here is the chart's own: variant, then pin, which is
		// what the release will render.
		movedTo, ok := renderedImage(plan.Chart, plan.Values, nil, app)
		if !ok {
			// The chart renders no image for the app, so Helm will refuse the
			// upgrade naming the variant. box cannot compare what it cannot
			// render, and stopping the app for a release that will not install
			// is worse than saying nothing about it.
			continue
		}
		if move, moves := decideMove(app, run, movedTo, len(globs[app]) > 0); moves {
			work.Moves = append(work.Moves, move)
		}
	}
	return work, nil
}

// Take stops the apps Pending named and copies their config, with the same
// order a step uses: stop, wait for the pods to go, then copy. Any failure
// before the release moves starts the apps this run stopped again and returns
// the error, because the release was never tried.
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
	// Whether the copy can be taken is asked before anything stops, so a box
	// that cannot copy refuses with the apps still running and nothing
	// recorded: the box refused, not the release.
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

// Prune keeps the newest two copies per app once the release is up on the
// images it moved to.
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

// images is what the copy's manifest records for every moving app: the image it
// ran and the one this apply moves it to. Only a copied app's entry is used.
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

// decideMove is one app's move: whether this apply changes its image at all
// and, when it does and the catalog names state to copy, the bytes it runs as
// far as the cluster can name them.
//
// Whether an app moves is decided on the reference its StatefulSets ask for,
// never on the digest its pods report. A tag-only pin the values did not touch
// renders the same tag the app already runs, and two digests under that one tag
// are the same reference however they were pulled; comparing a pod digest there
// would make an unchanged pin read as a move, and stop the app on every apply
// that followed. The pod digest is read only once an app has moved and is going
// to be copied, because the copy has to name the bytes that wrote the state.
//
// When those bytes cannot be named -- no pod reports one, its instances report
// different ones, or its instances run different references -- the app still
// moves, uncopied: box has nothing it can put back, and a refusal would lock
// the box out of the apply that pins the digest. The plan says why before the
// question, and the operator's no is the safeguard.
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
	// No single reference is what the app runs, so nothing a copy records
	// could say which image the state belongs to. A hand `kubectl set image`
	// on one instance is how this happens.
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

// ranDigest is the one digest an app's pods report for a reference that names
// only a tag, or the reason they name none.
//
// One instance with nothing to say is enough: box can only record the bytes
// every instance ran, and a pod that is scaled to zero or has not started says
// nothing. Two digests are the same answer -- the tag covered more than one
// image while the app ran -- and box cannot pick one. The caller keeps the app
// uncopied in both cases rather than record a reference a rollback would drift
// from.
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

// sortedInstances is an app's StatefulSet names in one order, so the plan reads
// the same on every run whatever order the cluster listed them in.
func sortedInstances(images map[string]snapshot.ImageRef) []string {
	instances := make([]string, 0, len(images))
	for instance := range images {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	return instances
}

// imagePairs names each instance's reference, in instance order, for a plan row
// that cannot say what the app runs as one reference: the operator needs to see
// which instance is on which image.
func imagePairs(images map[string]snapshot.ImageRef) string {
	instances := sortedInstances(images)
	pairs := make([]string, 0, len(instances))
	for _, instance := range instances {
		pairs = append(pairs, instance+" "+images[instance].String())
	}
	return strings.Join(pairs, ", ")
}
