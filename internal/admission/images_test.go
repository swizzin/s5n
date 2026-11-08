package admission

import (
	"reflect"
	"strings"
	"testing"
)

// TestBlessedMatchesThePolicyRule is the case-by-case table of the image rule:
// each row is the image, the blessed list, and what policy.yaml's
// `i.startsWith(r + ':') || i.startsWith(r + '@')` answers. The rows that
// differ from what a "parse the reference" rule would answer are marked, and
// they are the ones the Go copy has to get wrong on purpose.
func TestBlessedMatchesThePolicyRule(t *testing.T) {
	const (
		sonarr = "ghcr.io/home-operations/sonarr"
		s5n    = "ghcr.io/swizzin/s5n"
	)
	cases := []struct {
		name    string
		image   string
		blessed []string
		want    bool
		why     string
	}{
		{"tag", sonarr + ":5.0.0.2907", []string{sonarr}, true, `startsWith(repo + ':')`},
		{"digest", sonarr + "@sha256:abc", []string{sonarr}, true, `startsWith(repo + '@')`},
		{"tag and digest", sonarr + ":5.0.0.2907@sha256:abc", []string{sonarr}, true, "the tag's ':' comes first"},
		{"bare repository", sonarr, []string{sonarr}, false, "no tag and no digest: the image is not `repo:` or `repo@`"},
		{"hyphen suffix", sonarr + "-evil:1", []string{sonarr}, false, "'-' is not a boundary, so the prefix never matches"},
		{"path suffix", sonarr + "/extra:1", []string{sonarr}, false, "'/' is not a boundary either"},
		{"shorter repository", "ghcr.io/home-operations", []string{"ghcr.io/home-operations/sonarr"}, false, "the image is shorter than the repository"},
		{"case differs", "GHCR.io/home-operations/sonarr:5", []string{sonarr}, false, "CEL string comparison is case-sensitive"},
		{"empty image", "", []string{sonarr}, false, ""},
		{"no blessed entries", sonarr + ":5", nil, false, "an empty list admits nothing"},
		{"empty blessed entry", sonarr + ":5", []string{"", sonarr}, true, "the policy filters '' out of the split, then sonarr still matches"},
		{"only empty blessed entry", sonarr + ":5", []string{""}, false, "a bare ',' in the params must not bless ':anything'"},
		{
			name:    "docker.io implied, bare name blessed",
			image:   "busybox:1.36",
			blessed: []string{"busybox"},
			want:    true,
			why:     "the rule is textual: `busybox` is a blessed prefix",
		},
		{
			name:    "docker.io implied, full name given",
			image:   "docker.io/library/busybox:1.36",
			blessed: []string{"busybox"},
			want:    false,
			why:     "nothing is normalised: docker.io/library/busybox is not `busybox:`-prefixed",
		},
		{
			name:    "docker.io full name blessed, bare name given",
			image:   "busybox:1.36",
			blessed: []string{"docker.io/library/busybox"},
			want:    false,
			why:     "the same textual rule, the other way round",
		},
		{
			name:    "registry host with a port",
			image:   "registry.example.com:5000/app:tag",
			blessed: []string{"registry.example.com:5000/app"},
			want:    true,
			why:     "the port is inside the blessed repository",
		},
		{
			name:    "registry host alone, image has a port",
			image:   "registry.example.com:5000/app:tag",
			blessed: []string{"registry.example.com"},
			want:    true,
			why:     "the port's ':' is a boundary, so a bare host entry admits everything on it — the CEL's own hole, reproduced rather than fixed",
		},
		{
			name:    "s5n itself, tag empty in variants",
			image:   s5n + ":v1.2.3",
			blessed: []string{s5n},
			want:    true,
			why:     "the chart's own image, pinned by tag",
		},
		{
			name:    "s5n prefix lookalike",
			image:   "ghcr.io/swizzin/s5n-job:1",
			blessed: []string{s5n},
			want:    false,
			why:     "the reason the rule tests a boundary and not strings.HasPrefix(image, repo)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Blessed(tc.image, tc.blessed); got != tc.want {
				t.Fatalf("Blessed(%q, %v) = %v, want %v (%s)", tc.image, tc.blessed, got, tc.want, tc.why)
			}
		})
	}
}

func TestUnblessedImagesCoversEveryContainerList(t *testing.T) {
	blessed := []string{"ghcr.io/home-operations/sonarr", "ghcr.io/swizzin/s5n"}
	pods := []Pod{
		{
			Namespace: "example-user",
			Name:      "sonarr-0",
			Containers: []Container{
				{Name: "sonarr", Image: "ghcr.io/home-operations/sonarr:5.0.0.2907"},
				{Name: "sidecar", Image: "docker.io/library/nginx:1.27"},
			},
			InitContainers: []Container{
				{Name: "init", Image: "ghcr.io/swizzin/s5n:v1.2.3"},
				{Name: "creds", Image: "ghcr.io/example/creds:1"},
			},
			EphemeralContainers: []Container{
				{Name: "debugger", Image: "docker.io/library/busybox:1.36"},
			},
		},
		{
			Namespace:  "example-user",
			Name:       "example-0",
			Containers: []Container{{Name: "example", Image: "ghcr.io/example/example:1"}},
		},
		{
			Namespace:  "example-user",
			Name:       "sonarr-1",
			Containers: []Container{{Name: "sonarr", Image: "ghcr.io/home-operations/sonarr:6.0.0"}},
		},
	}
	want := []UnblessedImage{
		{Namespace: "example-user", Pod: "sonarr-0", Container: "sidecar", Kind: "container", Image: "docker.io/library/nginx:1.27"},
		{Namespace: "example-user", Pod: "sonarr-0", Container: "creds", Kind: "initContainer", Image: "ghcr.io/example/creds:1"},
		{Namespace: "example-user", Pod: "sonarr-0", Container: "debugger", Kind: "ephemeralContainer", Image: "docker.io/library/busybox:1.36"},
		{Namespace: "example-user", Pod: "example-0", Container: "example", Kind: "container", Image: "ghcr.io/example/example:1"},
	}
	got := UnblessedImages(pods, blessed)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnblessedImages =\n%+v\nwant\n%+v", got, want)
	}
}

func TestUnblessedImagesOfBlessedPodsIsEmpty(t *testing.T) {
	pods := []Pod{{
		Namespace: "example-user",
		Name:      "sonarr-0",
		Containers: []Container{
			{Name: "sonarr", Image: "ghcr.io/home-operations/sonarr:5.0.0.2907"},
		},
		InitContainers: []Container{
			{Name: "init", Image: "ghcr.io/swizzin/s5n@sha256:abc"},
		},
	}}
	if got := UnblessedImages(pods, []string{"ghcr.io/home-operations/sonarr", "ghcr.io/swizzin/s5n"}); len(got) != 0 {
		t.Fatalf("UnblessedImages = %+v, want none", got)
	}
}

func TestUnblessedImageString(t *testing.T) {
	u := UnblessedImage{Namespace: "example-user", Pod: "sonarr-0", Container: "sidecar", Kind: "container", Image: "docker.io/library/nginx:1.27"}
	if got, want := u.String(), "sonarr-0/sidecar (container): docker.io/library/nginx:1.27"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	u = UnblessedImage{Namespace: "example-user", Pod: "sonarr-0", Kind: "container", Image: "docker.io/library/nginx:1.27"}
	if got, want := u.String(), "sonarr-0 (container): docker.io/library/nginx:1.27"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestPolicyCELStillMatchesTheGoRule fails when policy.yaml's image rule stops
// being the prefix test Blessed reproduces: the Go copy is what names the pods a
// new binding strands, and it is only useful while it answers what the cluster
// answers. A string check, not a CEL evaluator — this module carries no CEL
// dependency and a disagreement shows up here before it shows up in a cluster.
func TestPolicyCELStillMatchesTheGoRule(t *testing.T) {
	policy := string(readFixture(t, "../../install/platform/admission/policy.yaml"))
	for _, want := range []string{
		`i.startsWith(r + ':') || i.startsWith(r + '@')`,
		`variables.unblessed.size() == 0`,
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy.yaml no longer contains %q: the Go rule in this package has to follow it", want)
		}
	}
}
