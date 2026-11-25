package apply

// versions.go is the confirm's version column: what the source ran, what the
// release runs, and the one refusal the pair can make (docs/output.md, "The
// migration's screens").
//
// An app's config is written by the version that ran it, and an older app is not
// one anybody can trust with a newer app's database. So the two versions are on
// the row a person decides from, an upgrade is marked, and a downgrade stops the
// run before anything moves unless they say --yolo.
//
// The comparison reads the version a person reads. A tag is a version with the
// packaging around it stripped -- version-4.0.14.2939, v1.36.1, release-5.0,
// 5.3.0-lt1.2 -- and the fourth part and beyond is a build number (Plex's
// 1.43.5.11079, Sonarr's 5.0.0.3041), so it is in neither the column nor the
// comparison: two builds of one version are one version.

import (
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// AppVersion is one app's row in the version column: the image the source ran and
// the image this release runs for it.
type AppVersion struct {
	App string
	// Source and Box are the versions as a person reads them, empty when that
	// side names no version this box can read (a digest, `latest`, an image it
	// has no tag for).
	Source, Box string
	// Known says both sides read as a version, so they can be compared. A pair
	// with one unknown is `?` and is never a refusal: box does not stop a
	// migration over something it could not read.
	Known bool
	// Cmp is the release against the source: -1 when the release runs an older
	// app, 0 when they are the same version, 1 when it runs a newer one.
	Cmp int
}

// Downgrade says this release runs an older app than wrote the config.
func (v AppVersion) Downgrade() bool { return v.Known && v.Cmp < 0 }

// Cell is the row's text: one version when there is nothing to compare, the pair
// with the direction when there is, and `?` when this box could not read one side.
// It is empty when this run had no chart to ask, so a summary built without one
// says nothing about versions rather than a column of question marks.
func (v AppVersion) Cell() string {
	switch {
	case v.Source == "" && v.Box == "":
		return ""
	case !v.Known:
		return "?"
	case v.Cmp == 0:
		return v.Box
	case v.Cmp > 0:
		return v.Source + " → " + v.Box + "  ↑"
	default:
		return v.Source + " → " + v.Box + "  ↓"
	}
}

// versionOf reads one app's pair: the image the source ran, from the plan, and
// the image this release runs for it, from the chart.
func versionOf(instance, source, release string) AppVersion {
	v := AppVersion{App: instance, Source: tagVersion(source), Box: tagVersion(release)}
	_, sourceOK := parseVersion(source)
	_, boxOK := parseVersion(release)
	v.Known = sourceOK && boxOK
	if v.Known {
		v.Cmp = compareVersion(source, release)
	}
	return v
}

// Versions is the confirm's version column, from the plan and the chart alone:
// one row per planned instance, in the summary's order, with no staging copy and
// no cluster behind it.
//
// It is what the confirm prints and what the refusal reads, so the run can
// refuse a downgrade before it copies an app's config -- a release older than
// the version that wrote the source is a fact about two image references. image
// is the chart's own resolution, empty for a chart that has no image for the app
// (the row then reads `?`).
func Versions(p *plan.Plan, catalog []string, image func(app string) string) []AppVersion {
	names := planApps(p)
	out := make([]AppVersion, 0, len(names))
	for _, name := range names {
		app := CatalogApp(name, catalog)
		release := ""
		if image != nil && app != "" {
			release = image(app)
		}
		out = append(out, versionOf(name, p.Apps[name].Image, release))
	}
	return out
}

// Downgrades filters a version column to the apps this release would run at an
// older version than wrote their config, in the column's order.
func Downgrades(versions []AppVersion) []AppVersion {
	var out []AppVersion
	for _, v := range versions {
		if v.Downgrade() {
			out = append(out, v)
		}
	}
	return out
}

// Versions is the summary's own version column, in its order: the rows the
// confirm prints, so a caller that has the summary refuses on exactly what the
// operator was shown.
func (s Summary) Versions() []AppVersion {
	versions := make([]AppVersion, 0, len(s.Instances))
	for _, in := range s.Instances {
		versions = append(versions, in.Version)
	}
	return versions
}

// Downgrades is every app this release would run at an older version than the
// source's config was written by, sorted by name (the summary's order).
func (s Summary) Downgrades() []AppVersion { return Downgrades(s.Versions()) }

// tagVersion is the version an image reference names, as a person reads it, or ""
// when it names none box can read.
func tagVersion(ref string) string {
	parts, ok := parseVersion(ref)
	if !ok {
		return ""
	}
	shown := parts
	if len(shown) > versionParts {
		shown = shown[:versionParts]
	}
	// A version reads with at least its major and minor part, and without a
	// trailing zero: 5.3.0 is 5.3, and 5.0.0 is 5.0.
	floor := 2
	if len(shown) < floor {
		floor = len(shown)
	}
	for len(shown) > floor && shown[len(shown)-1] == 0 {
		shown = shown[:len(shown)-1]
	}
	out := make([]string, len(shown))
	for i, n := range shown {
		if n == wildcard {
			out[i] = "x"
			continue
		}
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ".")
}

// versionParts is how many parts of a version a person reads: major, minor and
// patch. The rest is the build.
const versionParts = 3

// wildcard is a part a project writes as a wildcard (`2.x`): it reads as the
// zero a missing part reads as, and it decides nothing.
const wildcard = -1

// parseVersion reads the version out of an image reference: the tag, less the
// packaging a project puts around it. ok is false for a reference with no tag
// (a bare digest, `latest`), which is the `?` the column shows.
//
// A part a project wildcards (`2.x`) is read as the number it starts with, so a
// pair of one decidable part still compares.
func parseVersion(ref string) ([]int, bool) {
	tag := ref
	if i := strings.IndexByte(tag, '@'); i >= 0 {
		tag = tag[:i]
	}
	// The colon after the last slash is the tag's; before it, it is a
	// registry's port.
	if i := strings.LastIndexByte(tag, ':'); i >= 0 && !strings.Contains(tag[i:], "/") {
		tag = tag[i+1:]
	} else {
		return nil, false
	}
	tag = strings.TrimSpace(tag)
	for _, prefix := range []string{"version-", "release-", "rel-"} {
		if len(tag) > len(prefix) && strings.EqualFold(tag[:len(prefix)], prefix) {
			tag = tag[len(prefix):]
			break
		}
	}
	if len(tag) > 1 && (tag[0] == 'v' || tag[0] == 'V') && tag[1] >= '0' && tag[1] <= '9' {
		tag = tag[1:]
	}
	// A tag's own suffix is the build's packaging, not the version:
	// 5.3.0-lt1.2 is qBittorrent 5.3.0 on libtorrent 1.2.
	if i := strings.IndexByte(tag, '-'); i > 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return nil, false
	}
	fields := strings.Split(tag, ".")
	parts := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			return nil, false
		}
		if f == "x" || f == "X" || f == "*" {
			// A wildcard is one part box cannot compare, and it says nothing
			// about the parts after it: it reads as the zero a missing part
			// reads as, and the column shows it as it was written.
			parts = append(parts, wildcard)
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

// compareVersion is the two versions' order by the parts a person reads: the
// build is compared not at all.
func compareVersion(source, release string) int {
	a, _ := parseVersion(source)
	b, _ := parseVersion(release)
	if len(a) > versionParts {
		a = a[:versionParts]
	}
	if len(b) > versionParts {
		b = b[:versionParts]
	}
	for len(a) < len(b) {
		a = append(a, 0)
	}
	for len(b) < len(a) {
		b = append(b, 0)
	}
	for i := range a {
		if a[i] == wildcard || b[i] == wildcard {
			continue
		}
		switch {
		case a[i] < b[i]:
			return 1
		case a[i] > b[i]:
			return -1
		}
	}
	return 0
}
