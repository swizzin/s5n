package update

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

func TestPlanSteps(t *testing.T) {
	rel := func(version, minBox, upgradeFrom string) release.Published {
		return release.Published{Version: version, MinBox: minBox, UpgradeFrom: upgradeFrom}
	}
	cases := []struct {
		name      string
		installed string
		target    string
		box       string
		releases  []release.Published
		want      []Step
		wantErr   string
	}{
		{
			name:      "one step when the target may follow the installed release",
			installed: "0.4.0",
			target:    "0.5.0",
			box:       "0.4.0",
			releases:  []release.Published{rel("0.4.0", "0.4.0", "0.4.0"), rel("0.5.0", "0.4.0", "0.4.0")},
			want:      []Step{{From: "0.4.0", To: "0.5.0"}},
		},
		{
			name:      "two steps through the release the target may follow",
			installed: "0.4.0",
			target:    "0.6.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.5.0", "0.4.0", "0.4.0"),
				rel("0.6.0", "0.4.0", "0.5.0"),
			},
			want: []Step{{From: "0.4.0", To: "0.5.0"}, {From: "0.5.0", To: "0.6.0"}},
		},
		{
			name:      "the newest reachable intermediate wins",
			installed: "0.4.0",
			target:    "0.7.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.5.0", "0.4.0", "0.4.0"),
				rel("0.6.0", "0.4.0", "0.4.0"),
				rel("0.7.0", "0.4.0", "0.6.0"),
			},
			want: []Step{{From: "0.4.0", To: "0.6.0"}, {From: "0.6.0", To: "0.7.0"}},
		},
		{
			name:      "a gap is an error naming where the path stops",
			installed: "0.4.0",
			target:    "0.6.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.6.0", "0.4.0", "0.5.0"),
			},
			wantErr: "no published release can be upgraded from 0.4.0",
		},
		{
			name:      "an intermediate this box is too old for is not on the path",
			installed: "0.4.0",
			target:    "0.6.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.5.0", "9.0.0", "0.4.0"),
				rel("0.6.0", "0.4.0", "0.5.0"),
			},
			wantErr: "no published release can be upgraded from 0.4.0",
		},
		{
			name:      "a malformed floor is an error naming the release",
			installed: "0.4.0",
			target:    "0.5.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.5.0", "0.4.0", "soon"),
			},
			wantErr: "release 0.5.0: annotation " + release.UpgradeFromAnnotation,
		},
		{
			name:      "a target at or below the installed release is no steps",
			installed: "0.5.0",
			target:    "0.5.0",
			box:       "0.4.0",
			releases:  []release.Published{rel("0.5.0", "0.4.0", "0.4.0")},
			want:      nil,
		},
		{
			name:      "prereleases are not steps",
			installed: "0.4.0",
			target:    "0.7.0",
			box:       "0.4.0",
			releases: []release.Published{
				rel("0.4.0", "0.4.0", "0.4.0"),
				rel("0.6.0-rc.1", "0.4.0", "0.4.0"),
				rel("0.7.0", "0.4.0", "0.6.0-rc.1"),
			},
			// 0.7.0's floor names a prerelease that is not on the path, so
			// there is no hop from 0.4.0: the answer must be the gap, not a
			// silent step over an rc.
			wantErr: "no published release can be upgraded from 0.4.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planSteps(tc.installed, tc.target, tc.box, tc.releases)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("planSteps = %v, %v; want the error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planSteps: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planSteps = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// variants is a chart's variants.yaml with one digest per app.
func variants(sonarrDigest string) []byte {
	return variantsRepo(sonarrDigest, "registry.example.com/seedbox/sonarr")
}

// variantsRepo is the same file with sonarr published from another repository,
// for the case where the chart moves an app and not just its digest.
func variantsRepo(sonarrDigest, sonarrRepo string) []byte {
	return []byte(fmt.Sprintf(`qbittorrent:
  default:
    repository: registry.example.com/seedbox/qbittorrent
    tag: "5.3.0"
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
sonarr:
  default:
    repository: %s
    tag: "5.0.0"
    digest: %s
  alt:
    repository: registry.example.com/seedbox/sonarr-alt
    tag: "5.1.0"
    digest: sha256:9999999999999999999999999999999999999999999999999999999999999999
radarr:
  default:
    repository: registry.example.com/seedbox/radarr
    tag: "7.0.0"
    digest: sha256:3333333333333333333333333333333333333333333333333333333333333333
`, sonarrRepo, sonarrDigest))
}

// TestMovingApps: what a step stops and copies, and what the operator's own pin
// holds in place. qbittorrent's image never moves and radarr is off, so the
// only app either list can name is sonarr.
func TestMovingApps(t *testing.T) {
	const (
		digestOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		digestNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	// on is the values with every app enabled and sonarr's image as the case
	// needs it.
	on := func(sonarrImage map[string]any) map[string]any {
		return map[string]any{"apps": map[string]any{
			"qbittorrent": map[string]any{"enabled": true},
			"sonarr":      map[string]any{"enabled": true, "image": sonarrImage},
			"radarr":      map[string]any{"enabled": false},
		}}
	}
	defaultImage := map[string]any{"variant": "default"}
	old := release.Chart{Version: "0.4.0", Variants: variants(digestOld)}
	next := release.Chart{Version: "0.5.0", Variants: variants(digestNew)}
	same := release.Chart{Version: "0.5.1", Variants: variants(digestOld)}
	movedRepo := release.Chart{Version: "0.5.2", Variants: variantsRepo(digestOld, "registry.example.com/seedbox/sonarr-next")}
	// A variant the values select that the old release does not ship.
	noOldAlt := release.Chart{Version: "0.4.0", Variants: []byte(`sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
`)}

	cases := []struct {
		name   string
		from   release.Chart
		to     release.Chart
		values map[string]any
		moving []string
		held   []string
	}{
		{
			name: "no pin moves the app whose chart digest moved",
			from: old, to: next, values: on(defaultImage),
			moving: []string{"sonarr"},
		},
		{
			name: "a digest pin holds it",
			from: old, to: next,
			values: on(map[string]any{"variant": "default", "digest": digestOld}),
			held:   []string{"sonarr"},
		},
		{
			name: "a tag pin holds it",
			from: old, to: next,
			values: on(map[string]any{"variant": "default", "tag": "5.0.0"}),
			held:   []string{"sonarr"},
		},
		{
			name: "a pin does not hold an app whose repository moved",
			from: old, to: movedRepo,
			values: on(map[string]any{"variant": "default", "digest": digestOld}),
			moving: []string{"sonarr"},
		},
		{
			name: "a pin on an app the chart did not move is neither",
			from: old, to: same,
			values: on(map[string]any{"variant": "default", "digest": digestOld}),
		},
		{
			name: "a variant box cannot see on both sides still moves, pin or not",
			from: noOldAlt, to: next,
			values: on(map[string]any{"variant": "alt", "digest": digestOld}),
			moving: []string{"sonarr"},
		},
		{
			name: "a chart with no variants.yaml moves nothing",
			from: release.Chart{Version: "0.4.0"}, to: release.Chart{Version: "0.5.0"},
			values: on(defaultImage),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moving, held, err := movingApps(tc.from, tc.to, tc.values)
			if err != nil {
				t.Fatalf("movingApps: %v", err)
			}
			if !reflect.DeepEqual(moving, tc.moving) {
				t.Fatalf("moving = %v, want %v", moving, tc.moving)
			}
			if !reflect.DeepEqual(held, tc.held) {
				t.Fatalf("held = %v, want %v", held, tc.held)
			}
		})
	}
}
