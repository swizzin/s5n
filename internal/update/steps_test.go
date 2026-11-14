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
	return []byte(fmt.Sprintf(`qbittorrent:
  default:
    repository: registry.example.com/seedbox/qbittorrent
    tag: "5.3.0"
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
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
`, sonarrDigest))
}

func TestMovingApps(t *testing.T) {
	const (
		digestOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		digestNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	values := map[string]any{"apps": map[string]any{
		"qbittorrent": map[string]any{"enabled": true},
		"sonarr":      map[string]any{"enabled": true},
		"radarr":      map[string]any{"enabled": false},
	}}
	old := release.Chart{Version: "0.4.0", Variants: variants(digestOld)}
	next := release.Chart{Version: "0.5.0", Variants: variants(digestNew)}

	moving, err := movingApps(old, next, values)
	if err != nil {
		t.Fatalf("movingApps: %v", err)
	}
	if !reflect.DeepEqual(moving, []string{"sonarr"}) {
		t.Fatalf("moving = %v, want only sonarr: qbittorrent's image is unchanged and radarr is off", moving)
	}

	// The same digest is the same image, whatever the tag says.
	same, err := movingApps(old, release.Chart{Version: "0.5.1", Variants: variants(digestOld)}, values)
	if err != nil {
		t.Fatalf("movingApps: %v", err)
	}
	if len(same) != 0 {
		t.Fatalf("moving = %v with an unchanged image", same)
	}

	// A variant the values select does not exist in the old release: box
	// cannot tell, so it snapshots.
	alt := map[string]any{"apps": map[string]any{
		"sonarr": map[string]any{"enabled": true, "image": map[string]any{"variant": "alt"}},
	}}
	noOldAlt := release.Chart{Version: "0.4.0", Variants: []byte(`sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
`)}
	moving, err = movingApps(noOldAlt, next, alt)
	if err != nil {
		t.Fatalf("movingApps: %v", err)
	}
	if !reflect.DeepEqual(moving, []string{"sonarr"}) {
		t.Fatalf("moving = %v, want sonarr: the variant it selects is not in the old release", moving)
	}

	// A chart that ships no variants.yaml says nothing about images.
	moving, err = movingApps(release.Chart{Version: "0.4.0"}, release.Chart{Version: "0.5.0"}, values)
	if err != nil || len(moving) != 0 {
		t.Fatalf("moving = %v, %v with no variants files", moving, err)
	}
}
