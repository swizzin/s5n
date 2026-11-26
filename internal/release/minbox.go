package release

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	// MinBoxAnnotation names the oldest box that may install a release
	// (docs/box.md, "A minimum box version"). It rises only when a shared
	// interface changes, never for an app, digest or optional key.
	MinBoxAnnotation = "box.s5n/min-box"
	// UpgradeFromAnnotation is the oldest release this chart can be upgraded
	// from; `box update` acts on it.
	UpgradeFromAnnotation = "box.s5n/upgrade-from"

	// DevVersion is a box built without -X main.version; it skips min-box and
	// says so once.
	DevVersion = "dev"
)

// CheckMinBox refuses a release whose min-box is newer than this box. It runs
// before a cluster client exists, so a refusal cannot have created anything. A
// chart with no annotation is accepted: it has no floor to meet.
func CheckMinBox(boxVersion string, chart Chart) error {
	minBox := strings.TrimSpace(chart.Annotations[MinBoxAnnotation])
	if minBox == "" {
		return nil
	}
	floor, err := semver.NewVersion(minBox)
	if err != nil {
		return fmt.Errorf("chart %s %s: annotation %s %q is not a version: %v",
			chart.Name, chart.Version, MinBoxAnnotation, minBox, err)
	}
	if IsDevVersion(boxVersion) {
		return nil
	}
	own, err := semver.NewVersion(boxVersion)
	if err != nil {
		return fmt.Errorf("box version %q is not a version, so it cannot be compared with %s %q: build box with -X main.version",
			boxVersion, MinBoxAnnotation, minBox)
	}
	if own.LessThan(floor) {
		return fmt.Errorf("chart %s %s needs box %s or newer; this is box %s, so it cannot install that release: install a newer box",
			chart.Name, chart.Version, minBox, boxVersion)
	}
	return nil
}

// IsDevVersion reports a box built without a version; "dev" cannot be
// compared with a floor.
func IsDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == DevVersion
}
