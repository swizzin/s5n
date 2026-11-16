package update

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/snapshot"
)

// Step is one release-to-release move: the chart version the box is on and the
// one this step installs. The pair names a snapshot, and the step records it as
// the revision a rollback command returns past.
type Step struct {
	From string
	To   string
}

// planSteps is the update path: the releases box has to install, in order, to
// get from installed to target without skipping a migration.
//
// A release carries box.s5n/upgrade-from in its Chart.yaml: the oldest release
// it can be installed over, because an older one's data was never migrated to
// what this release expects. So the target is one step when its floor is at or
// below the installed release. Otherwise the path steps to the newest release
// between them whose floor the current release meets and whose min-box this box
// meets, and repeats from there. Every release on the path is a published one:
// a version box invented would be a migration nobody ran.
//
// It never steps backwards, prereleases are not on the path (they are not
// releases box installs unasked), a release with no floor declares none, and an
// annotation that is not a version is an error naming the release -- guessing
// there would silently skip the release that carries the migration.
func planSteps(installed, target, boxVersion string, releases []release.Published) ([]Step, error) {
	cur, err := semver.NewVersion(installed)
	if err != nil {
		return nil, fmt.Errorf("installed release %q is not a version: %v", installed, err)
	}
	want, err := semver.NewVersion(target)
	if err != nil {
		return nil, fmt.Errorf("target release %q is not a version: %v", target, err)
	}
	if !want.GreaterThan(cur) {
		return nil, nil
	}

	byVersion := make(map[string]release.Published, len(releases))
	for _, r := range releases {
		v, err := semver.NewVersion(r.Version)
		if err != nil {
			continue
		}
		// Prereleases stay in the index: a prerelease is not a step box
		// takes unasked, but one the operator named with --chart-version is
		// the target, and the target has to be found here.
		byVersion[v.String()] = r
	}

	var steps []Step
	for cur.LessThan(want) {
		from := cur
		next, version, err := nextStep(from, want, boxVersion, releases, byVersion)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, fmt.Errorf("cannot update from %s to %s: no published release can be upgraded from %s",
				installed, target, from.Original())
		}
		if !version.GreaterThan(from) {
			// Defensive: a release list that names a version below the one
			// being stepped from must not loop or move a box backwards.
			return nil, fmt.Errorf("cannot update from %s to %s: release %s does not move the box forward",
				installed, target, next.Version)
		}
		steps = append(steps, Step{From: from.Original(), To: next.Version})
		cur = version
	}
	return steps, nil
}

// nextStep is one hop: the target itself when its floor allows the current
// release, otherwise the newest release between them that does and that this
// box meets min-box for.
func nextStep(from, target *semver.Version, boxVersion string, releases []release.Published, byVersion map[string]release.Published) (*release.Published, *semver.Version, error) {
	targetRelease, ok := byVersion[target.String()]
	if !ok {
		return nil, nil, fmt.Errorf("release %s is not in the published list", target.Original())
	}
	floor, err := upgradeFrom(targetRelease)
	if err != nil {
		return nil, nil, err
	}
	if floor == nil || !from.LessThan(floor) {
		return &targetRelease, target, nil
	}

	var best *release.Published
	var bestVersion *semver.Version
	for i := range releases {
		candidate := releases[i]
		v, err := semver.NewVersion(candidate.Version)
		if err != nil || v.Prerelease() != "" {
			continue
		}
		if !v.GreaterThan(from) || !v.LessThan(target) {
			continue
		}
		candidateFloor, err := upgradeFrom(candidate)
		if err != nil {
			return nil, nil, err
		}
		if candidateFloor != nil && from.LessThan(candidateFloor) {
			continue
		}
		meets, err := meetsMinBox(candidate, boxVersion)
		if err != nil {
			return nil, nil, err
		}
		if !meets {
			continue
		}
		if bestVersion == nil || v.GreaterThan(bestVersion) {
			best, bestVersion = &releases[i], v
		}
	}
	return best, bestVersion, nil
}

// upgradeFrom is one release's floor, or nil when it declares none (a chart
// from before the annotation).
func upgradeFrom(r release.Published) (*semver.Version, error) {
	if r.UpgradeFrom == "" {
		return nil, nil
	}
	v, err := semver.NewVersion(r.UpgradeFrom)
	if err != nil {
		return nil, fmt.Errorf("release %s: annotation %s %q is not a version: %v",
			r.Version, release.UpgradeFromAnnotation, r.UpgradeFrom, err)
	}
	return v, nil
}

// meetsMinBox is SelectVersion's rule for one release, applied to a candidate
// step: a release this box is too old for is not a step on anyone's path.
func meetsMinBox(r release.Published, boxVersion string) (bool, error) {
	if r.MinBox == "" || release.IsDevVersion(boxVersion) {
		return true, nil
	}
	floor, err := semver.NewVersion(r.MinBox)
	if err != nil {
		return false, fmt.Errorf("release %s: annotation %s %q is not a version: %v",
			r.Version, release.MinBoxAnnotation, r.MinBox, err)
	}
	own, err := semver.NewVersion(boxVersion)
	if err != nil {
		return false, fmt.Errorf("box version %q is not a version: %v", boxVersion, err)
	}
	return !own.LessThan(floor), nil
}

// ImageChange is one moving app's image on each side of a step: the one it ran
// in the release the step starts from, and the one the step moves it to. They
// travel with the plan because a snapshot records both -- the first is what a
// rollback holds the app at, the second is what the hold avoids -- and nothing
// after the release moves can say what the app used to run.
type ImageChange struct {
	Ran     snapshot.ImageRef
	MovedTo snapshot.ImageRef
	// Snapshot says the app's catalog entry names files an image upgrade can
	// rewrite, so a step stops it and copies them before the release moves.
	// An app without them is moved without a copy, is not stopped, and is
	// never rolled back.
	Snapshot bool
}

// movingApps is the enabled apps whose image moves between two chart releases,
// sorted, the apps the operator's values hold in place, and what each moving
// app's image is on either side of the step. A database migrates when its image
// changes, so Moving is exactly the set a step has to snapshot before it
// installs the new release.
//
// The image is the one values select: apps.<app>.image.variant, "default" when
// nothing says otherwise. A variant that exists in both releases is compared by
// repository and digest, and by tag when neither release pins a digest: the
// chart pins digest, so a tag that was re-pushed to new bytes is an image change
// the digest sees, and a release that pins no digest at all (a locally built
// image) changes the image by changing the tag. An app in only
// one of the two releases is not moving (there is no install to migrate), and a
// chart that ships no variants.yaml tells box nothing about images, so it moves
// nothing.
//
// A pin in the values -- apps.<app>.image.digest, or a tag without a digest --
// holds the app at one image whatever the chart published: the chart renders
// the pinned digest on both sides, or no digest at all when the pin is a tag,
// so the same bytes run before and after the step. Such an app is Held when it
// would have moved without the pin, and it is neither stopped nor copied,
// because nothing of it changes. A pin cannot hold an app whose repository
// changes between the releases: that is a different image, pinned or not.
func movingApps(from, to release.Chart, values map[string]any, holds []snapshot.Hold) (moving, held, heldBack []string, images map[string]ImageChange, err error) {
	if len(from.Variants) == 0 || len(to.Variants) == 0 {
		return nil, nil, nil, nil, nil
	}
	oldImages, err := admission.Images(from.Variants)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("release %s: %w", from.Version, err)
	}
	newImages, err := admission.Images(to.Variants)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("release %s: %w", to.Version, err)
	}
	images = make(map[string]ImageChange)
	for _, app := range release.EnabledApps(values) {
		kind, change := appDecision(app, values, oldImages, newImages, holds)
		switch kind {
		case moveMoves:
			moving = append(moving, app)
			images[app] = change
		case moveHeld:
			held = append(held, app)
		case moveHeldBack:
			heldBack = append(heldBack, app)
		}
	}
	sort.Strings(moving)
	sort.Strings(held)
	sort.Strings(heldBack)
	return moving, held, heldBack, images, nil
}

// appMove is what one step does with one app.
type appMove int

const (
	// moveNone: the app is not affected by this step.
	moveNone appMove = iota
	// moveMoves: its image changes and the release will roll it out.
	moveMoves
	// moveHeld: the operator's values pin holds it where it is.
	moveHeld
	// moveHeldBack: a rollback hold keeps it on the image it ran.
	moveHeldBack
)

// appDecision decides one app's fate in a step: the variant the values select, the
// images both releases ship for it, and the operator's pins and the box's holds.
func appDecision(app string, values map[string]any, oldImages, newImages map[string]map[string]admission.Image, holds []snapshot.Hold) (appMove, ImageChange) {
	variant := variantOf(values, app)
	oldImage, oldOK := oldImages[app][variant]
	newImage, newOK := newImages[app][variant]
	oldApp, newApp := hasApp(oldImages, app), hasApp(newImages, app)
	switch {
	case oldOK && newOK:
		return shippedMove(app, values, oldImage, newImage, holds)
	case !oldApp && !newApp:
		// Neither release ships this app: the values name something the
		// chart would ignore, so there is no install to migrate.
		return moveNone, ImageChange{}
	case oldApp && newApp:
		// The app exists on both sides but the variant the values select
		// does not: box cannot tell whether the image moved, so it takes the
		// safe answer and snapshots. A pin does not change that -- box cannot
		// say what the pin would hold. The side that ships no such variant
		// leaves its image zero, and the copy refuses rather than record an
		// image box never saw.
		return moveMoves, ImageChange{Ran: imageRef(oldImage), MovedTo: imageRef(newImage)}
	default:
		// The app is in one release only. There is nothing to migrate.
		return moveNone, ImageChange{}
	}
}

// shippedMove decides an app both releases ship the selected variant of: the
// digest is the image, so a tag re-pushed to new bytes still reads as a move. The
// operator's pin is applied first, and a hold only decides when no pin does.
func shippedMove(app string, values map[string]any, oldImage, newImage admission.Image, holds []snapshot.Hold) (appMove, ImageChange) {
	// What the chart did is remembered first, because the pin may hide it.
	chartMoved := imageMoved(oldImage, newImage)
	tag, digest := pinOf(values, app)
	pinned := tag != "" || digest != ""
	switch {
	case digest != "":
		oldImage.Digest, newImage.Digest = digest, digest
	case tag != "":
		// A tag pin drops the variant's digest, and box cannot see whether
		// the two tags are the same bytes.
		oldImage.Digest, newImage.Digest = "", ""
	}
	if h, ok := holdFor(holds, app); ok && !pinned {
		// The app runs what the rollback left it on, not what this release's
		// chart says: its old side is the hold's image, and its new side stays
		// that image until a release pins another.
		moved := imageRef(newImage)
		if !holdLapsed(moved, h) {
			return moveHeldBack, ImageChange{}
		}
		if !sameImage(h.At, moved) {
			return moveMoves, ImageChange{Ran: h.At, MovedTo: moved}
		}
		return moveNone, ImageChange{}
	}
	if imageMoved(oldImage, newImage) {
		return moveMoves, ImageChange{Ran: imageRef(oldImage), MovedTo: imageRef(newImage)}
	}
	if chartMoved && pinned {
		return moveHeld, ImageChange{}
	}
	return moveNone, ImageChange{}
}

// imageMoved reports whether two releases pin different images for one app, by
// the identity a rollback reads (sameImage): the repository, then the digest
// when either side has one, then the tag.
//
// The tag is the case the digest cannot answer. A release that pins no digest --
// a locally built image, or a variant released before its first digest pin --
// moves an app by tag alone: Helm rolls the pod and the app may migrate, so a
// step that read that as no move would snapshot nothing, roll nothing back, and
// say "no image moves" about a release that replaced every pod it names.
func imageMoved(oldImage, newImage admission.Image) bool {
	return !sameImage(imageRef(oldImage), imageRef(newImage))
}

// imageRef is the chart's image as the snapshot records it.
func imageRef(img admission.Image) snapshot.ImageRef {
	return snapshot.ImageRef{Repository: img.Repository, Tag: img.Tag, Digest: img.Digest}
}

// pinOf is the pin the values put on one app's image: the digest when they set
// one, otherwise the tag, which drops the variant's digest. Values that set
// neither leave the chart's own pin in place.
func pinOf(values map[string]any, app string) (tag, digest string) {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	digest, _ = image["digest"].(string)
	tag, _ = image["tag"].(string)
	return strings.TrimSpace(tag), strings.TrimSpace(digest)
}

// pinLabel names the pin the values hold an app at, for the plan: the tag when
// they pin one, else the digest. renderPlan has no values of its own, so the
// plan carries the labels.
func pinLabel(values map[string]any, app string) string {
	tag, digest := pinOf(values, app)
	if tag != "" {
		return tag
	}
	return digest
}

func hasApp(images map[string]map[string]admission.Image, app string) bool {
	_, ok := images[app]
	return ok
}

// variantOf is the variant the values select for one app. Values own the
// choice; the repository itself is always the chart's.
func variantOf(values map[string]any, app string) string {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	variant, _ := image["variant"].(string)
	variant = strings.TrimSpace(variant)
	if variant == "" {
		return "default"
	}
	return variant
}
