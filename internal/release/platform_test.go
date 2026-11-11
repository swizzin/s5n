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

// platformDoc is a whole platform.yaml for the parser tests: the five pins and
// the two k0s checksums, spelled the way the chart ships them. A test that needs
// a damaged file changes one line of this, so the damage is the only thing
// wrong with it.
func platformDoc() string {
	return "authelia: \"1.0.0@sha256:" + strings.Repeat("a", 64) + "\"\n" +
		"k0s: v1.0.0\n" +
		"k0sSHA256amd64: \"" + strings.Repeat("1", 64) + "\"\n" +
		"k0sSHA256arm64: \"" + strings.Repeat("2", 64) + "\"\n" +
		"envoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n"
}

// docWithout is platformDoc with one line left out.
func docWithout(line string) string {
	return strings.Replace(platformDoc(), line, "", 1)
}

// docWith is platformDoc with one line replaced by another.
func docWith(old, line string) string {
	return strings.Replace(platformDoc(), old, line, 1)
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
	// The k0s checksums are the fixture chart's too, one per architecture: a
	// sum read from anywhere but the release in hand shows up here.
	if chart.Pins.K0sSHA256["amd64"] != strings.Repeat("1", 64) || chart.Pins.K0sSHA256["arm64"] != strings.Repeat("2", 64) {
		t.Fatalf("k0s checksums = %v, want the fixture chart's", chart.Pins.K0sSHA256)
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
	body := []byte(docWith("metallb: 1.0.0\n", "metallb: \"\"\n"))
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
// plan an install at version "". The k0s checksums are keys like any other, and
// a sum that is not a sha256 is refused for what it is -- 64 lowercase hex, as
// the release's own sha256sums.txt writes it.
func TestParseVersionsRefusesADamagedFile(t *testing.T) {
	full := platformDoc()
	files := map[string]struct {
		doc  string
		want string
	}{
		"a missing key": {
			doc:  docWithout("certManager: v1.0.0\n"),
			want: `no "certManager"`,
		},
		"an empty value": {
			doc:  docWith("metallb: 1.0.0\n", "metallb: \"\"\n"),
			want: `"metallb" is empty`,
		},
		"a misspelt key": {
			doc:  full + "envoygateway: 1.0.0\n",
			want: `unknown key "envoygateway"`,
		},
		"a missing k0s checksum": {
			doc:  docWithout("k0sSHA256arm64: \"" + strings.Repeat("2", 64) + "\"\n"),
			want: `no "k0sSHA256arm64"`,
		},
		"a checksum of the wrong length": {
			doc:  docWith(strings.Repeat("1", 64), strings.Repeat("1", 63)),
			want: `"k0sSHA256amd64" is`,
		},
		"an uppercase checksum": {
			doc:  docWith(strings.Repeat("1", 64), strings.Repeat("A", 64)),
			want: `not a sha256`,
		},
		"a checksum that is not hex": {
			doc:  docWith(strings.Repeat("1", 64), strings.Repeat("g", 64)),
			want: `not a sha256`,
		},
		"a checksum left unquoted": {
			doc:  docWith("k0sSHA256amd64: \""+strings.Repeat("1", 64)+"\"", "k0sSHA256amd64: "+strings.Repeat("1", 64)),
			want: "not a string: quote it",
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
	if v.K0sSHA256["amd64"] != strings.Repeat("1", 64) || v.K0sSHA256["arm64"] != strings.Repeat("2", 64) {
		t.Fatalf("parsed k0s checksums = %v, want them keyed by architecture", v.K0sSHA256)
	}
}

// A value typed as a YAML number instead of a string is refused rather than
// silently rounded: a bare 1.10 is 1.1 to a YAML parser, and a chart version
// is not a number.
func TestParseVersionsKeepsTheVersionAsWritten(t *testing.T) {
	v, err := ParseVersions([]byte(docWith("k0s: v1.0.0\n", "k0s: v1.37.2+k0s.1\n")))
	if err != nil {
		t.Fatal(err)
	}
	if v.K0s != "v1.37.2+k0s.1" || v.EnvoyGateway != "1.0.0" || v.MetalLB != "1.0.0" || v.CertManager != "v1.0.0" {
		t.Fatalf("parsed %+v", v)
	}
	if !strings.HasPrefix(v.Authelia, "1.0.0@sha256:") {
		t.Fatalf("parsed authelia %q: an image reference is kept as written, repository and all", v.Authelia)
	}
	// A sum is kept as written too: it is compared byte for byte with what the
	// release publishes, so nothing here trims or lowercases it.
	if v.K0sSHA256["amd64"] != strings.Repeat("1", 64) {
		t.Fatalf("parsed k0s checksum %q", v.K0sSHA256["amd64"])
	}
	if _, err := ParseVersions([]byte(docWith("k0s: v1.0.0\n", "k0s: 1.10\n"))); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Fatalf("a version written as a YAML float was accepted (it reads as 1.1, not 1.10): %v", err)
	}
}

// The shipped file is the only home for the pins, so it must carry all five as
// bare versions -- this is the test that fails when someone adds a component to
// the parser and forgets the chart, or the other way round. The two k0s
// checksums are here for the same reason: box installs nothing it cannot
// verify, and the shipped file is where that sum comes from.
func TestShippedPinsAreWhole(t *testing.T) {
	pins := shippedPins(t)
	for key, got := range map[string]string{
		"authelia":       pins.Authelia,
		"k0s":            pins.K0s,
		"k0sSHA256amd64": pins.K0sSHA256["amd64"],
		"k0sSHA256arm64": pins.K0sSHA256["arm64"],
		"envoyGateway":   pins.EnvoyGateway,
		"metallb":        pins.MetalLB,
		"certManager":    pins.CertManager,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: no pin in charts/s5n/%s", key, PlatformFile)
		}
		if strings.ContainsAny(got, " 	\"'") {
			t.Errorf("%s: %q is not a bare version", key, got)
		}
	}
}

// The k0s checksums live under the k0s line and nowhere else, quoted, with no
// `# renovate:` marker: they are not versions, so Renovate must never open a
// pull request against one. They move in the same pull request as the version
// above them -- `go run ./hack/k0s-sums --write` writes all three lines -- and
// adjacency is what keeps a reader (and this test) from finding a sum that
// describes some other release.
func TestK0sChecksumsSitUnderTheirVersionWithoutARenovateMarker(t *testing.T) {
	file := string(shippedFile(t))
	block := regexp.MustCompile("(?m)^k0s: \\S+[^\n]*\n" +
		`k0sSHA256amd64: "[0-9a-f]{64}"` + "\n" +
		`k0sSHA256arm64: "[0-9a-f]{64}"` + "\n")
	if !block.MatchString(file) {
		t.Fatalf("charts/s5n/%s must carry the quoted amd64 and arm64 checksums on the two lines under k0s:, and nothing else on those lines:\n%s", PlatformFile, file)
	}
	if regexp.MustCompile(`(?m)^k0sSHA256\w+:.*# renovate:`).MatchString(file) {
		t.Error("a k0s checksum line carries a # renovate: marker: Renovate must not bump a checksum, it moves with the k0s line")
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
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
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
