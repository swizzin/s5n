package pin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/snapshot"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestAVersionTakesItsVariantsTagShape(t *testing.T) {
	for _, c := range []struct {
		variant string
		ref     string
		want    string
	}{
		{"5.3.1-lt2.1", "5.3", "5.3-lt2.1"},
		{"5.3.1-lt2.1", "5.3.1", "5.3.1-lt2.1"},
		{"5.3.1-lt2.1", "5.3.1-lt2.1", "5.3.1-lt2.1"},
		{"5.3.1-lt2.1", "5.2-lt2.1", "5.2-lt2.1"},
		{"5.3.1-lt2.1-10g", "5.3", "5.3-lt2.1-10g"},
		{"5.3.1-lt2.1-10g", "5.3.1-lt2.1-10g", "5.3.1-lt2.1-10g"},
		{"v3.5.2", "3.5", "v3.5"},
		{"v3.5.2", "v3.5", "v3.5"},
		{"2.1.0-stable", "2.1", "2.1-stable"},
		{"2.1.0-stable", "2.1-stable", "2.1-stable"},
		{"5.0.0.3041", "5.0", "5.0"},
		{"5.0.0.3041", "5.0.0.3041", "5.0.0.3041"},
	} {
		t.Run(c.variant+"+"+c.ref, func(t *testing.T) {
			got := ForVariant(ShapeOf(c.variant), Ref{Tag: c.ref})
			if got.Tag != c.want {
				t.Fatalf("ForVariant(%s, %s).Tag = %q, want %q", c.variant, c.ref, got.Tag, c.want)
			}
		})
	}
}

func TestAPinRefIsATagADigestOrBoth(t *testing.T) {
	for _, c := range []struct {
		raw       string
		want      Ref
		errPhrase string
	}{
		{raw: "5.3", want: Ref{Tag: "5.3"}},
		{raw: "5.3-lt2.1", want: Ref{Tag: "5.3-lt2.1"}},
		{raw: digestA, want: Ref{Digest: digestA}},
		{raw: "5.3-lt2.1@" + digestA, want: Ref{Tag: "5.3-lt2.1", Digest: digestA}},
		{raw: "", errPhrase: "empty"},
		{raw: "5.3@" + digestA + "@" + digestB, errPhrase: "more than one"},
		{raw: "sha256:abc", errPhrase: "sha256:<64 hex>"},
		{raw: "5.3@sha256:abc", errPhrase: "sha256:<64 hex>"},
		{raw: "sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", errPhrase: "lowercase"},
	} {
		t.Run(c.raw, func(t *testing.T) {
			got, err := ParseRef(c.raw)
			if c.errPhrase != "" {
				if err == nil || !strings.Contains(err.Error(), c.errPhrase) {
					t.Fatalf("ParseRef(%q) = %+v, %v; want an error naming %q", c.raw, got, err, c.errPhrase)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("ParseRef(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
			}
		})
	}
}

const testVariants = `qbittorrent:
  default:
    repository: ghcr.io/example/qbittorrent
    tag: "5.3.1-lt2.1"
  lt2.1-10g:
    repository: ghcr.io/example/qbittorrent
    tag: "5.3.1-lt2.1-10g"
sonarr:
  default:
    repository: ghcr.io/example/sonarr
    tag: "5.0.0.3041"
    digest: ` + digestB + `
`

func tagPinValues(app, tag, variant string) map[string]any {
	image := map[string]any{"tag": tag}
	if variant != "" {
		image["variant"] = variant
	}
	return map[string]any{"apps": map[string]any{app: map[string]any{"enabled": true, "image": image}}}
}

func TestTaggedPinsAreTheTagOnlyOnes(t *testing.T) {
	values := tagPinValues("qbittorrent", "5.3-lt2.1", "default")
	values["apps"].(map[string]any)["sonarr"] = map[string]any{"enabled": true, "image": map[string]any{"digest": digestA}}
	pins, err := TaggedPins([]byte(testVariants), values, []string{"qbittorrent", "sonarr"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].App != "qbittorrent" {
		t.Fatalf("tagged = %+v, want qbittorrent alone: a digest pin is frozen", pins)
	}
	if pins[0].Repository != "ghcr.io/example/qbittorrent" || pins[0].Tag != "5.3-lt2.1" {
		t.Fatalf("tagged = %+v, want the variant's repository and the values' tag", pins[0])
	}
}

func TestTaggedPinsAreOnlyEnabledApps(t *testing.T) {
	values := tagPinValues("qbittorrent", "5.3-lt2.1", "default")
	pins, err := TaggedPins([]byte(testVariants), values, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 0 {
		t.Fatalf("tagged = %+v, want none: a disabled app renders no image", pins)
	}
}

func TestATagPinForAnAppTheChartDoesNotShipIsRefused(t *testing.T) {
	values := tagPinValues("radarr", "7.1", "")
	_, err := TaggedPins([]byte(testVariants), values, []string{"radarr"}, nil)
	if err == nil || !strings.Contains(err.Error(), "radarr") {
		t.Fatalf("TaggedPins = %v, want an error naming radarr", err)
	}
}

func TestATagPinCarriesTheRecordItHas(t *testing.T) {
	values := tagPinValues("qbittorrent", "5.3-lt2.1", "default")
	records := []snapshot.Pin{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent",
		Tag: "5.3-lt2.1", Digest: digestA,
	}}
	pins, err := TaggedPins([]byte(testVariants), values, []string{"qbittorrent"}, records)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Recorded != digestA {
		t.Fatalf("tagged = %+v, want the recorded digest beside the tag", pins)
	}
}

// fakeRegistry answers by tag, and counts the asks so a test can prove a call
// never reached it.
type fakeRegistry struct {
	answers map[string]string
	err     error
	asked   []string
}

func (f *fakeRegistry) Resolve(_ context.Context, repository, reference string) (string, error) {
	f.asked = append(f.asked, repository+":"+reference)
	if f.err != nil {
		return "", f.err
	}
	digest, ok := f.answers[repository+":"+reference]
	if !ok {
		return "", ErrNotFound
	}
	return digest, nil
}

func (f *fakeRegistry) askedAbout() string { return strings.Join(f.asked, " ") }

func TestResolveMovesAPinWhoseTagWasRebuilt(t *testing.T) {
	reg := &fakeRegistry{answers: map[string]string{"ghcr.io/example/qbittorrent:5.3-lt2.1": digestB}}
	answers, err := Resolve(context.Background(), reg, []Tagged{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1", Recorded: digestA,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Digest != digestB || !answers[0].Moved() {
		t.Fatalf("answers = %+v, want the new digest and a move", answers)
	}
}

func TestResolveKeepsTheRecordWhenTheRegistryIsDown(t *testing.T) {
	reg := &fakeRegistry{err: errors.New("dial tcp: connection refused")}
	answers, err := Resolve(context.Background(), reg, []Tagged{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1", Recorded: digestA,
	}})
	if err != nil {
		t.Fatalf("Resolve = %v, want the recorded digest to carry the pin", err)
	}
	if len(answers) != 1 || answers[0].Digest != digestA || answers[0].Resolved || answers[0].Moved() {
		t.Fatalf("answers = %+v, want the record and an unchecked answer", answers)
	}
	if answers[0].Err == nil {
		t.Fatal("the answer does not say why the pin was not checked")
	}
}

func TestResolveRefusesATagWithNothingToFallBackTo(t *testing.T) {
	reg := &fakeRegistry{err: errors.New("dial tcp: connection refused")}
	_, err := Resolve(context.Background(), reg, []Tagged{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1",
	}})
	if err == nil || !strings.Contains(err.Error(), "qbittorrent") || !strings.Contains(err.Error(), "5.3-lt2.1") {
		t.Fatalf("Resolve = %v, want an error naming the app and tag", err)
	}
}

func TestRecordedDoesNotAskAboutAPinItHas(t *testing.T) {
	reg := &fakeRegistry{answers: map[string]string{"ghcr.io/example/qbittorrent:5.3-lt2.1": digestB}}
	answers, err := Recorded(context.Background(), reg, []Tagged{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1", Recorded: digestA,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Digest != digestA || answers[0].Moved() {
		t.Fatalf("answers = %+v, want the record, not the registry", answers)
	}
	if got := reg.askedAbout(); got != "" {
		t.Fatalf("apply asked the registry %q for a pin it had recorded", got)
	}
}

func TestRecordedResolvesATagItHasNoRecordFor(t *testing.T) {
	reg := &fakeRegistry{answers: map[string]string{"ghcr.io/example/qbittorrent:5.3-lt2.1": digestB}}
	answers, err := Recorded(context.Background(), reg, []Tagged{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Digest != digestB || !answers[0].Resolved {
		t.Fatalf("answers = %+v, want the registry's digest", answers)
	}
	if got := reg.askedAbout(); got != "ghcr.io/example/qbittorrent:5.3-lt2.1" {
		t.Fatalf("asked %q, want the one tag with no record", got)
	}
}

func TestTheOverlayKeepsTheTagAndCarriesTheDigest(t *testing.T) {
	overlay := Overlay([]Answer{{
		Tagged: Tagged{App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1", Recorded: digestA},
		Digest: digestA, Resolved: true,
	}})
	apps, _ := overlay["apps"].(map[string]any)
	qbt, _ := apps["qbittorrent"].(map[string]any)
	image, _ := qbt["image"].(map[string]any)
	if image["tag"] != "5.3-lt2.1" || image["digest"] != digestA {
		t.Fatalf("overlay = %#v, want the tag kept with the digest beside it", overlay)
	}
}

func TestDigestsAndRecordsAreTheResolvedAnswers(t *testing.T) {
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	answers := []Answer{
		{Tagged: Tagged{App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent", Tag: "5.3-lt2.1", Recorded: digestA}, Digest: digestB, Resolved: true},
		{Tagged: Tagged{App: "sonarr", Repository: "ghcr.io/example/sonarr", Tag: "5.0", Recorded: digestA}, Digest: digestA, Err: errors.New("registry down")},
	}
	digests := Digests(answers)
	if len(digests) != 2 || digests["qbittorrent"] != digestB || digests["sonarr"] != digestA {
		t.Fatalf("digests = %+v, want both answers' rendered digests", digests)
	}
	records := Records(answers, "0.6.3", at)
	if len(records) != 1 || records[0].App != "qbittorrent" || records[0].Digest != digestB || records[0].Chart != "0.6.3" || !records[0].Resolved.Equal(at) {
		t.Fatalf("records = %+v, want qbittorrent's answer alone: checked is not resolved", records)
	}
}
