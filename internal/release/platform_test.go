package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shippedFile is charts/s5n/platform.yaml, the file a chart release carries.
// The other packages read it through releasetest; this one cannot (that helper
// returns this package's own type), so it reads the path it owns.
func shippedFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "s5n", "platform.yaml"))
	if err != nil {
		t.Fatalf("read the chart's platform pins: %v", err)
	}
	return b
}

// shippedPins is that file, parsed the way box parses it.
func shippedPins(t *testing.T) Versions {
	t.Helper()
	pins, err := ParseVersions(shippedFile(t))
	if err != nil {
		t.Fatalf("parse charts/s5n/platform.yaml: %v", err)
	}
	return pins
}

// The pins ship inside the chart now, so the fixture loader is the first thing
// to prove: a chart that carries platform.yaml has them parsed, and box never
// reads a version out of its own binary.
func TestChartCarriesThePlatformPins(t *testing.T) {
	chart := testChart(t)
	// testdata/chart is a fixture, not the real chart: its pins are its own, so
	// a value leaking in from a global (or from the real chart) shows up here.
	if chart.Pins.K0s != "v1.99.0+k0s.1" || chart.Pins.EnvoyGateway != "9.9.9" || chart.Pins.MetalLB != "9.9.9" {
		t.Fatalf("chart pins = %+v, want the fixture chart's", chart.Pins)
	}
	if !strings.HasPrefix(chart.Pins.Authelia, "docker.io/example/authelia:1.0.0@sha256:") {
		t.Fatalf("authelia pin = %q", chart.Pins.Authelia)
	}
	// And the checkout's own chart parses to something box would install.
	if got := shippedPins(t); got.K0s == "" || got.Authelia == "" {
		t.Fatalf("the checkout's chart pins = %+v", got)
	}
}

// copyChart writes a copy of testdata/chart into a temp directory, with the
// named file replaced or (when body is nil) left out. Tests that refuse a
// release need a release that is wrong in exactly one way.
func copyChart(t *testing.T, omit string, replace map[string][]byte) string {
	t.Helper()
	dst := t.TempDir()
	src := "testdata/chart"
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == omit {
			continue
		}
		var data []byte
		if body, ok := replace[name]; ok {
			data = body
		} else {
			data, err = os.ReadFile(filepath.Join(src, name))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// A release that ships no platform.yaml is one box cannot install: there is no
// compiled-in fallback, and inventing one here would put the platform back in
// the binary. The refusal names the release, so the operator can see which one
// they pulled.
func TestChartWithoutPlatformPinsIsRefused(t *testing.T) {
	dir := copyChart(t, PlatformFile, nil)
	_, err := loadChartDir(DevVersion, dir)
	if err == nil {
		t.Fatal("a chart with no platform.yaml loaded")
	}
	for _, want := range []string{"chart s5n-test 0.0.1", PlatformFile} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// An empty pin is the same refusal one step later: box would otherwise plan an
// install at version "", which is a chart nobody publishes.
func TestChartWithAnEmptyPinIsRefused(t *testing.T) {
	body := []byte("authelia: \"docker.io/example/authelia:1.0.0\"\nk0s: v1.99.0+k0s.1\nenvoyGateway: 9.9.9\nmetallb: \"\"\ncertManager: v9.9.9\n")
	dir := copyChart(t, "", map[string][]byte{PlatformFile: body})
	_, err := loadChartDir(DevVersion, dir)
	if err == nil {
		t.Fatal("a chart with an empty pin loaded")
	}
	for _, want := range []string{"chart s5n-test 0.0.1", `"metallb" is empty`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// A missing or unknown key is an error, not a zero value: box would otherwise
// plan an install at version "".
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
			if _, err := ParseVersions([]byte(tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseVersions(%q) = %v, want an error containing %q", tc.doc, err, tc.want)
			}
		})
	}

	v, err := ParseVersions([]byte(full))
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
	v, err := ParseVersions([]byte("authelia: \"docker.io/example/authelia:4.39.28@sha256:" + strings.Repeat("b", 64) + "\"\nk0s: v1.37.2+k0s.1\nenvoyGateway: \"1.9.3\"\nmetallb: 0.16.1\ncertManager: v1.21.3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v.K0s != "v1.37.2+k0s.1" || v.EnvoyGateway != "1.9.3" || v.MetalLB != "0.16.1" || v.CertManager != "v1.21.3" {
		t.Fatalf("parsed %+v", v)
	}
	if !strings.HasPrefix(v.Authelia, "docker.io/example/authelia:4.39.28@sha256:") {
		t.Fatalf("parsed authelia %q: an image reference is kept as written, repository and all", v.Authelia)
	}
	if _, err := ParseVersions([]byte("authelia: \"1.0.0\"\nk0s: 1.10\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n")); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Fatalf("a version written as a YAML float was accepted (it reads as 1.1, not 1.10): %v", err)
	}
}

// The shipped file is the only home for the pins, so it must carry all five as
// bare versions -- this is the test that fails when someone adds a component to
// the parser and forgets the chart, or the other way round.
func TestShippedPinsAreWhole(t *testing.T) {
	pins := shippedPins(t)
	for key, got := range map[string]string{
		"authelia":     pins.Authelia,
		"k0s":          pins.K0s,
		"envoyGateway": pins.EnvoyGateway,
		"metallb":      pins.MetalLB,
		"certManager":  pins.CertManager,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: no pin in charts/s5n/%s", key, PlatformFile)
		}
		if strings.ContainsAny(got, " \t\"'") {
			t.Errorf("%s: %q is not a bare version", key, got)
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
	pin := shippedPins(t).Authelia
	ref, digest, ok := strings.Cut(pin, "@sha256:")
	if !ok {
		t.Fatalf("authelia pin %q has no @sha256: digest: an image box installs is pinned by digest, like every test image", pin)
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
		t.Fatalf("authelia pin %q names no repository: it has to be <registry>/<repository>:<tag>@sha256:..., not a bare tag", pin)
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
	if strings.HasPrefix(pin, tag+"@") {
		t.Fatalf("authelia pin %q starts with the version: the repository is missing", pin)
	}
	// The line is what Renovate reads: a docker datasource comment naming the
	// same repository, on the same line as the whole reference, so a bump is
	// one pull request against one package and updates the digest with the tag.
	if !regexp.MustCompile(`(?m)^authelia: "` + regexp.QuoteMeta(pin) + `"\s+# renovate: datasource=docker depName=` + regexp.QuoteMeta(renovateRepo) + `$`).Match(shippedFile(t)) {
		t.Errorf("the authelia line in charts/s5n/%s is not a quoted pin with a docker datasource comment:\n%s", PlatformFile, shippedFile(t))
	}
}

// renovateRepo is the upstream image Renovate watches for the login.
const renovateRepo = "authelia/authelia"

// Every other pin carries a Renovate comment naming its datasource too: the
// chart move must not have cost the other lane's regex a line.
func TestEveryPinCarriesItsRenovateSource(t *testing.T) {
	file := string(shippedFile(t))
	for key, want := range map[string]string{
		"k0s":          "datasource=github-releases depName=k0sproject/k0s",
		"envoyGateway": "datasource=docker depName=envoyproxy/gateway-helm",
		"metallb":      "datasource=helm depName=metallb",
		"certManager":  "datasource=docker depName=jetstack/charts/cert-manager",
	} {
		line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `: \S+\s+# renovate: (.*)$`).FindStringSubmatch(file)
		if line == nil {
			t.Errorf("%s: no pin line with a # renovate: comment", key)
			continue
		}
		if !strings.Contains(line[1], want) {
			t.Errorf("%s: renovate comment = %q, want it to name %q", key, line[1], want)
		}
	}
}
