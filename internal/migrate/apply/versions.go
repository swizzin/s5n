package apply

// versions.go is the confirm's version column: what the source ran, what the
// release runs, and the downgrade refusal (docs/output.md, "The migration's
// screens"). An app's config was written by the version that ran it, so a
// downgrade stops the run unless the operator says --yolo. The comparison reads the
// version a person reads: packaging stripped, and the fourth part and beyond (a
// build number) ignored.

import (
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// AppVersion is one app's row in the version column: the image the source ran and
// the image this release runs for it.
type AppVersion struct {
	App string
	// Source and Box are readable versions; empty for a digest or `latest`.
	Source, Box string
	// Known says both sides read as versions; an unknown side is `?`, never a
	// refusal.
	Known bool
	// Cmp is the release against the source: -1 when the release runs an older
	// app, 0 when they are the same version, 1 when it runs a newer one.
	Cmp int
}

// Downgrade says this release runs an older app than wrote the config.
func (v AppVersion) Downgrade() bool { return v.Known && v.Cmp < 0 }

// Cell is the row's text: one version, a marked pair, `?`, or empty with no chart.
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

// Versions is the column from the plan and chart alone: the confirm prints it and
// the refusal reads it, so a downgrade stops before any copy.
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

// tagVersion is the readable version an image names, or "".
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

// versionParts is major, minor and patch; the rest is the build.
const versionParts = 3

// wildcard reads as the zero a missing part does, and decides nothing.
const wildcard = -1

// parseVersion reads the tag minus packaging; ok is false for a digest or
func parseVersion(ref string) ([]int, bool) {
	tag := ref
	if i := strings.IndexByte(tag, '@'); i >= 0 {
		tag = tag[:i]
	}
	// Only the colon after the last slash is the tag's.
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
	// A suffix is packaging, not version: 5.3.0-lt1.2 is 5.3.0 on libtorrent 1.2.
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
			// A wildcard says nothing about later parts; the column shows it as
			// written.
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
