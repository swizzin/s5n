package release

import (
	"fmt"
	"sort"

	"github.com/Masterminds/semver/v3"
)

// VersionSkip is a release passed over for a newer min-box; a user who asked
// for the newest is owed the reason.
type VersionSkip struct {
	Version string
	MinBox  string
}

// Published is one released tag and the two annotations box plans with. An OCI
// tag knows nothing about which release may follow which, so this is read from
// the archive, one cached pull per version (see Releases).
type Published struct {
	// Version is the published tag, equal to the chart's version.
	Version string
	// MinBox and UpgradeFrom are the annotations; empty means no floor.
	MinBox      string
	UpgradeFrom string
}

// parseVersion reads one version the way semver orders it. A leading "v" is
// accepted, because a tag may carry one.
func parseVersion(v string) (*semver.Version, error) {
	return semver.NewVersion(v)
}

// SelectVersion picks the newest release this box can use. Ordering is semver,
// not string order; non-versions are ignored, and prereleases are ignored unless
// named with --chart-version. minBoxOf pulls one release's annotation, and an
// error stops selection rather than falling through to an older release.
// exclude names releases a step failed on: treated as absent, not refused.
func SelectVersion(tags []string, boxVersion string, minBoxOf func(version string) (string, error), exclude func(version string) bool) (string, []VersionSkip, error) {
	type release struct {
		tag     string
		version *semver.Version
	}
	var releases []release
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}
		if v.Prerelease() != "" {
			continue
		}
		releases = append(releases, release{tag: tag, version: v})
	}
	sort.SliceStable(releases, func(i, j int) bool {
		if c := releases[i].version.Compare(releases[j].version); c != 0 {
			return c > 0
		}
		return releases[i].tag > releases[j].tag
	})
	if len(releases) == 0 {
		return "", nil, fmt.Errorf("no published release is a version box can install")
	}

	var skipped []VersionSkip
	excluded := 0
	for _, r := range releases {
		if exclude != nil && exclude(r.tag) {
			excluded++
			continue
		}
		minBox, err := minBoxOf(r.tag)
		if err != nil {
			return "", skipped, err
		}
		if minBox == "" || IsDevVersion(boxVersion) {
			return r.tag, skipped, nil
		}
		floor, err := semver.NewVersion(minBox)
		if err != nil {
			return "", skipped, fmt.Errorf("release %s: annotation %s %q is not a version: %v",
				r.tag, MinBoxAnnotation, minBox, err)
		}
		own, err := semver.NewVersion(boxVersion)
		if err != nil {
			return "", skipped, fmt.Errorf("box version %q is not a version: %v", boxVersion, err)
		}
		if own.LessThan(floor) {
			skipped = append(skipped, VersionSkip{Version: r.tag, MinBox: minBox})
			continue
		}
		return r.tag, skipped, nil
	}
	if excluded == len(releases) {
		return "", skipped, fmt.Errorf("every published release has already failed on this box")
	}
	return "", skipped, fmt.Errorf("every published release needs a newer box than %s", boxVersion)
}
