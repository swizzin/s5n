package update

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/snapshot"
)

// Step is one release-to-release move; the pair names a snapshot.
type Step struct {
	From string
	To   string
}

// planSteps is the update path: the releases to install, in order, so no
// migration is skipped. A release's box.s5n/upgrade-from is the oldest it can be
// installed over; when the target's floor is above installed, step to the
// newest release between that the current one meets and this box's min-box
// allows. Every step is published -- an invented version is a migration nobody
// ran -- and a malformed annotation is an error, never a skipped migration.
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
		// A prerelease is never an unasked step, but --chart-version may name
		// one as the target, so it must be found here.
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
			// Never loop or move a box backwards on a stale release list.
			return nil, fmt.Errorf("cannot update from %s to %s: release %s does not move the box forward",
				installed, target, next.Version)
		}
		steps = append(steps, Step{From: from.Original(), To: next.Version})
		cur = version
	}
	return steps, nil
}

// nextStep is one hop: the target when its floor allows it, else the newest
// release between that does and that this box meets min-box for.
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

// upgradeFrom is one release's floor, nil when it declares none.
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

// meetsMinBox: a release this box is too old for is not a step.
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

// ImageChange is one app's image on each side of a step; the pair travels with
// the plan because after the release moves nothing can say what it ran.
type ImageChange struct {
	Ran     snapshot.ImageRef
	MovedTo snapshot.ImageRef
	// Snapshot says the catalog names files an upgrade can rewrite, so the step
	// stops and copies the app; one without them is moved uncopied, unstoppable
	// and never rolled back.
	Snapshot bool
}

// movingApps is the enabled apps whose image moves, the ones a pin or hold keeps
// in place, and each move's image pair -- exactly what a step snapshots.
//
// Images compare by repository and digest, by tag when neither release pins one.
// An app in only one release has nothing to migrate. A pin holds an app on one
// image; it cannot hold one whose repository changes.
func movingApps(from, to release.Chart, values map[string]any, holds []snapshot.Hold, answers map[string]pin.Answer) (moving, held, heldBack []string, images map[string]ImageChange, err error) {
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
		kind, change := appDecision(app, values, oldImages, newImages, holds, answers)
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
	// moveNone: not affected. moveMoves: image changes. moveHeld: a values pin
	// holds it. moveHeldBack: a rollback hold keeps it on its image.
	moveNone appMove = iota
	moveMoves
	moveHeld
	moveHeldBack
)

// appDecision decides one app's fate: selected variant, both releases' images,
// values pins and holds.
func appDecision(app string, values map[string]any, oldImages, newImages map[string]map[string]admission.Image, holds []snapshot.Hold, answers map[string]pin.Answer) (appMove, ImageChange) {
	variant := variantOf(values, app)
	oldImage, oldOK := oldImages[app][variant]
	newImage, newOK := newImages[app][variant]
	oldApp, newApp := hasApp(oldImages, app), hasApp(newImages, app)
	switch {
	case oldOK && newOK:
		return shippedMove(app, values, oldImage, newImage, holds, answers)
	case !oldApp && !newApp:
		// Neither release ships it: there is no install to migrate.
		return moveNone, ImageChange{}
	case oldApp && newApp:
		// The variant is missing on one side: box cannot tell whether the image
		// moved, so snapshot. A zero image makes the copy refuse rather than
		// record one box never saw.
		return moveMoves, ImageChange{Ran: imageRef(oldImage), MovedTo: imageRef(newImage)}
	default:
		// The app is in one release only; nothing to migrate.
		return moveNone, ImageChange{}
	}
}

// shippedMove decides an app both releases ship: the digest is the image, so a
// re-pushed tag reads as a move. A pin is applied first; a hold decides only
// when no pin does.
func shippedMove(app string, values map[string]any, oldImage, newImage admission.Image, holds []snapshot.Hold, answers map[string]pin.Answer) (appMove, ImageChange) {
	// What the chart did is remembered first, because the pin may hide it.
	chartMoved := imageMoved(oldImage, newImage)
	tag, digest := pinOf(values, app)
	pinned := tag != "" || digest != ""
	switch {
	case digest != "":
		oldImage.Digest, newImage.Digest = digest, digest
	case tag != "":
		if answer, ok := answers[app]; ok {
			return pinnedMove(answer, chartMoved)
		}
		// A tag pin with no registry answer drops the digest: box cannot see
		// whether the tags match, and must not read a rebuild as no move.
		oldImage.Digest, newImage.Digest = "", ""
	}
	if h, ok := holdFor(holds, app); ok && !pinned {
		// The app runs what the rollback left it on, not this chart's image.
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

// pinnedMove decides a tag-pinned app from what the tag resolved to: a rebuilt tag
// is an image move, and the chart's own pins are not the app's image at all.
func pinnedMove(answer pin.Answer, chartMoved bool) (appMove, ImageChange) {
	ran := snapshot.ImageRef{Repository: answer.Repository, Tag: answer.Tag, Digest: answer.Recorded}
	movedTo := snapshot.ImageRef{Repository: answer.Repository, Tag: answer.Tag, Digest: answer.Digest}
	switch {
	case answer.Digest == "":
		// Nothing to render; Resolve refuses this pin before the plan.
		return moveNone, ImageChange{}
	case answer.Recorded == "" || answer.Moved():
		return moveMoves, ImageChange{Ran: ran, MovedTo: movedTo}
	case chartMoved:
		return moveHeld, ImageChange{}
	}
	return moveNone, ImageChange{}
}

// imageMoved compares two images by sameImage's identity. The tag is the case a
// digest cannot answer: a release pinning none moves the app by tag alone, and
// reading that as no move would snapshot nothing for a release that replaced
// every pod.
func imageMoved(oldImage, newImage admission.Image) bool {
	return !sameImage(imageRef(oldImage), imageRef(newImage))
}

// imageRef is the chart's image as the snapshot records it.
func imageRef(img admission.Image) snapshot.ImageRef {
	return snapshot.ImageRef{Repository: img.Repository, Tag: img.Tag, Digest: img.Digest}
}

// pinOf is the values' pin on one app: digest, else tag (which drops the
// variant's digest); neither leaves the chart's.
func pinOf(values map[string]any, app string) (tag, digest string) {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	digest, _ = image["digest"].(string)
	tag, _ = image["tag"].(string)
	return strings.TrimSpace(tag), strings.TrimSpace(digest)
}

// pinLabel names the values' pin for the plan, which carries no values.
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

// variantOf is the values' selected variant, defaulting to "default".
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
