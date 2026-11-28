// Package pin is what a tag pin follows: the ref a user writes, the tag it
// becomes in the selected variant's shape, and the digest a registry answered for
// it. The digest is recorded beside the snapshots (internal/snapshot), never in
// values.
package pin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ErrNotFound is a registry saying the reference is not in that repository: a
// tag its publisher never built, or a digest it has pruned.
var ErrNotFound = errors.New("not found")

// Shape is a variant's tag around its version: the prefix (seerr's "v", usually
// none) and the suffix ("-lt2.1", "-10g", "-stable"), so a user's version is
// written the way the publisher tags that build.
type Shape struct {
	Prefix string
	Suffix string
}

// ShapeOf splits a variant's chart tag into its prefix, leading version and
// suffix. A tag with no leading version wraps nothing: the ref is used as it is.
func ShapeOf(tag string) Shape {
	tag = strings.TrimSpace(tag)
	prefix := ""
	if strings.HasPrefix(tag, "v") && len(tag) > 1 && isDigit(tag[1]) {
		prefix, tag = "v", tag[1:]
	}
	n := leadingVersion(tag)
	if n == 0 {
		return Shape{Prefix: prefix}
	}
	return Shape{Prefix: prefix, Suffix: tag[n:]}
}

// Tag wraps a bare version in the shape, and takes any other ref as given: a ref
// that already carries the suffix is a full tag.
func (s Shape) Tag(ref string) string {
	body := ref
	if s.Prefix != "" {
		body = strings.TrimPrefix(body, s.Prefix)
	}
	if leadingVersion(body) == len(body) && len(body) > 0 {
		return s.Prefix + body + s.Suffix
	}
	return s.Prefix + body
}

// Version is the version part of a tag in this shape: what a chart's tag is
// compared with to say the chart has moved past the pin.
func (s Shape) Version(tag string) string {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), s.Prefix)
	n := leadingVersion(tag)
	if n == 0 {
		return ""
	}
	return s.Prefix + tag[:n]
}

// Ref is one pin as the user wrote it: a tag to follow, a digest to freeze, or
// both.
type Ref struct {
	Tag    string
	Digest string
}

// ParseRef reads one ref: a version (5.3), a full tag (5.3-lt2.1), a digest
// (sha256:<64 hex>), or tag@digest.
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, errors.New("empty ref: want a version (5.3), a tag (5.3-lt2.1), a digest, or tag@digest")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return Ref{}, fmt.Errorf("ref %q has whitespace", raw)
	}
	tag, digest, both := strings.Cut(raw, "@")
	switch {
	case both && strings.Contains(digest, "@"):
		return Ref{}, fmt.Errorf("ref %q has more than one @", raw)
	case both && tag == "":
		return Ref{}, fmt.Errorf("ref %q names no tag before its @", raw)
	case both:
		if err := snapshot.CheckDigest(digest); err != nil {
			return Ref{}, fmt.Errorf("ref %q: digest %q %w", raw, digest, err)
		}
		return Ref{Tag: tag, Digest: digest}, nil
	case strings.HasPrefix(tag, "sha256:"):
		if err := snapshot.CheckDigest(tag); err != nil {
			return Ref{}, fmt.Errorf("digest %q %w", tag, err)
		}
		return Ref{Digest: tag}, nil
	}
	return Ref{Tag: tag}, nil
}

// ForVariant is the ref as the chart renders it: a bare version takes the
// variant's tag shape, and a digest stays a digest.
func ForVariant(shape Shape, ref Ref) Ref {
	out := ref
	if ref.Tag != "" {
		out.Tag = shape.Tag(ref.Tag)
	}
	return out
}

// Tagged is one app the values hold on a tag: the repository the tag lives in and
// the digest box last recorded for it, empty when it has none.
type Tagged struct {
	App        string
	Repository string
	Tag        string
	Recorded   string
}

// TaggedPins reads the values' tag-only pins for the given app keys: a pin that
// names a digest is frozen, and a disabled app renders no image. A pin on an app
// the chart does not ship, or on a variant it does not have, is refused here rather
// than rendering an image box never saw.
func TaggedPins(variants []byte, values map[string]any, apps []string, records []snapshot.Pin) ([]Tagged, error) {
	images, err := admission.Images(variants)
	if err != nil {
		return nil, err
	}
	out := make([]Tagged, 0, len(apps))
	for _, app := range apps {
		tag, digest := valuesPin(values, app)
		if tag == "" || digest != "" {
			continue
		}
		variantsForApp, ok := images[app]
		if !ok {
			return nil, fmt.Errorf("apps.%s.image.tag: this chart ships no image for %s", app, app)
		}
		variant := variantOf(values, app)
		image, ok := variantsForApp[variant]
		if !ok {
			return nil, fmt.Errorf("apps.%s.image.variant: %q is not a blessed variant of %s", app, variant, app)
		}
		p := Tagged{App: app, Repository: image.Repository, Tag: tag}
		if record, ok := snapshot.PinFor(records, app); ok && record.Repository == image.Repository && record.Tag == tag {
			p.Recorded = record.Digest
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

// Resolver answers what a tag or digest names in one repository now: the digest of
// the manifest, which for a multi-arch tag is the index.
type Resolver interface {
	Resolve(ctx context.Context, repository, reference string) (digest string, err error)
}

// Answer is one tag pin after box asked the registry.
type Answer struct {
	Tagged
	// Digest is the digest this run renders.
	Digest string
	// Resolved says the registry answered it; false means Err explains why the
	// recorded digest is the one that runs.
	Resolved bool
	// Err is the registry's answer when the pin could not be checked.
	Err error
}

// Moved reports that this answer renders other bytes than the record's: a rebuilt
// tag is an image move like any other.
func (a Answer) Moved() bool {
	return a.Recorded != "" && a.Digest != "" && a.Digest != a.Recorded
}

// Resolve asks the registry about every tag pin: update's answer, where a rebuilt
// tag is a move. A failed ask keeps the recorded digest and says why; with no
// record to fall back to it is an error, because box renders no tag without a
// digest.
func Resolve(ctx context.Context, r Resolver, pins []Tagged) ([]Answer, error) {
	return resolve(ctx, r, pins, func(Tagged) bool { return true })
}

// Recorded answers a pin from the record it has and asks the registry only for a
// tag with none: apply's answer, which never moves a pin the box already recorded.
// Moving one is update's job.
func Recorded(ctx context.Context, r Resolver, pins []Tagged) ([]Answer, error) {
	return resolve(ctx, r, pins, func(p Tagged) bool { return p.Recorded == "" })
}

// resolve is both answers: ask decides which pins reach the registry.
func resolve(ctx context.Context, r Resolver, pins []Tagged, ask func(Tagged) bool) ([]Answer, error) {
	if len(pins) > 0 && r == nil {
		return nil, fmt.Errorf("%s is pinned to a tag and this box cannot ask a registry: pass a tag with its digest", strings.Join(appNames(pins), ", "))
	}
	out := make([]Answer, 0, len(pins))
	for _, p := range pins {
		a := Answer{Tagged: p, Digest: p.Recorded}
		if ask(p) {
			digest, err := r.Resolve(ctx, p.Repository, p.Tag)
			switch {
			case err != nil && p.Recorded == "":
				return nil, fmt.Errorf("%s: the pin %s could not be resolved and box has no digest recorded for it: %w", p.App, p.Tag, err)
			case err != nil:
				a.Err = err
			default:
				a.Digest, a.Resolved = digest, true
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// Overlay is the values fragment that renders each tag pin as repo:tag@digest. The
// tag stays because it is the user's intent; the digest is what this run resolved.
func Overlay(answers []Answer) map[string]any {
	apps := make(map[string]any, len(answers))
	for _, a := range answers {
		if a.Digest == "" {
			continue
		}
		apps[a.App] = map[string]any{"image": map[string]any{"tag": a.Tag, "digest": a.Digest}}
	}
	if len(apps) == 0 {
		return nil
	}
	return map[string]any{"apps": apps}
}

// Digests is each app's rendered digest, for the apply a step runs.
func Digests(answers []Answer) map[string]string {
	out := make(map[string]string, len(answers))
	for _, a := range answers {
		if a.Digest != "" {
			out[a.App] = a.Digest
		}
	}
	return out
}

// Records is the file to write after a successful apply: one record per answer
// the registry gave, so an unchecked pin keeps the time it was last really
// checked.
func Records(answers []Answer, chartVersion string, at time.Time) []snapshot.Pin {
	out := make([]snapshot.Pin, 0, len(answers))
	for _, a := range answers {
		if !a.Resolved {
			continue
		}
		out = append(out, snapshot.Pin{
			App:        a.App,
			Repository: a.Repository,
			Tag:        a.Tag,
			Digest:     a.Digest,
			Resolved:   at,
			Chart:      chartVersion,
		})
	}
	return out
}

// Warnings is one line per pin the registry did not answer, for the caller to say
// before the plan.
func Warnings(answers []Answer) []string {
	var out []string
	for _, a := range answers {
		if a.Err == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s: pinned to %s, which could not be checked, so box keeps %s", a.App, a.Tag, shortDigest(a.Digest)))
	}
	return out
}

// shortDigest is the first eight hex characters, the handle a person reads.
func shortDigest(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 8 {
		return "sha256:" + hex[:8] + "…"
	}
	return digest
}

func appNames(pins []Tagged) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, p.App)
	}
	return out
}

// valuesPin is the values' pin on one app: digest, else tag.
func valuesPin(values map[string]any, app string) (tag, digest string) {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	digest, _ = image["digest"].(string)
	tag, _ = image["tag"].(string)
	return strings.TrimSpace(tag), strings.TrimSpace(digest)
}

// variantOf is the values' selected variant, defaulting to "default".
func variantOf(values map[string]any, app string) string {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	variant, _ := image["variant"].(string)
	variant = strings.TrimSpace(variant)
	if variant == "" {
		return "default"
	}
	return variant
}

// leadingVersion is the byte count of the version at the start of s:
// [0-9]+(\.[0-9]+)*, or 0 when s does not begin with one.
func leadingVersion(s string) int {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	if n == 0 {
		return 0
	}
	for n < len(s) && s[n] == '.' {
		next := n + 1
		for next < len(s) && isDigit(s[next]) {
			next++
		}
		if next == n+1 {
			break
		}
		n = next
	}
	return n
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
