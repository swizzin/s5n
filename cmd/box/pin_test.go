package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/snapshot"
)

const pinQbtRepo = "ghcr.io/example/qbittorrent"

// pinFixture is a values file, a config root and a chart whose qBittorrent
// variants have the two tag shapes.
type pinFixture struct {
	dir      string
	grant    string
	values   string
	config   string
	chart    release.Chart
	resolver *pinTestRegistry
	at       time.Time
}

const pinFixtureValues = `# alice's box: everything here is hers to tune.
global:
  tz: "Europe/Berlin"

apps:
  qbittorrent:
    image:
      variant: lt2.1-10g
      tag: "5.3.1-lt2.1-10g"
    env:
      TZ: "Europe/Berlin"

  sonarr:
    env:
      SONARR__LOG__LEVEL: debug

wiring:
  enabled: true
`

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	dir := t.TempDir()
	f := &pinFixture{dir: dir, grant: filepath.Join(dir, "grant.yaml"), values: filepath.Join(dir, "values.yaml"), config: filepath.Join(dir, "config")}
	if err := os.MkdirAll(f.config, 0o750); err != nil {
		t.Fatal(err)
	}
	grantYAML := "namespace: testbox\nstorage:\n  data:\n    path: " + filepath.Join(dir, "data") +
		"\n  config:\n    path: " + f.config + "\n"
	if err := os.WriteFile(f.grant, []byte(grantYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.values, []byte(pinFixtureValues), 0o600); err != nil {
		t.Fatal(err)
	}
	f.chart = release.Chart{Name: "s5n", Version: "0.6.3", Variants: []byte(`qbittorrent:
  default:
    repository: ` + pinQbtRepo + `
    tag: "5.3.1-lt2.1"
  lt2.1-10g:
    repository: ` + pinQbtRepo + `
    tag: "5.3.1-lt2.1-10g"
sonarr:
  default:
    repository: ghcr.io/example/sonarr
    tag: "5.0.0.3041"
`)}
	f.resolver = &pinTestRegistry{answers: map[string]string{
		pinQbtRepo + ":5.3-lt2.1-10g":     pinTestDigestB,
		pinQbtRepo + ":5.3.1-lt2.1-10g":   pinTestDigestA,
		pinQbtRepo + ":5.4-lt2.1-10g":     pinTestDigestB,
		pinQbtRepo + ":" + pinTestDigestA: pinTestDigestA,
		pinQbtRepo + ":" + pinTestDigestB: pinTestDigestB,
	}}
	f.at = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	return f
}

func (f *pinFixture) files(out *bytes.Buffer) *pinFiles {
	return &pinFiles{out: &say.Plain{W: out}, resolver: f.resolver, now: func() time.Time { return f.at }}
}

func (f *pinFixture) decide(t *testing.T, req pinRequest) (pinOutcome, string) {
	t.Helper()
	out := &bytes.Buffer{}
	got, err := f.files(out).decide(context.Background(), req, f.chart)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	body, err := os.ReadFile(f.values)
	if err != nil {
		t.Fatal(err)
	}
	return got, string(body)
}

func (f *pinFixture) request(app, ref string) pinRequest {
	return pinRequest{GrantPath: f.grant, ValuesPath: f.values, App: app, Ref: ref}
}

func TestPinWritesTheTagInTheVariantsShape(t *testing.T) {
	f := newPinFixture(t)
	got, body := f.decide(t, f.request("qbittorrent", "5.4"))
	if got.Tag != "5.4-lt2.1-10g" || got.Digest != "" || !got.Changed || got.Frozen {
		t.Fatalf("outcome = %+v, want the tag in the selected variant's shape", got)
	}
	if !strings.Contains(body, `tag: "5.4-lt2.1-10g"`) {
		t.Fatalf("the values do not carry the tag:\n%s", body)
	}
	if !strings.Contains(body, "      variant: lt2.1-10g") {
		t.Fatalf("the pin changed the variant:\n%s", body)
	}
	for _, kept := range []string{"# alice's box: everything here is hers to tune.", `tz: "Europe/Berlin"`, "      SONARR__LOG__LEVEL: debug", "wiring:"} {
		if !strings.Contains(body, kept) {
			t.Fatalf("the pin lost %q:\n%s", kept, body)
		}
	}
	pins, err := snapshot.LoadPins(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Tag != "5.4-lt2.1-10g" || pins[0].Digest != pinTestDigestB || pins[0].Chart != "0.6.3" || !pins[0].Resolved.Equal(f.at) {
		t.Fatalf("pins = %+v, want qbittorrent's tag resolved and recorded", pins)
	}
}

func TestPinWithATagTheRegistryDoesNotHaveWritesNothing(t *testing.T) {
	f := newPinFixture(t)
	before, err := os.ReadFile(f.values)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.files(&bytes.Buffer{}).decide(context.Background(), f.request("qbittorrent", "5.2"), f.chart)
	if err == nil || !strings.Contains(err.Error(), pinQbtRepo) || !strings.Contains(err.Error(), "5.2-lt2.1-10g") {
		t.Fatalf("pin = %v, want a refusal naming the repository and tag", err)
	}
	after, err := os.ReadFile(f.values)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a refused pin changed the values:\n%s", after)
	}
	if pins, err := snapshot.LoadPins(f.config); err != nil || len(pins) != 0 {
		t.Fatalf("a refused pin wrote a record: %+v, %v", pins, err)
	}
}

func TestPinADigestFreezesItAndDropsTheTag(t *testing.T) {
	f := newPinFixture(t)
	got, body := f.decide(t, f.request("qbittorrent", pinTestDigestB))
	if !got.Frozen || got.Digest != pinTestDigestB || got.Tag != "" {
		t.Fatalf("outcome = %+v, want a frozen digest", got)
	}
	if !strings.Contains(body, "digest: "+pinTestDigestB) || strings.Contains(body, "tag:") {
		t.Fatalf("the values are not one digest pin:\n%s", body)
	}
}

func TestPinATagAndDigestThatDisagreeIsRefused(t *testing.T) {
	f := newPinFixture(t)
	_, err := f.files(&bytes.Buffer{}).decide(context.Background(), f.request("qbittorrent", "5.3-lt2.1-10g@"+pinTestDigestA), f.chart)
	if err == nil || !strings.Contains(err.Error(), "not") {
		t.Fatalf("pin = %v, want a refusal naming the tag's digest", err)
	}
}

func TestPinADigestTheRegistryDoesNotHaveIsRefused(t *testing.T) {
	f := newPinFixture(t)
	absent := "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	_, err := f.files(&bytes.Buffer{}).decide(context.Background(), f.request("qbittorrent", absent), f.chart)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("pin = %v, want a refusal naming the digest", err)
	}
}

func TestPinAnAppTheChartDoesNotShipIsRefused(t *testing.T) {
	f := newPinFixture(t)
	_, err := f.files(&bytes.Buffer{}).decide(context.Background(), f.request("radarr", "7.1"), f.chart)
	if err == nil || !strings.Contains(err.Error(), "radarr") {
		t.Fatalf("pin = %v, want a refusal naming radarr", err)
	}
}

func TestPinDropsARollbackHold(t *testing.T) {
	f := newPinFixture(t)
	if err := snapshot.SaveHolds(f.config, []snapshot.Hold{{
		App:      "qbittorrent",
		At:       snapshot.ImageRef{Repository: pinQbtRepo, Tag: "5.2-lt2.1-10g", Digest: pinTestDigestA},
		Avoiding: snapshot.ImageRef{Repository: pinQbtRepo, Tag: "5.3.1-lt2.1-10g", Digest: pinTestDigestB},
		Snapshot: "0.6.3-0.6.3-20261127T093000Z", Since: f.at, By: snapshot.ByUpdate,
	}, holdFor("sonarr")}); err != nil {
		t.Fatal(err)
	}
	if _, _ = f.decide(t, f.request("qbittorrent", "5.4")); true {
		holds, err := snapshot.LoadHolds(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if len(holds) != 1 || holds[0].App != "sonarr" {
			t.Fatalf("holds = %+v, want qbittorrent's gone and sonarr's kept", holds)
		}
	}
}

func TestUnpinRemovesBothHalvesAndTheRecord(t *testing.T) {
	f := newPinFixture(t)
	if _, body := f.decide(t, f.request("qbittorrent", "5.4")); !strings.Contains(body, "5.4-lt2.1-10g") {
		t.Fatalf("precondition: the pin was not written:\n%s", body)
	}
	got, body := f.decide(t, pinRequest{GrantPath: f.grant, ValuesPath: f.values, App: "qbittorrent", Unpin: true})
	if !got.Changed || got.Tag != "" || got.Digest != "" {
		t.Fatalf("outcome = %+v, want the app following the chart again", got)
	}
	if strings.Contains(body, "tag:") || strings.Contains(body, "digest:") {
		t.Fatalf("unpin left a pin behind:\n%s", body)
	}
	if !strings.Contains(body, "variant: lt2.1-10g") {
		t.Fatalf("unpin took the variant:\n%s", body)
	}
	for _, kept := range []string{"# alice's box: everything here is hers to tune.", "      SONARR__LOG__LEVEL: debug", "wiring:"} {
		if !strings.Contains(body, kept) {
			t.Fatalf("unpin lost %q:\n%s", kept, body)
		}
	}
	if pins, err := snapshot.LoadPins(f.config); err != nil || len(pins) != 0 {
		t.Fatalf("pins after unpin = %+v, %v; want none", pins, err)
	}
}

// TestUnpinRemovesAnImageKeyThatHeldOnlyThePin: a pin on an app with no other
// tuning leaves nothing behind, and box cannot leave `image:` empty.
func TestUnpinRemovesAnImageKeyThatHeldOnlyThePin(t *testing.T) {
	f := newPinFixture(t)
	if err := os.WriteFile(f.values, []byte("apps:\n  sonarr:\n    image:\n      tag: \"5.0\"\nwiring:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, body := f.decide(t, pinRequest{GrantPath: f.grant, ValuesPath: f.values, App: "sonarr", Unpin: true})
	if !got.Changed {
		t.Fatalf("outcome = %+v, want a change", got)
	}
	if strings.Contains(body, "image:") || strings.Contains(body, "sonarr") {
		t.Fatalf("unpin left an empty app or image behind:\n%s", body)
	}
	if !strings.Contains(body, "wiring:") {
		t.Fatalf("unpin took the rest of the file:\n%s", body)
	}
}

func TestUnpinAnAppThatIsNotPinnedIsNotAChange(t *testing.T) {
	f := newPinFixture(t)
	before, err := os.ReadFile(f.values)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.files(&bytes.Buffer{}).decide(context.Background(), pinRequest{
		GrantPath: f.grant, ValuesPath: f.values, App: "sonarr", Unpin: true,
	}, f.chart)
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed {
		t.Fatalf("outcome = %+v, want no change", got)
	}
	after, err := os.ReadFile(f.values)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("an unpin of nothing changed the values:\n%s", after)
	}
}
