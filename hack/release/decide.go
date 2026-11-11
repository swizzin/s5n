package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"go.yaml.in/yaml/v3"
)

// The chart's two files, and the only ones a release may be cut for without a
// maintainer: the app images it blesses, and the platform pins box installs.
// A platform patch ships in the chart (docs/box.md, "The platform layer"), so
// both are chart releases, and box's own code and manifests are not here at all.
const (
	tagPrefix    = "s5n-v"
	variantsFile = "charts/s5n/variants.yaml"
	platformFile = "charts/s5n/platform.yaml"
)

// releasedFiles is every path the automatic release may have moved. Anything
// else in the range -- a template, a Go file, a doc -- is a maintainer's
// release, because CI has not proven it safe to publish unattended.
var releasedFiles = map[string]bool{
	variantsFile: true,
	platformFile: true,
}

// Decision is the verdict for the range since the last s5n-v* tag.
type Decision struct {
	// Release is whether the push may cut a chart release.
	Release bool
	// Version is the version to tag, without the s5n-v prefix, when Release
	// is true: the last tag's patch raised by one.
	Version string
	// Reason is one line for the log, whichever way the verdict went.
	Reason string
}

// decideRange reports whether a push to main may cut a chart release.
//
// changed is every path that differs between the last s5n-v* tag and HEAD (the
// net diff, so a merge commit needs no special case of its own); the four byte
// slices are charts/s5n/variants.yaml and charts/s5n/platform.yaml at that tag
// and at HEAD, empty when the file did not exist on that side; prevVersion is
// the tag's version without its prefix, empty when there is no tag.
//
// A release is cut only when every changed path is one of the two files, both
// files' changes are patch or digest bumps, and something moved. That is
// exactly the set Renovate may merge on main unattended, so the release
// confirms a change CI already ran the full e2e on and never invents one.
func decideRange(changed []string, oldVariants, newVariants, oldPlatform, newPlatform []byte, prevVersion string) (Decision, error) {
	if prevVersion == "" {
		// v0.2.0 predates the s5n-v family: it says nothing about what this
		// range contains, so it is not a base to count a patch from.
		return Decision{Reason: "no " + tagPrefix + "* tag to build on: v0.2.0 predates the family, so nothing is released until a maintainer tags one"}, nil
	}
	since := tagPrefix + prevVersion
	if len(changed) == 0 {
		return Decision{Reason: "no commits since " + since}, nil
	}
	var other []string
	for _, path := range changed {
		if !releasedFiles[path] {
			other = append(other, path)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return Decision{Reason: "changed since " + since + ": " + strings.Join(other, ", ") + " (an automatic release only moves " + variantsFile + " and " + platformFile + ")"}, nil
	}

	oldApps, err := parseVariants(oldVariants)
	if err != nil {
		return Decision{}, fmt.Errorf("%s at %s: %w", variantsFile, since, err)
	}
	newApps, err := parseVariants(newVariants)
	if err != nil {
		return Decision{}, fmt.Errorf("%s at HEAD: %w", variantsFile, err)
	}
	images, reason := variantReason(oldApps, newApps)
	if reason != "" {
		return Decision{Reason: reason}, nil
	}

	oldPins, err := parsePins(oldPlatform)
	if err != nil {
		return Decision{}, fmt.Errorf("%s at %s: %w", platformFile, since, err)
	}
	newPins, err := parsePins(newPlatform)
	if err != nil {
		return Decision{}, fmt.Errorf("%s at HEAD: %w", platformFile, err)
	}
	pins, reason := pinReason(oldPins, newPins)
	if reason != "" {
		return Decision{Reason: reason}, nil
	}

	if images+pins == 0 {
		return Decision{Reason: "neither " + variantsFile + " nor " + platformFile + " moved since " + since}, nil
	}
	prev, err := semver.NewVersion(prevVersion)
	if err != nil {
		return Decision{}, fmt.Errorf("%s%s is not a version: %w", tagPrefix, prevVersion, err)
	}
	next := prev.IncPatch()
	return Decision{
		Release: true,
		Version: next.String(),
		Reason: fmt.Sprintf("%d app image and %d platform pin bump(s) since %s, all patches or digests: releasing %s%s",
			images, pins, since, tagPrefix, next),
	}, nil
}

// variant is one image the chart ships for an app: the repository, the tag and
// the digest. The tag and digest may be empty: that is the s5n image's own
// entry, whose tag is the chart's appVersion and whose digest is stamped at
// packaging, never committed.
type variant struct {
	Repository string
	Tag        string
	Digest     string
}

// parseVariants reads charts/s5n/variants.yaml as app -> variant -> image. The
// shape is the chart's own (internal/admission reads the same file the same
// way): a top-level key per app, a key per variant, and repository/tag/digest
// under it.
func parseVariants(b []byte) (map[string]map[string]variant, error) {
	var apps map[string]map[string]struct {
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
		Digest     string `yaml:"digest"`
	}
	if err := yaml.Unmarshal(b, &apps); err != nil {
		return nil, err
	}
	out := make(map[string]map[string]variant, len(apps))
	for app, byName := range apps {
		out[app] = make(map[string]variant, len(byName))
		for name, v := range byName {
			out[app][name] = variant{Repository: v.Repository, Tag: v.Tag, Digest: v.Digest}
		}
	}
	return out, nil
}

// variantReason compares two variants.yaml files: it returns how many app
// images moved and, when one did not move the way a release may, the reason to
// refuse. A moved image is a patch or digest change; a minor or major tag, a
// changed repository, an added or removed app or variant is not.
func variantReason(oldApps, newApps map[string]map[string]variant) (int, string) {
	moved := 0
	for _, key := range unionKeys(oldApps, newApps) {
		app, name := key.app, key.name
		oldV, inOld := oldApps[app][name]
		newV, inNew := newApps[app][name]
		switch {
		case !inNew:
			return 0, "variant " + key.String() + " was removed"
		case !inOld:
			return 0, "variant " + key.String() + " was added"
		case oldV.Repository != newV.Repository:
			return 0, "variant " + key.String() + ": the repository changed"
		case oldV.Tag == newV.Tag && oldV.Digest == newV.Digest:
			continue
		case oldV.Tag == newV.Tag:
			moved++ // digest-only
		default:
			switch classify(oldV.Tag, newV.Tag) {
			case bumpPatch:
				moved++
			case bumpMinor:
				return 0, "variant " + key.String() + ": " + oldV.Tag + " -> " + newV.Tag + " is a minor bump"
			case bumpMajor:
				return 0, "variant " + key.String() + ": " + oldV.Tag + " -> " + newV.Tag + " is a major bump"
			default:
				return 0, "variant " + key.String() + ": " + oldV.Tag + " -> " + newV.Tag + " is not a patch or digest bump"
			}
		}
	}
	return moved, ""
}

// parsePins reads charts/s5n/platform.yaml as key -> value. Every value is a
// scalar: a version, or -- for the login -- a whole image reference. The file
// is flat, one key per component, with the `# renovate:` marker inline.
func parsePins(b []byte) (map[string]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(doc))
	for k, v := range doc {
		out[k] = scalar(v)
	}
	return out, nil
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// pinReason compares two platform.yaml files the way variantReason compares
// variants.yaml: how many pins moved, and the reason to refuse when one moved
// past a patch.
func pinReason(oldPins, newPins map[string]string) (int, string) {
	moved := 0
	for _, key := range sortedKeys(oldPins, newPins) {
		oldV, inOld := oldPins[key]
		newV, inNew := newPins[key]
		switch {
		case !inNew:
			return 0, "pin " + key + " was removed"
		case !inOld:
			return 0, "pin " + key + " was added"
		case oldV == newV:
			continue
		}
		switch classify(pinVersion(oldV), pinVersion(newV)) {
		case bumpNone:
			moved++ // the version did not move: only the digest did
		case bumpPatch:
			moved++
		case bumpMinor:
			return 0, "pin " + key + ": " + oldV + " -> " + newV + " is a minor bump"
		case bumpMajor:
			return 0, "pin " + key + ": " + oldV + " -> " + newV + " is a major bump"
		default:
			return 0, "pin " + key + ": " + oldV + " -> " + newV + " is not a patch or digest bump"
		}
	}
	return moved, ""
}

// pinVersion is the version a pin's value names: the image tag when the value
// is a whole image reference (the login), the value itself otherwise. The
// digest is not the version: it moves with the tag.
func pinVersion(value string) string {
	ref := value
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && strings.Contains(ref[:i], "/") {
		return ref[i+1:]
	}
	return ref
}

// bump is how one tag or pin moved between two releases.
type bump int

const (
	// bumpNone is an unchanged value: it names the same version and digest.
	bumpNone bump = iota
	// bumpPatch is a patch-level move: the first two components (major,
	// minor) are unchanged. This is Renovate's own rule for a docker tag
	// (dist/workers/repository/process/lookup/update-type.js): major is the
	// first component, minor the second, and anything else -- a third patch
	// number, a fourth build number, a suffix -- is a patch. So sonarr's
	// 5.0.0.2907 -> 5.0.0.2910 is a patch, which is what lets it automerge.
	bumpPatch
	// bumpMinor is the second component moving; bumpMajor is the first.
	bumpMinor
	bumpMajor
	// bumpOther is a move this program will not call a bump at all: a
	// non-numeric tag, a downgrade, or a tag it cannot parse.
	bumpOther
)

// classify names how newTag moves from oldTag, the way Renovate would.
func classify(oldTag, newTag string) bump {
	if oldTag == newTag {
		return bumpNone
	}
	oldParts, newParts := numericParts(oldTag), numericParts(newTag)
	if len(oldParts) == 0 || len(newParts) == 0 {
		return bumpOther
	}
	if compareParts(newParts, oldParts) <= 0 {
		return bumpOther // not an update: a downgrade or a sideways move
	}
	if section(oldParts, 0) != section(newParts, 0) {
		return bumpMajor
	}
	if section(oldParts, 1) != section(newParts, 1) {
		return bumpMinor
	}
	return bumpPatch
}

// numericParts reads a version's leading dot-separated numbers: 1.2.3 is
// [1 2 3], v1.37.2+k0s.1 is [1 37 2], 5.0.0.2907 is [5 0 0 2907]. A leading "v"
// and any "-suffix" or "+build" metadata are not part of the numbers. A tag
// that names no number at all is not a version this program reasons about.
func numericParts(v string) []int {
	s := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil
	}
	parts := make([]int, 0, 4)
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		parts = append(parts, n)
	}
	return parts
}

func section(parts []int, i int) int {
	if i < len(parts) {
		return parts[i]
	}
	return 0
}

// compareParts orders two component lists, missing components as zero.
func compareParts(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if section(a, i) != section(b, i) {
			return section(a, i) - section(b, i)
		}
	}
	return 0
}

// variantKey names one app variant, for a reason line.
type variantKey struct{ app, name string }

func (k variantKey) String() string { return k.app + "." + k.name }

// display names a variant for a release body: an app's default variant is the
// app, anything else carries its variant name.
func (k variantKey) display() string {
	if k.name == "default" {
		return k.app
	}
	return k.String()
}

// unionKeys is every app variant in either file, sorted, so a reason line is
// stable and an added or removed variant is seen.
func unionKeys(a, b map[string]map[string]variant) []variantKey {
	seen := map[variantKey]bool{}
	var keys []variantKey
	for _, m := range []map[string]map[string]variant{a, b} {
		for app, byName := range m {
			for name := range byName {
				k := variantKey{app, name}
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func sortedKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// movedImages is the app images that moved between two variants.yaml files, one
// line each, for the s5n release's body.
func movedImages(oldVariants, newVariants []byte) ([]string, error) {
	oldApps, err := parseVariants(oldVariants)
	if err != nil {
		return nil, err
	}
	newApps, err := parseVariants(newVariants)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, key := range unionKeys(oldApps, newApps) {
		oldV, inOld := oldApps[key.app][key.name]
		newV, inNew := newApps[key.app][key.name]
		switch {
		case !inOld:
			moved = append(moved, fmt.Sprintf("%s: added (%s:%s)", key.display(), newV.Repository, newV.Tag))
		case !inNew:
			moved = append(moved, fmt.Sprintf("%s: removed", key.display()))
		case oldV == newV:
			continue
		case oldV.Tag == newV.Tag:
			moved = append(moved, fmt.Sprintf("%s: %s digest updated", key.display(), oldV.Tag))
		default:
			moved = append(moved, fmt.Sprintf("%s: %s -> %s", key.display(), oldV.Tag, newV.Tag))
		}
	}
	return moved, nil
}

// allImages is every app image in one variants.yaml, for a first release, when
// there is no previous tag to diff against.
func allImages(apps map[string]map[string]variant) []string {
	var keys []variantKey
	for app, byName := range apps {
		for name := range byName {
			keys = append(keys, variantKey{app, name})
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := apps[k.app][k.name]
		if v.Tag == "" {
			out = append(out, fmt.Sprintf("%s: %s (the chart's appVersion)", k.display(), v.Repository))
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s:%s", k.display(), v.Repository, v.Tag))
	}
	return out
}

// movedPins is the platform pins that moved between two platform.yaml files,
// one line each, for the s5n release's body: a platform bump ships in the chart
// like an image, so the release has to say which pins a user's update brings.
func movedPins(oldPlatform, newPlatform []byte) ([]string, error) {
	oldPins, err := parsePins(oldPlatform)
	if err != nil {
		return nil, err
	}
	newPins, err := parsePins(newPlatform)
	if err != nil {
		return nil, err
	}
	var moved []string
	for _, key := range sortedKeys(oldPins, newPins) {
		oldV, inOld := oldPins[key]
		newV, inNew := newPins[key]
		switch {
		case !inOld:
			moved = append(moved, fmt.Sprintf("%s: added (%s)", key, newV))
		case !inNew:
			moved = append(moved, fmt.Sprintf("%s: removed", key))
		case oldV == newV:
			continue
		case pinVersion(oldV) == pinVersion(newV):
			moved = append(moved, fmt.Sprintf("%s: %s digest updated", key, pinVersion(oldV)))
		default:
			moved = append(moved, fmt.Sprintf("%s: %s -> %s", key, pinVersion(oldV), pinVersion(newV)))
		}
	}
	return moved, nil
}

// allPins is every pin in one platform.yaml, for a first release.
func allPins(pins map[string]string) []string {
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s: %s", k, pinVersion(pins[k])))
	}
	return out
}
