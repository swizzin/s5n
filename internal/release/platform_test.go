package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shippedFile is charts/s5n/platform.yaml, read directly: releasetest returns
// this package's own type.
func shippedFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "s5n", "platform.yaml"))
	if err != nil {
		t.Fatalf("read the chart's platform pins: %v", err)
	}
	return b
}

// shippedPins is that file parsed as box parses it.
func shippedPins(t *testing.T) Versions {
	t.Helper()
	pins, err := ParseVersions(shippedFile(t))
	if err != nil {
		t.Fatalf("parse charts/s5n/platform.yaml: %v", err)
	}
	return pins
}

// platformDoc is a whole platform.yaml; a damaged test changes one line.
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

func TestChartCarriesThePlatformPins(t *testing.T) {
	chart := testChart(t)
	// The fixture's own pins expose a value leaking from a global or the chart.
	if chart.Pins.K0s != "v1.99.0+k0s.1" || chart.Pins.EnvoyGateway != "9.9.9" || chart.Pins.MetalLB != "9.9.9" {
		t.Fatalf("chart pins = %+v, want the fixture chart's", chart.Pins)
	}
	if !strings.HasPrefix(chart.Pins.Authelia, "docker.io/example/authelia:1.0.0@sha256:") {
		t.Fatalf("authelia pin = %q", chart.Pins.Authelia)
	}
	// One fixture checksum per architecture exposes a sum from elsewhere.
	if chart.Pins.K0sSHA256["amd64"] != strings.Repeat("1", 64) || chart.Pins.K0sSHA256["arm64"] != strings.Repeat("2", 64) {
		t.Fatalf("k0s checksums = %v, want the fixture chart's", chart.Pins.K0sSHA256)
	}
	// And the checkout's own chart parses to something box would install.
	if got := shippedPins(t); got.K0s == "" || got.Authelia == "" {
		t.Fatalf("the checkout's chart pins = %+v", got)
	}
}

// copyChart copies the fixture with one file replaced or left out.
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
	// A sum is kept as written: it is compared byte for byte.
	if v.K0sSHA256["amd64"] != strings.Repeat("1", 64) {
		t.Fatalf("parsed k0s checksum %q", v.K0sSHA256["amd64"])
	}
	if _, err := ParseVersions([]byte(docWith("k0s: v1.0.0\n", "k0s: 1.10\n"))); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Fatalf("a version written as a YAML float was accepted (it reads as 1.1, not 1.10): %v", err)
	}
}

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
	// A resolvable reference needs the repository slash and tag colon.
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || !strings.Contains(ref[:colon], "/") {
		t.Fatalf("authelia pin %q names no repository: it has to be <registry>/<repository>:<tag>@sha256:..., not a bare tag", pin)
	}
	repo, tag := ref[:colon], ref[colon+1:]
	// The repository is Renovate's depName and is version-independent.
	if repo != "docker.io/"+renovateRepo {
		t.Errorf("authelia repository is %q, want docker.io/%s", repo, renovateRepo)
	}
	if tag == "" || strings.ContainsAny(tag, ":@") {
		t.Errorf("authelia tag %q is not a bare tag", tag)
	}
	if strings.HasPrefix(pin, tag+"@") {
		t.Fatalf("authelia pin %q starts with the version: the repository is missing", pin)
	}
	// Renovate reads the docker comment and reference from one line.
	if !regexp.MustCompile(`(?m)^authelia: "` + regexp.QuoteMeta(pin) + `"\s+# renovate: datasource=docker depName=` + regexp.QuoteMeta(renovateRepo) + `$`).Match(shippedFile(t)) {
		t.Errorf("the authelia line in charts/s5n/%s is not a quoted pin with a docker datasource comment:\n%s", PlatformFile, shippedFile(t))
	}
}

// renovateRepo is the login image Renovate watches.
const renovateRepo = "authelia/authelia"

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
