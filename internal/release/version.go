package release

import (
	"fmt"
	"sort"

	"github.com/Masterminds/semver/v3"
)

// VersionSkip is one release box passed over because its min-box is newer than
// this box. Selecting a version records every one of them, because a user who
// asked for "the newest" is owed the reason they did not get it.
type VersionSkip struct {
	Version string
	MinBox  string
}

// parseVersion reads one version the way semver orders it. A leading "v" is
// accepted, because a tag may carry one.
func parseVersion(v string) (*semver.Version, error) {
	return semver.NewVersion(v)
}

// selectVersion picks the release box installs from the tags a chart repository
// publishes: the newest one this box can use.
//
// Ordering is semver, not string order, so 0.10.0 is newer than 0.9.0. Tags
// that are not versions at all (a "latest" that someone pushed by hand) are
// ignored. Prereleases are ignored by default: a tag is how a release is
// published, and an rc on the published list is not one this box should install
// unasked; naming one explicitly with --chart-version is the escape hatch.
//
// minBoxOf supplies one version's box.s5n/min-box annotation, which means
// pulling that release; it is a callback so this stays a pure function over a
// tag list and can be table-tested. An error from it stops the selection: box
// will not fall through to an older release because the registry failed to
// serve a newer one.
func selectVersion(tags []string, boxVersion string, minBoxOf func(version string) (string, error)) (string, []VersionSkip, error) {
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
	for _, r := range releases {
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
	return "", skipped, fmt.Errorf("every published release needs a newer box than %s", boxVersion)
}
