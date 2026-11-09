package release

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	// MinBoxAnnotation is the Chart.yaml annotation naming the oldest box that
	// may install a chart release (docs/box.md, "A minimum box version"). It
	// goes up only when one of the interfaces box and the chart share changes
	// in a way an older box would get wrong — a renamed grant key, a status
	// field whose meaning changed, a new kind of setup question — never for a
	// new app, a digest bump or an optional values key.
	MinBoxAnnotation = "box.s5n/min-box"
	// UpgradeFromAnnotation is the oldest release the chart can be upgraded
	// from. box carries it; `box update`, which steps through intermediate
	// releases, is what will act on it.
	UpgradeFromAnnotation = "box.s5n/upgrade-from"

	// DevVersion is the version of a box built without -X main.version. It
	// cannot be compared with a chart's floor, so it skips the check and says
	// so once; a release build always has one.
	DevVersion = "dev"
)

// CheckMinBox refuses a chart release whose box.s5n/min-box is newer than this
// box. It is a pure function over the chart's annotations and two version
// strings, so the refusal is table-tested without a registry or a cluster.
//
// It runs before anything is written: box loads and checks its chart before it
// opens a cluster client, so a refusal here cannot have created a namespace, a
// volume or a release. A chart with no annotation is accepted — the annotation
// is chart-owned, and a chart that predates it has no floor to meet.
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

// IsDevVersion reports whether box was built without a version. Such a box
// skips the min-box check, because "dev" is not a version anything can be
// compared against.
func IsDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == DevVersion
}
