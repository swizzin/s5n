package update

import (
	"errors"
	"fmt"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ConfigRoot is the grant's storage.config.path, whose child per instance is
// that instance's /config. Snapshots and holds both sit under it.
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

// HoldStore is the record of apps held at the image they ran before a step
// moved them: `<config root>/.box/holds.json`. The path is an argument because a
// hold lives beside its config root.
type HoldStore interface {
	Load(configRoot string) ([]snapshot.Hold, error)
	Save(configRoot string, holds []snapshot.Hold) error
}

// PinStore is the record of the digest each app's tag pin resolved to:
// `<config root>/.box/pins.json`. The path is an argument because the record lives
// beside its config root, like a hold.
type PinStore interface {
	Load(configRoot string) ([]snapshot.Pin, error)
	Save(configRoot string, pins []snapshot.Pin) error
}

// FilePins is PinStore over internal/snapshot's file.
type FilePins struct{}

// Load reads the records, in app order. A box that pins no tag has no file.
func (FilePins) Load(configRoot string) ([]snapshot.Pin, error) {
	return snapshot.LoadPins(configRoot)
}

// Save replaces the records, removing the file when the list is empty.
func (FilePins) Save(configRoot string, pins []snapshot.Pin) error {
	return snapshot.SavePins(configRoot, pins)
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

// holdLapsed reports whether a release pins something other than the image a
// rollback avoided, using sameImage's identity. The tag fallback matters: a
// release pinning no digest would otherwise leave the hold forever, because ""
// equals "" for every tag.
func holdLapsed(pinned snapshot.ImageRef, h snapshot.Hold) bool {
	return !sameImage(pinned, h.Avoiding)
}

// Lapse is one hold a release has moved past, and the sentence box says when it
// removes it.
type Lapse struct {
	Hold snapshot.Hold
	Text string
}

// HoldValues is the overlay keeping each held app on its image, and the holds
// this release lapsed. A pin wins; another pinned image lapses; an unnameable
// variant stands.
//
// Lapsed holds are removed only after an apply without them succeeded.
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
		// A validated hold always names an image; this cannot happen.
		return
	}
	apps, _ := out["apps"].(map[string]any)
	if apps == nil {
		apps = make(map[string]any)
		out["apps"] = apps
	}
	apps[h.App] = map[string]any{"image": image}
}

// chartAppImage is the image a release pins for an app's selected variant; a
// chart or variant that says nothing yields false.
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
