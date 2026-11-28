package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
)

const (
	pinTestDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinTestDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pinTestRegistry answers one tag and counts its asks.
type pinTestRegistry struct {
	answers map[string]string
	err     error
	asked   []string
}

func (f *pinTestRegistry) Resolve(_ context.Context, repository, reference string) (string, error) {
	f.asked = append(f.asked, repository+":"+reference)
	if f.err != nil {
		return "", f.err
	}
	digest, ok := f.answers[repository+":"+reference]
	if !ok {
		kind := "tag"
		if strings.HasPrefix(reference, "sha256:") {
			kind = "digest"
		}
		return "", fmt.Errorf("%s has no %s %s: %w", repository, kind, reference, pin.ErrNotFound)
	}
	return digest, nil
}

// pinTestBox is a values file pinning qbittorrent's tag, and the config root its
// record lives beside.
func pinTestBox(t *testing.T) (*seedbox.Seedbox, string, release.Chart) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.MkdirAll(config, 0o750); err != nil {
		t.Fatal(err)
	}
	grant := filepath.Join(dir, "grant.yaml")
	values := filepath.Join(dir, "values.yaml")
	grantYAML := "namespace: testbox\nstorage:\n  data:\n    path: " + filepath.Join(dir, "data") +
		"\n  config:\n    path: " + config + "\n"
	if err := os.WriteFile(grant, []byte(grantYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(values, []byte("apps:\n  qbittorrent:\n    image:\n      tag: \"5.3-lt2.1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grant, values)
	if err != nil {
		t.Fatal(err)
	}
	chart := release.Chart{Name: "s5n", Version: "0.6.3", Variants: []byte(`qbittorrent:
  default:
    repository: ghcr.io/example/qbittorrent
    tag: "5.3.1-lt2.1"
    digest: ` + pinTestDigestA + `
`)}
	return sb, config, chart
}

func pinTestValues(tag string) map[string]any {
	return map[string]any{"apps": map[string]any{
		"qbittorrent": map[string]any{"enabled": true, "image": map[string]any{"tag": tag}},
	}}
}

func TestBoxPinsRendersTheRecordedDigestWithoutAsking(t *testing.T) {
	sb, config, chart := pinTestBox(t)
	if err := snapshot.SavePins(config, []snapshot.Pin{{
		App: "qbittorrent", Repository: "ghcr.io/example/qbittorrent",
		Tag: "5.3-lt2.1", Digest: pinTestDigestA, Resolved: time.Now().UTC(), Chart: "0.6.3",
	}}); err != nil {
		t.Fatal(err)
	}
	reg := &pinTestRegistry{answers: map[string]string{"ghcr.io/example/qbittorrent:5.3-lt2.1": pinTestDigestB}}
	b := newBoxPins(&say.Plain{W: &bytes.Buffer{}}, reg)

	overlay, err := b.values(context.Background(), sb, chart, pinTestValues("5.3-lt2.1"))
	if err != nil {
		t.Fatalf("boxpins: %v", err)
	}
	image := overlay["apps"].(map[string]any)["qbittorrent"].(map[string]any)["image"].(map[string]any)
	if image["digest"] != pinTestDigestA || image["tag"] != "5.3-lt2.1" {
		t.Fatalf("overlay = %v, want the recorded digest beside the tag", image)
	}
	if len(reg.asked) != 0 {
		t.Fatalf("apply asked the registry about %v, want the record used as it is", reg.asked)
	}
	b.applied()
	pins, err := snapshot.LoadPins(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Digest != pinTestDigestA {
		t.Fatalf("pins = %+v, want the record unchanged", pins)
	}
}

func TestBoxPinsResolvesATagItHasNoRecordFor(t *testing.T) {
	sb, config, chart := pinTestBox(t)
	reg := &pinTestRegistry{answers: map[string]string{"ghcr.io/example/qbittorrent:5.3-lt2.1": pinTestDigestB}}
	b := newBoxPins(&say.Plain{W: &bytes.Buffer{}}, reg)

	overlay, err := b.values(context.Background(), sb, chart, pinTestValues("5.3-lt2.1"))
	if err != nil {
		t.Fatal(err)
	}
	image := overlay["apps"].(map[string]any)["qbittorrent"].(map[string]any)["image"].(map[string]any)
	if image["digest"] != pinTestDigestB {
		t.Fatalf("overlay = %v, want the resolved digest", image)
	}
	// Nothing is recorded until the apply succeeds.
	if pins, err := snapshot.LoadPins(config); err != nil || len(pins) != 0 {
		t.Fatalf("pins before the apply = %+v, %v; want none", pins, err)
	}
	b.applied()
	pins, err := snapshot.LoadPins(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Digest != pinTestDigestB || pins[0].Chart != "0.6.3" {
		t.Fatalf("pins = %+v, want the resolved digest recorded after the apply", pins)
	}
}

func TestBoxPinsRefusesATagItCannotResolve(t *testing.T) {
	sb, _, chart := pinTestBox(t)
	reg := &pinTestRegistry{err: errors.New("dial tcp: connection refused")}
	b := newBoxPins(&say.Plain{W: &bytes.Buffer{}}, reg)

	_, err := b.values(context.Background(), sb, chart, pinTestValues("5.3-lt2.1"))
	if err == nil || !strings.Contains(err.Error(), "qbittorrent") || !strings.Contains(err.Error(), "5.3-lt2.1") {
		t.Fatalf("boxpins = %v, want a refusal naming the app and tag", err)
	}
}
