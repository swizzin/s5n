package update

import (
	"errors"
	"fmt"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ConfigRoot is the grant's storage.config.path: the host directory whose child
// per instance is that instance's /config (the chart's subPath). Snapshots are
// taken under it and the holds that name them sit beside it, so a caller that
// reads or writes either names the same directory from here.
func ConfigRoot(sb *seedbox.Seedbox) (string, error) {
	vols, err := sb.Storage()
	if err != nil {
		return "", err
	}
	for _, v := range vols {
		if v.Kind == "config" {
			return v.Path, nil
		}
	}
	return "", errors.New("the grant names no config volume, so there is nowhere to copy an app's config")
}

// loadHolds is the box's holds, or none when the caller keeps no store.
func loadHolds(store HoldStore, configRoot string) ([]snapshot.Hold, error) {
	if store == nil {
		return nil, nil
	}
	return store.Load(configRoot)
}

// HoldStore is the record of the apps box is holding at the image they ran
// before a step moved them: `<config root>/.box/holds.json`, written by a
// rollback and read by every plan and apply.
//
// It is an interface for the same reason the rest of this package is: a test
// plans and rolls back against a value it can assert on, and cmd/box wires the
// file in. The path comes from the grant, so it is an argument rather than a
// field: a hold lives beside the config root it belongs to.
type HoldStore interface {
	Load(configRoot string) ([]snapshot.Hold, error)
	Save(configRoot string, holds []snapshot.Hold) error
}

// FileHolds is HoldStore over internal/snapshot's file.
type FileHolds struct{}

// Load reads the holds, in app order. A box that holds nothing has no file.
func (FileHolds) Load(configRoot string) ([]snapshot.Hold, error) {
	return snapshot.LoadHolds(configRoot)
}

// Save replaces the holds, removing the file when the list is empty.
func (FileHolds) Save(configRoot string, holds []snapshot.Hold) error {
	return snapshot.SaveHolds(configRoot, holds)
}

// holdFor is one app's hold, if the box keeps one.
func holdFor(holds []snapshot.Hold, app string) (snapshot.Hold, bool) {
	for _, h := range holds {
		if h.App == app {
			return h, true
		}
	}
	return snapshot.Hold{}, false
}

// holdLapsed reports whether one release has moved past a hold: it pins, for
// the app's selected variant, a repository or digest other than the image the
// rollback avoided. The digest is what the chart pins, so a release that
// re-pushed the avoided tag to new bytes lapses the hold too.
func holdLapsed(pinned snapshot.ImageRef, h snapshot.Hold) bool {
	return pinned.Repository != h.Avoiding.Repository || pinned.Digest != h.Avoiding.Digest
}

// Lapse is one hold a release has moved past, and the sentence box says when it
// removes it: the app moves with the release again because the chart pins
// something other than the image the rollback avoided.
type Lapse struct {
	Hold snapshot.Hold
	Text string
}

// HoldValues is the overlay release.Options.Holds applies: the values that keep
// each held app on the image it ran, and the holds this release has lapsed.
//
// A hold only ever says an app runs the image a snapshot recorded, so it loses
// to anything with a better claim:
//
//   - a pin in the operator's values wins, because a hand pin is the operator's
//     decision about this app and the hold is box's;
//   - a release that pins a different repository or digest has lapsed the hold,
//     because the chart's own image is what the next release is for;
//   - a release whose catalog cannot say what the app's variant is cannot be
//     compared, so the hold stands.
//
// The lapsed list is what the caller removes from the file, and only after an
// apply that rendered without those holds has succeeded: a hold dropped by a
// run whose apply failed would move the app onto the image its rollback avoided.
func HoldValues(chart release.Chart, values map[string]any, holds []snapshot.Hold) (map[string]any, []Lapse, error) {
	out := make(map[string]any)
	var lapsed []Lapse
	for _, h := range holds {
		if tag, digest := pinOf(values, h.App); tag != "" || digest != "" {
			continue
		}
		pinned, ok := chartAppImage(chart, values, h.App)
		if !ok {
			addHold(out, h)
			continue
		}
		if holdLapsed(pinned, h) {
			lapsed = append(lapsed, Lapse{Hold: h, Text: lapseText(chart, h, pinned)})
			continue
		}
		addHold(out, h)
	}
	return out, lapsed, nil
}

// LapseText is the sentence box says when it removes a lapsed hold.
func lapseText(chart release.Chart, h snapshot.Hold, pinned snapshot.ImageRef) string {
	return fmt.Sprintf("%s moves with the release again: %s %s pins %s, not %s",
		h.App, chart.Name, chart.Version, pinned.Label(), h.Avoiding.Label())
}

// addHold puts one app's image into the overlay.
func addHold(out map[string]any, h snapshot.Hold) {
	image := make(map[string]any, 2)
	if h.At.Digest != "" {
		image["digest"] = h.At.Digest
	}
	if h.At.Tag != "" {
		image["tag"] = h.At.Tag
	}
	if len(image) == 0 {
		// A hold is validated when it is read and when it is written, so
		// this is a value no file box accepted could carry.
		return
	}
	apps, _ := out["apps"].(map[string]any)
	if apps == nil {
		apps = make(map[string]any)
		out["apps"] = apps
	}
	apps[h.App] = map[string]any{"image": image}
}

// chartAppImage is the image one release pins for an app, for the variant the
// values select. A chart without variants.yaml, or one whose app or variant is
// not there, says nothing.
func chartAppImage(chart release.Chart, values map[string]any, app string) (snapshot.ImageRef, bool) {
	if len(chart.Variants) == 0 {
		return snapshot.ImageRef{}, false
	}
	images, err := admission.Images(chart.Variants)
	if err != nil {
		return snapshot.ImageRef{}, false
	}
	img, ok := images[app][variantOf(values, app)]
	if !ok {
		return snapshot.ImageRef{}, false
	}
	return imageRef(img), true
}
