package platformassets

import (
	"regexp"
	"strings"
	"testing"
)

// The file is the only home for the pins, so the shipped bytes must already be
// the four values box installs -- this is the test that fails when someone adds
// a component to the parser and forgets the file, or the other way round.
func TestEmbeddedVersions(t *testing.T) {
	for key, got := range map[string]string{
		"authelia":     Pins.Authelia,
		"k0s":          Pins.K0s,
		"envoyGateway": Pins.EnvoyGateway,
		"metallb":      Pins.MetalLB,
		"certManager":  Pins.CertManager,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: no pin in the embedded file", key)
		}
		if strings.ContainsAny(got, " 	\"'") {
			t.Errorf("%s: %q is not a bare version", key, got)
		}
	}
	// The values are the ones in the file, not defaults compiled into Go.
	for _, pin := range []string{Pins.Authelia, Pins.K0s, Pins.EnvoyGateway, Pins.MetalLB, Pins.CertManager} {
		if !strings.Contains(string(File), pin) {
			t.Errorf("%q is not in the embedded file:\n%s", pin, File)
		}
	}
}

// TestAutheliaPinIsAnImageReference pins the one entry here that is not a
// chart version: box writes the login's Deployment itself, so the pin has to
// be the whole image reference -- repository, tag and digest. A tag alone would
// let the login change under a box that never changed; a tag and digest with no
// repository is not an image at all (containerd reads it as
// docker.io/library/<tag>), which is a pod that never starts.
func TestAutheliaPinIsAnImageReference(t *testing.T) {
	ref, digest, ok := strings.Cut(Pins.Authelia, "@sha256:")
	if !ok {
		t.Fatalf("authelia pin %q has no @sha256: digest: an image box installs is pinned by digest, like every test image", Pins.Authelia)
	}
	if len(digest) != 64 {
		t.Errorf("authelia digest %q is not a sha256 hex string", digest)
	}
	for _, r := range digest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("authelia digest %q is not lowercase hex", digest)
		}
	}
	// The reference is repository, tag: the slash and the last colon are what
	// make it resolvable, and a value without them is the version on its own.
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || !strings.Contains(ref[:colon], "/") {
		t.Fatalf("authelia pin %q names no repository: it has to be <registry>/<repository>:<tag>@sha256:..., not a bare tag", Pins.Authelia)
	}
	repo, tag := ref[:colon], ref[colon+1:]
	// The repository is the one Renovate's depName names, and it does not
	// change when the version does.
	if repo != "docker.io/"+renovateRepo {
		t.Errorf("authelia repository is %q, want docker.io/%s", repo, renovateRepo)
	}
	if tag == "" || strings.ContainsAny(tag, ":@") {
		t.Errorf("authelia tag %q is not a bare tag", tag)
	}
	if strings.HasPrefix(Pins.Authelia, tag+"@") {
		t.Fatalf("authelia pin %q starts with the version: the repository is missing", Pins.Authelia)
	}
	// The line is what Renovate reads: a docker datasource comment naming the
	// same repository, on the same line as the whole reference, so a bump is
	// one pull request against one package and updates the digest with the tag.
	if !regexp.MustCompile(`(?m)^authelia: "` + regexp.QuoteMeta(Pins.Authelia) + `"\s+# renovate: datasource=docker depName=` + regexp.QuoteMeta(renovateRepo) + `$`).Match(File) {
		t.Errorf("the authelia line in the embedded file is not a quoted pin with a docker datasource comment:\n%s", File)
	}
}

// renovateRepo is the upstream image Renovate watches for the login.
const renovateRepo = "authelia/authelia"

// A missing, empty or unknown key is an error, not a zero value: box would
// otherwise plan an install at version "".
func TestParseVersionsRefusesADamagedFile(t *testing.T) {
	full := "authelia: \"1.0.0@sha256:" + strings.Repeat("a", 64) + "\"\nk0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n"
	files := map[string]struct {
		doc  string
		want string
	}{
		"a missing key": {
			doc:  "authelia: \"1.0.0\"\nk0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\n",
			want: `no "certManager"`,
		},
		"an empty value": {
			doc:  "authelia: \"1.0.0\"\nk0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: \"\"\ncertManager: v1.0.0\n",
			want: `"metallb" is empty`,
		},
		"a misspelt key": {
			doc:  full + "envoygateway: 1.0.0\n",
			want: `unknown key "envoygateway"`,
		},
		"not a mapping": {
			doc:  "- 1.0.0\n",
			want: "read:",
		},
	}
	for name, tc := range files {
		t.Run(name, func(t *testing.T) {
			if _, err := parseVersions([]byte(tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseVersions(%q) = %v, want an error containing %q", tc.doc, err, tc.want)
			}
		})
	}

	v, err := parseVersions([]byte(full))
	if err != nil {
		t.Fatalf("a complete file was refused: %v", err)
	}
	if v.K0s != "v1.0.0" || v.EnvoyGateway != "1.0.0" || v.MetalLB != "1.0.0" || v.CertManager != "v1.0.0" {
		t.Fatalf("parsed %+v", v)
	}
	if !strings.HasPrefix(v.Authelia, "1.0.0@sha256:") {
		t.Fatalf("parsed authelia %q", v.Authelia)
	}
}

// A value typed as a YAML number instead of a string is refused rather than
// silently rounded: a bare 1.10 is 1.1 to a YAML parser, and a chart version
// is not a number.
func TestParseVersionsKeepsTheVersionAsWritten(t *testing.T) {
	v, err := parseVersions([]byte("authelia: \"docker.io/example/authelia:4.39.28@sha256:" + strings.Repeat("b", 64) + "\"\nk0s: v1.37.2+k0s.1\nenvoyGateway: \"1.9.3\"\nmetallb: 0.16.1\ncertManager: v1.21.3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v.K0s != "v1.37.2+k0s.1" || v.EnvoyGateway != "1.9.3" || v.MetalLB != "0.16.1" || v.CertManager != "v1.21.3" {
		t.Fatalf("parsed %+v", v)
	}
	if !strings.HasPrefix(v.Authelia, "docker.io/example/authelia:4.39.28@sha256:") {
		t.Fatalf("parsed authelia %q: an image reference is kept as written, repository and all", v.Authelia)
	}
	if _, err := parseVersions([]byte("authelia: \"1.0.0\"\nk0s: 1.10\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n")); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Fatalf("a version written as a YAML float was accepted (it reads as 1.1, not 1.10): %v", err)
	}
}
