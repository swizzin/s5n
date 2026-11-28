package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

const pinDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// TestAStepsDigestsReachTheRelease: update resolved every tag pin before the
// plan, so its apply renders repo:tag@digest without asking a registry again.
func TestAStepsDigestsReachTheRelease(t *testing.T) {
	f := newFakes(t)
	f.writeValues(t, "apps:\n  sonarr:\n    image:\n      tag: \"5.0\"\n")
	f.pinDigests = map[string]string{"sonarr": pinDigest}
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.rel.upgraded) != 1 {
		t.Fatalf("upgrades = %d, want one", len(f.rel.upgraded))
	}
	apps, _ := f.rel.upgraded[0]["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	image, _ := sonarr["image"].(map[string]any)
	if image["digest"] != pinDigest {
		t.Fatalf("the release rendered %v, want the step's digest on sonarr", image)
	}
	if image["tag"] != "5.0" {
		t.Fatalf("the release rendered %v, want the pinned tag kept", image)
	}
}

// TestAPinsOverlayIsTheFallback: a bare apply has no plan to carry digests, so it
// asks its own resolver.
func TestAPinsOverlayIsTheFallback(t *testing.T) {
	f := newFakes(t)
	f.writeValues(t, "apps:\n  sonarr:\n    image:\n      tag: \"5.0\"\n")
	f.pins = func(_ context.Context, _ *seedbox.Seedbox, _ Chart, values map[string]any) (map[string]any, error) {
		if _, ok := values["apps"].(map[string]any)["sonarr"]; !ok {
			t.Errorf("the overlay was asked before the values named the app: %v", values)
		}
		return map[string]any{"apps": map[string]any{"sonarr": map[string]any{
			"image": map[string]any{"tag": "5.0", "digest": pinDigest},
		}}}, nil
	}
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	apps, _ := f.rel.upgraded[0]["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	image, _ := sonarr["image"].(map[string]any)
	if image["digest"] != pinDigest {
		t.Fatalf("the release rendered %v, want the overlay's digest", image)
	}
}

// TestAnEmptyPinSetAsksNobody: update resolved and found nothing pinned; the
// resolver it would have used is not called.
func TestAnEmptyPinSetAsksNobody(t *testing.T) {
	f := newFakes(t)
	f.pinDigests = map[string]string{}
	f.pins = func(context.Context, *seedbox.Seedbox, Chart, map[string]any) (map[string]any, error) {
		return nil, errors.New("the resolver was asked with nothing pinned")
	}
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
}

// TestATagPinTheResolverRefusesStopsBeforeTheCluster: the refusal happens while
// the values are checked, so no client opens and nothing is rendered.
func TestATagPinTheResolverRefusesStopsBeforeTheCluster(t *testing.T) {
	f := newFakes(t)
	f.pins = func(context.Context, *seedbox.Seedbox, Chart, map[string]any) (map[string]any, error) {
		return nil, errors.New("qbittorrent: the pin 5.3-lt2.1 could not be resolved")
	}
	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "could not be resolved") {
		t.Fatalf("apply = %v, want the resolver's refusal", err)
	}
	if f.openCalls != 0 || f.storeOpens != 0 {
		t.Fatalf("a refusal opened %d release clients and %d storage clients, want none", f.openCalls, f.storeOpens)
	}
}
