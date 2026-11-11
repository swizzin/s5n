package main

import (
	"strings"
	"testing"
)

// hex64 is a sha256-shaped digest built from one character, so a fixture reads
// as a digest without spelling 64 hex digits out.
func hex64(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// variantsYAML is a charts/s5n/variants.yaml with two apps: qbittorrent is
// constant, so the sonarr line under test is the only thing that can move.
func variantsYAML(sonarrTag, sonarrDigest string) []byte {
	return []byte(`qbittorrent:
  default:
    repository: ghcr.io/home-operations/qbittorrent
    tag: "5.3.0"
    digest: ` + hex64("c") + `
sonarr:
  default:
    repository: ghcr.io/home-operations/sonarr
    tag: "` + sonarrTag + `"
    digest: ` + sonarrDigest + `
s5n:
  default:
    repository: ghcr.io/swizzin/s5n
    tag: ""
    digest: ""
`)
}

// platformYAML is a charts/s5n/platform.yaml: the login's whole image reference,
// k0s's release tag, and two chart pins. k0s is constant, so the pins under
// test are the only things that can move.
func platformYAML(envoy, metallb, autheliaTag string) []byte {
	return []byte(`authelia: "docker.io/authelia/authelia:` + autheliaTag + `@` + hex64("d") + `"  # renovate: datasource=docker depName=authelia/authelia
k0s: v1.37.2+k0s.1       # renovate: datasource=github-releases depName=k0sproject/k0s
envoyGateway: "` + envoy + `"       # renovate: datasource=docker depName=envoyproxy/gateway-helm
metallb: "` + metallb + `"           # renovate: datasource=helm depName=metallb registryUrl=https://metallb.github.io/metallb
`)
}

// TestDecideRange is the case table: which ranges a push to main may cut a
// chart release for, and which it must refuse.
func TestDecideRange(t *testing.T) {
	variants := variantsYAML("5.0.0.2907", hex64("a"))
	platform := platformYAML("1.9.3", "0.16.1", "4.39.28")
	tests := []struct {
		name     string
		changed  []string
		oldV     []byte
		newV     []byte
		oldP     []byte
		newP     []byte
		prev     string
		release  bool
		version  string
		reasonIn []string
	}{
		{
			name:    "a digest bump releases",
			changed: []string{variantsFile},
			oldV:    variants, newV: variantsYAML("5.0.0.2907", hex64("b")),
			oldP: platform, newP: platform,
			prev: "0.2.0", release: true, version: "0.2.1",
		},
		{
			name:    "a patch bump releases",
			changed: []string{variantsFile},
			oldV:    variants, newV: variantsYAML("5.0.0.2910", hex64("a")),
			oldP: platform, newP: platform,
			prev: "0.2.0", release: true, version: "0.2.1",
		},
		{
			name:    "a minor bump does not release",
			changed: []string{variantsFile},
			oldV:    variants, newV: variantsYAML("5.1.0.2907", hex64("a")),
			oldP: platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"minor bump", "sonarr.default"},
		},
		{
			name:    "a major bump does not release",
			changed: []string{variantsFile},
			oldV:    variants, newV: variantsYAML("6.0.0.2907", hex64("a")),
			oldP: platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"major bump"},
		},
		{
			name:    "a platform patch releases the chart",
			changed: []string{platformFile},
			oldV:    variants, newV: variants,
			oldP: platform, newP: platformYAML("1.9.4", "0.16.1", "4.39.28"),
			prev: "0.2.0", release: true, version: "0.2.1",
		},
		{
			name:    "a platform minor does not release",
			changed: []string{platformFile},
			oldV:    variants, newV: variants,
			oldP: platform, newP: platformYAML("1.10.0", "0.16.1", "4.39.28"),
			prev:     "0.2.0",
			reasonIn: []string{"envoyGateway", "minor bump"},
		},
		{
			name:    "the login's digest moving with its tag releases",
			changed: []string{platformFile},
			oldV:    variants, newV: variants,
			oldP: platform, newP: platformYAML("1.9.3", "0.16.1", "4.39.29"),
			prev: "0.2.0", release: true, version: "0.2.1",
		},
		{
			// One file moved each, both on main, one push: one chart release
			// that carries both.
			name:    "mixed variants and platform patches are one release",
			changed: []string{variantsFile, platformFile},
			oldV:    variants, newV: variantsYAML("5.0.0.2910", hex64("a")),
			oldP: platform, newP: platformYAML("1.9.3", "0.16.2", "4.39.28"),
			prev: "0.2.0", release: true, version: "0.2.1",
		},
		{
			name:    "any other file does not release, and is named",
			changed: []string{variantsFile, "charts/s5n/templates/deployment.yaml", "internal/platform/platform.go"},
			oldV:    variants, newV: variantsYAML("5.0.0.2910", hex64("a")),
			oldP: platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"charts/s5n/templates/deployment.yaml", "internal/platform/platform.go"},
		},
		{
			name:    "nothing since the tag does not release",
			changed: nil,
			oldV:    variants, newV: variants,
			oldP: platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"no commits since s5n-v0.2.0"},
		},
		{
			name:    "no s5n-v tag, only v0.2.0, does not release",
			changed: []string{variantsFile},
			oldV:    variants, newV: variantsYAML("5.0.0.2910", hex64("a")),
			oldP: platform, newP: platform,
			prev:     "",
			reasonIn: []string{"no s5n-v* tag", "v0.2.0"},
		},
		{
			name:    "the files listed but not moved does not release",
			changed: []string{variantsFile, platformFile},
			oldV:    variants, newV: variants,
			oldP: platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"neither"},
		},
		{
			name:    "a k0s minor does not release",
			changed: []string{platformFile},
			oldV:    variants, newV: variants,
			oldP:     platform,
			newP:     []byte(strings.Replace(string(platform), "v1.37.2+k0s.1", "v1.38.0+k0s.1", 1)),
			prev:     "0.2.0",
			reasonIn: []string{"k0s", "minor bump"},
		},
		{
			name:    "an added pin does not release",
			changed: []string{platformFile},
			oldV:    variants, newV: variants,
			oldP:     platform,
			newP:     append(append([]byte{}, platform...), []byte("pebble: \"1.0.0\"\n")...),
			prev:     "0.2.0",
			reasonIn: []string{"pin pebble was added"},
		},
		{
			name:    "a changed repository does not release",
			changed: []string{variantsFile},
			oldV:    variants,
			newV:    []byte(strings.Replace(string(variants), "ghcr.io/home-operations/sonarr", "ghcr.io/someone/sonarr", 1)),
			oldP:    platform, newP: platform,
			prev:     "0.2.0",
			reasonIn: []string{"the repository changed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decideRange(tt.changed, tt.oldV, tt.newV, tt.oldP, tt.newP, tt.prev)
			if err != nil {
				t.Fatalf("decideRange: %v", err)
			}
			if got.Release != tt.release {
				t.Fatalf("Release = %v, want %v (reason: %s)", got.Release, tt.release, got.Reason)
			}
			if tt.release && got.Version != tt.version {
				t.Fatalf("Version = %q, want %q", got.Version, tt.version)
			}
			if !tt.release && got.Version != "" {
				t.Fatalf("Version = %q on a refusal, want empty", got.Version)
			}
			if got.Reason == "" {
				t.Fatal("a decision must carry a reason for the log")
			}
			for _, want := range tt.reasonIn {
				if !strings.Contains(got.Reason, want) {
					t.Fatalf("reason %q does not mention %q", got.Reason, want)
				}
			}
		})
	}
}

// TestDecideRangeCountsTheBaseTag: the version is the last s5n-v* tag's patch
// plus one, not Chart.yaml's or the push's own idea of a version.
func TestDecideRangeCountsTheBaseTag(t *testing.T) {
	d, err := decideRange(
		[]string{variantsFile},
		variantsYAML("5.0.0.2907", hex64("a")), variantsYAML("5.0.0.2910", hex64("a")),
		platformYAML("1.9.3", "0.16.1", "4.39.28"), platformYAML("1.9.3", "0.16.1", "4.39.28"),
		"0.2.14",
	)
	if err != nil {
		t.Fatalf("decideRange: %v", err)
	}
	if !d.Release || d.Version != "0.2.15" {
		t.Fatalf("decision = %+v, want a release of 0.2.15", d)
	}
}

// TestClassify mirrors Renovate's own update-type rule for a docker tag: major
// is the first component, minor the second, and everything else -- a fourth
// build number included -- is a patch. This is what lets sonarr's four-part tag
// automerge, and so what the decision has to agree with.
func TestClassify(t *testing.T) {
	tests := []struct {
		old, new string
		want     bump
	}{
		{"5.0.0.2907", "5.0.0.2910", bumpPatch}, // a fourth component is a patch
		{"1.43.5.11079", "1.43.6.11079", bumpPatch},
		{"1.2.3", "1.2.4", bumpPatch},
		{"v1.37.2+k0s.1", "v1.37.3+k0s.1", bumpPatch},
		{"1.2.3", "1.2.3", bumpNone},
		{"1.2.3", "1.3.0", bumpMinor},
		{"1.2.3", "2.0.0", bumpMajor},
		{"0.16.1", "0.17.0", bumpMinor},
		{"1.2.3", "1.2.2", bumpOther}, // a downgrade is not an update
		{"latest", "1.2.3", bumpOther},
	}
	for _, tt := range tests {
		if got := classify(tt.old, tt.new); got != tt.want {
			t.Errorf("classify(%q, %q) = %v, want %v", tt.old, tt.new, got, tt.want)
		}
	}
}

// TestPinVersion: a pin's version is the image tag, for the login's whole
// image reference as much as for a bare version.
func TestPinVersion(t *testing.T) {
	tests := map[string]string{
		"4.39.28":                             "4.39.28",
		"v1.37.2+k0s.1":                       "v1.37.2+k0s.1",
		"docker.io/authelia/authelia:4.39.28": "4.39.28",
		"docker.io/authelia/authelia:4.39.28@sha256:" + strings.Repeat("a", 64): "4.39.28",
	}
	for in, want := range tests {
		if got := pinVersion(in); got != want {
			t.Errorf("pinVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMovedImages reads one half of the s5n release body: the app images that
// moved between two variants.yaml files.
func TestMovedImages(t *testing.T) {
	got, err := movedImages(variantsYAML("5.0.0.2907", hex64("a")), variantsYAML("5.0.0.2910", hex64("b")))
	if err != nil {
		t.Fatalf("movedImages: %v", err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "sonarr: 5.0.0.2907 -> 5.0.0.2910") {
		t.Fatalf("moved images = %q, want the sonarr move", joined)
	}
	if strings.Contains(joined, "qbittorrent") {
		t.Fatalf("moved images = %q, want qbittorrent left out", joined)
	}
}

// TestMovedPins reads the other half: a platform patch ships in the chart, so
// the release body has to name it.
func TestMovedPins(t *testing.T) {
	got, err := movedPins(
		platformYAML("1.9.3", "0.16.1", "4.39.28"),
		platformYAML("1.9.3", "0.16.2", "4.39.28"),
	)
	if err != nil {
		t.Fatalf("movedPins: %v", err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "metallb: 0.16.1 -> 0.16.2") {
		t.Fatalf("moved pins = %q, want the metallb move", joined)
	}
	if strings.Contains(joined, "envoyGateway") || strings.Contains(joined, "k0s") {
		t.Fatalf("moved pins = %q, want the unmoved pins left out", joined)
	}

	// The login is a whole image reference: its tag is what the body names.
	got, err = movedPins(
		platformYAML("1.9.3", "0.16.1", "4.39.28"),
		platformYAML("1.9.3", "0.16.1", "4.39.29"),
	)
	if err != nil {
		t.Fatalf("movedPins: %v", err)
	}
	if strings.Join(got, "\n") != "authelia: 4.39.28 -> 4.39.29" {
		t.Fatalf("moved pins = %q, want the login's tag move alone", got)
	}
}

// TestBody: both sections are in the release's markdown, and an empty one says
// so rather than vanishing.
func TestBody(t *testing.T) {
	out := body("s5n-v0.2.0", []string{"sonarr: 5.0.0.2907 -> 5.0.0.2910"}, nil)
	for _, want := range []string{
		"### App images that moved since s5n-v0.2.0",
		"- sonarr: 5.0.0.2907 -> 5.0.0.2910",
		"### Platform pins that moved since s5n-v0.2.0",
		"- none",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("body = %q, want %q", out, want)
		}
	}
	first := body("", []string{"sonarr: ghcr.io/home-operations/sonarr:5.0.0.2907"}, []string{"k0s: v1.37.2+k0s.1"})
	if !strings.HasPrefix(first, "The first s5n release:") {
		t.Fatalf("first release body = %q, want the first-release sentence", first)
	}
}
