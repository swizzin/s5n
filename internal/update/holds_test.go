package update

import (
	"reflect"
	"testing"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/snapshot"
)

// holdValuesChart pins sonarr at digestB.
func holdValuesChart() release.Chart {
	return release.Chart{Name: "s5n", Version: "0.5.0", Variants: variants(digestB)}
}

func heldSonarr() snapshot.Hold {
	return snapshot.Hold{
		App:      "sonarr",
		At:       snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0", Digest: digestA},
		Avoiding: snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0", Digest: digestB},
		Snapshot: "0.4.0-0.5.0-20260102T030405Z",
		By:       snapshot.ByUpdate,
	}
}

func TestHoldValuesKeepsTheAppOnItsOldImage(t *testing.T) {
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}}
	overlay, lapsed, err := HoldValues(holdValuesChart(), values, []snapshot.Hold{heldSonarr()})
	if err != nil {
		t.Fatal(err)
	}
	if len(lapsed) != 0 {
		t.Fatalf("lapsed = %+v, want none: the release still pins the avoided image", lapsed)
	}
	want := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"image": map[string]any{
		"digest": digestA,
		"tag":    "4.9.0",
	}}}}
	if !reflect.DeepEqual(overlay, want) {
		t.Fatalf("overlay = %#v, want %#v", overlay, want)
	}
}

func TestHoldValuesLosesToAUserPin(t *testing.T) {
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{
		"enabled": true,
		"image":   map[string]any{"digest": digestC},
	}}}
	overlay, lapsed, err := HoldValues(holdValuesChart(), values, []snapshot.Hold{heldSonarr()})
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 0 || len(lapsed) != 0 {
		t.Fatalf("overlay = %#v lapsed = %+v, want neither: the user's pin wins", overlay, lapsed)
	}
}

func TestHoldValuesLapsesWhenTheReleaseMovesPastIt(t *testing.T) {
	hold := heldSonarr()
	hold.Avoiding = snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.5", Digest: digestC}
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}}
	overlay, lapsed, err := HoldValues(holdValuesChart(), values, []snapshot.Hold{hold})
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 0 {
		t.Fatalf("overlay = %#v, want none: the hold lapsed", overlay)
	}
	if len(lapsed) != 1 || lapsed[0].Hold.App != "sonarr" {
		t.Fatalf("lapsed = %+v, want sonarr", lapsed)
	}
	const want = "sonarr moves with the release again: s5n 0.5.0 pins 5.0.0, not 4.9.5"
	if lapsed[0].Text != want {
		t.Fatalf("lapse text = %q, want %q", lapsed[0].Text, want)
	}
}

func TestHoldValuesLapsesWhenTheReleasePinsAnotherTag(t *testing.T) {
	hold := heldSonarr()
	hold.Avoiding = snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0"}
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}}
	chart := release.Chart{Name: "s5n", Version: "0.5.0", Variants: variantsTagOnly("5.0.1")}
	overlay, lapsed, err := HoldValues(chart, values, []snapshot.Hold{hold})
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 0 {
		t.Fatalf("overlay = %#v, want none: the release pins another tag", overlay)
	}
	if len(lapsed) != 1 || lapsed[0].Hold.App != "sonarr" {
		t.Fatalf("lapsed = %+v, want sonarr", lapsed)
	}
	const want = "sonarr moves with the release again: s5n 0.5.0 pins 5.0.1, not 5.0.0"
	if lapsed[0].Text != want {
		t.Fatalf("lapse text = %q, want %q", lapsed[0].Text, want)
	}
}

func TestHoldValuesKeepsAReleaseThatPinsTheSameTagOnlyImage(t *testing.T) {
	hold := heldSonarr()
	hold.Avoiding = snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "5.0.0"}
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}}
	chart := release.Chart{Name: "s5n", Version: "0.5.0", Variants: variantsTagOnly("5.0.0")}
	overlay, lapsed, err := HoldValues(chart, values, []snapshot.Hold{hold})
	if err != nil {
		t.Fatal(err)
	}
	if len(lapsed) != 0 || len(overlay) != 1 {
		t.Fatalf("overlay = %#v lapsed = %+v, want the hold kept", overlay, lapsed)
	}
}

func TestHoldValuesStandsWhenTheChartSaysNothing(t *testing.T) {
	values := map[string]any{"apps": map[string]any{"sonarr": map[string]any{"enabled": true}}}
	overlay, lapsed, err := HoldValues(release.Chart{Name: "s5n", Version: "0.5.0"}, values, []snapshot.Hold{heldSonarr()})
	if err != nil {
		t.Fatal(err)
	}
	if len(lapsed) != 0 || len(overlay) != 1 {
		t.Fatalf("overlay = %#v lapsed = %+v, want the hold kept", overlay, lapsed)
	}
}
