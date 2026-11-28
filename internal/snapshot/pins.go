package snapshot

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PinsName is the file beside the snapshots that records the digest box resolved
// for each app the values hold on a tag.
const PinsName = "pins.json"

// Pin is one app's tag pin as box last resolved it: the tag keeps following its
// publisher, and the digest is what this box renders until the tag resolves
// somewhere else. It never goes into the values file: a digest there would turn
// "follow this tag" into "freeze these bytes".
type Pin struct {
	// App is the app key (sonarr, not sonarr-4k): a pin overlays
	// apps.<app>.image, which instances share.
	App string `json:"app"`
	// Repository, Tag and Digest are the reference the tag resolved to.
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	// Resolved is when box last asked the registry and got this digest.
	Resolved time.Time `json:"resolved"`
	// Chart is the release box was resolving for, so a later run can say which
	// box wrote the record.
	Chart string `json:"chart"`
}

// pinsFile is the file's shape: one object with one key, so a file that grows a
// field later cannot be mistaken for a list of pins.
type pinsFile struct {
	Pins []Pin `json:"pins"`
}

// PinsPath is where a seedbox's pins live: <config root>/.box/pins.json, beside
// the records a rollback hold keeps, under a directory the seedbox's own box can
// write.
func PinsPath(configRoot string) string {
	return filepath.Join(configRoot, boxDir, PinsName)
}

// LoadPins reads the box's resolved tags, in app order. A box that pins no tag has
// no file, which is not an error; anything else is an error naming the path, because
// guessing here renders a tag without the digest box meant to run.
func LoadPins(configRoot string) ([]Pin, error) {
	doc, ok, err := loadBoxFile[pinsFile](configRoot, pinsFileRecord)
	if err != nil || !ok {
		return nil, err
	}
	pins, err := checkedPins(doc.Pins)
	if err != nil {
		return nil, fmt.Errorf("pins %s: %w", PinsPath(configRoot), err)
	}
	for i := range pins {
		pins[i].Resolved = pins[i].Resolved.UTC()
	}
	return pins, nil
}

// SavePins writes the box's resolved tags, sorted by app and at most one per app,
// through internal/atomicfile so a crash leaves the file as it was or complete. An
// empty list removes the file: its absence is what LoadPins reads as none.
func SavePins(configRoot string, pins []Pin) error {
	checked, err := checkedPins(pins)
	if err != nil {
		return fmt.Errorf("pins %s: %w", PinsPath(configRoot), err)
	}
	for i := range checked {
		checked[i].Resolved = checked[i].Resolved.UTC()
	}
	return saveBoxFile(configRoot, pinsFileRecord, pinsFile{Pins: checked}, len(checked) == 0)
}

// WithPin puts one app's resolution in place of any it had, in app order: a tag
// followed from one digest to another leaves one record, not two.
func WithPin(pins []Pin, p Pin) []Pin {
	out := WithoutPin(pins, p.App)
	out = append(out, p)
	sortPins(out)
	return out
}

// WithoutPin drops one app's resolution and keeps the rest in order. Removing one
// an app does not have is not a change.
func WithoutPin(pins []Pin, app string) []Pin {
	out := make([]Pin, 0, len(pins))
	for _, p := range pins {
		if p.App != app {
			out = append(out, p)
		}
	}
	return out
}

// PinFor is one app's record, if the box keeps one.
func PinFor(pins []Pin, app string) (Pin, bool) {
	for _, p := range pins {
		if p.App == app {
			return p, true
		}
	}
	return Pin{}, false
}

// checkedPins copies a list, sorts it by app and refuses anything box could not
// render: a name that is not an instance name, a reference missing a part, a
// digest that is not one, two records for one app.
func checkedPins(pins []Pin) ([]Pin, error) {
	out := make([]Pin, len(pins))
	copy(out, pins)
	sortPins(out)
	for i, p := range out {
		if err := checkPin(p); err != nil {
			return nil, err
		}
		if i > 0 && out[i-1].App == p.App {
			return nil, fmt.Errorf("%s is pinned twice; a box keeps one record per app", p.App)
		}
	}
	return out, nil
}

// checkPin refuses one record box could not render, or could render against the
// wrong image.
func checkPin(p Pin) error {
	if err := checkApp(p.App); err != nil {
		return fmt.Errorf("app %q: %w", p.App, err)
	}
	if p.Repository == "" {
		return fmt.Errorf("app %q: has no repository", p.App)
	}
	if strings.ContainsAny(p.Repository, " \t\r\n") {
		return fmt.Errorf("app %q: repository %q has whitespace", p.App, p.Repository)
	}
	if err := checkName(p.Tag); err != nil {
		return fmt.Errorf("app %q: tag %q %w", p.App, p.Tag, err)
	}
	if err := CheckDigest(p.Digest); err != nil {
		return fmt.Errorf("app %q: digest %q %w", p.App, p.Digest, err)
	}
	return nil
}

// CheckDigest refuses anything that is not an OCI content digest: a record box
// cannot render is a pod that never pulls.
func CheckDigest(digest string) error {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	switch {
	case !ok:
		return errors.New("is not sha256:<64 hex>")
	case len(hex) != 64:
		return errors.New("is not sha256:<64 hex>: it has the wrong length")
	}
	for _, r := range hex {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return errors.New("is not lowercase hex")
	}
	return nil
}

// sortPins orders pins by app, so two runs that follow the same apps write the
// same bytes.
func sortPins(pins []Pin) {
	sort.Slice(pins, func(i, j int) bool { return pins[i].App < pins[j].App })
}
