package release

import (
	"bytes"
	"fmt"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// PlatformFile is the chart file every version box installs lives in, at the
// chart's root beside values.yaml and variants.yaml (docs/box.md, "The platform
// layer"). It ships inside the release, so box reads it from the chart it
// pulled: nothing about the platform is compiled into the binary, and one
// release always installs one platform.
const PlatformFile = "platform.yaml"

// Versions is one pin per component box installs. It is the parsed
// platform.yaml of the chart release in hand, never a global: bootstrap
// installs k0s at the release's pin, the platform layer installs its charts at
// the same release's pins, and the login is served at its image, so a chart
// release and the platform it installs cannot disagree.
type Versions struct {
	// Authelia is root's login: an image reference, tag and digest together,
	// because the login is box's own manifest and never a chart's. It is the
	// one pin here that is not a chart version, which is why it carries its
	// digest: box writes the Deployment, so nothing else would pin the bytes.
	Authelia string
	// K0s is the k0s release box installs on a clean host: the release tag,
	// because that is what its download URL is built from.
	K0s string
	// K0sSHA256 is the sha256 of that release's own asset, per GOARCH: what
	// box downloads onto a clean host and then runs as root. It is keyed by
	// architecture because that is how box asks -- the installer refuses the
	// architectures it has no sum for -- so an architecture is one lookup and
	// not a field every caller has to know exists. Both k0s's architectures
	// are required: box documents amd64 and arm64, and a file that pinned one
	// would install the other unchecked.
	K0sSHA256 map[string]string
	// EnvoyGateway is the Envoy Gateway chart's version. The chart ships the
	// Gateway API CRDs box depends on, so its version is the CRD set's too.
	EnvoyGateway string
	// MetalLB is the MetalLB chart's version, in L2 mode. The upstream
	// manifest is tagged "v"+this; the tag is derived, never pinned twice.
	MetalLB string
	// CertManager is the cert-manager chart's version.
	CertManager string
}

// k0sSums is every key platform.yaml carries a k0s checksum under, and the
// architecture that checksum is of. A list rather than a map, so a file that is
// missing both is refused on a name that does not move between runs.
var k0sSums = []struct{ Key, Arch string }{
	{"k0sSHA256amd64", "amd64"},
	{"k0sSHA256arm64", "arm64"},
}

// ParseVersions reads platform.yaml with the YAML decoder box already uses for
// the manifests it applies, and accepts exactly the five pins and the two k0s
// checksums: a typo'd key would otherwise leave a component at the zero value,
// and box would plan an install of nothing.
//
// It is the one parser: a loaded release (chartFrom), hack/versions and the
// tests all come through here, so the file box installs from and the file the
// e2e reads cannot disagree about what a pin is.
func ParseVersions(b []byte) (Versions, error) {
	var doc map[string]any
	if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096).Decode(&doc); err != nil {
		return Versions{}, fmt.Errorf("read: %w", err)
	}
	var v Versions
	pins := map[string]*string{
		"authelia":     &v.Authelia,
		"k0s":          &v.K0s,
		"envoyGateway": &v.EnvoyGateway,
		"metallb":      &v.MetalLB,
		"certManager":  &v.CertManager,
	}
	for key, dst := range pins {
		raw, ok := doc[key]
		if !ok {
			return Versions{}, fmt.Errorf("no %q: every version box installs lives in this file", key)
		}
		// A version is a string, never a YAML number: 1.10 is 1.1 to a YAML
		// reader, and a chart version that loses a digit is one nobody
		// publishes.
		value, ok := raw.(string)
		if !ok {
			return Versions{}, fmt.Errorf("%q is a %T, not a string: quote it", key, raw)
		}
		if strings.TrimSpace(value) == "" {
			return Versions{}, fmt.Errorf("%q is empty", key)
		}
		*dst = strings.TrimSpace(value)
	}
	// The checksums are not versions and are checked as what they are: a sum
	// that is not a sha256 verifies nothing, and box would install a root-run
	// binary against a value no archive can ever match.
	v.K0sSHA256 = make(map[string]string, len(k0sSums))
	for _, s := range k0sSums {
		raw, ok := doc[s.Key]
		if !ok {
			return Versions{}, fmt.Errorf("no %q: box verifies the k0s it installs, so the checksum of the release above lives here beside it", s.Key)
		}
		value, ok := raw.(string)
		if !ok {
			return Versions{}, fmt.Errorf("%q is a %T, not a string: quote it", s.Key, raw)
		}
		sum := strings.TrimSpace(value)
		if !isSHA256(sum) {
			return Versions{}, fmt.Errorf("%q is %q, not a sha256: 64 lowercase hex characters, as the release's sha256sums.txt writes it", s.Key, sum)
		}
		v.K0sSHA256[s.Arch] = sum
	}
	for key := range doc {
		if _, ok := pins[key]; ok {
			continue
		}
		if _, ok := k0sSumFor(key); ok {
			continue
		}
		return Versions{}, fmt.Errorf("unknown key %q: the parser and this file disagree about what box installs", key)
	}
	return v, nil
}

// k0sSumFor is the architecture a platform.yaml key holds a k0s checksum for.
func k0sSumFor(key string) (arch string, ok bool) {
	for _, s := range k0sSums {
		if s.Key == key {
			return s.Arch, true
		}
	}
	return "", false
}

// isSHA256 is a checksum as the release publishes it: 64 lowercase hex
// characters. Uppercase is refused rather than lowercased, so the file stays
// byte-identical to the sha256sums.txt line a maintainer copies from.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// platformPins is a chart release's platform.yaml, read the way every other
// chart file is read. A release that ships none, or one whose pins are damaged,
// is refused here, when the release is loaded and long before anything is
// installed: box has no fallback version to install, because a compiled-in pin
// would be a second place the platform lives.
func platformPins(name, version string, file []byte) (Versions, error) {
	if len(file) == 0 {
		return Versions{}, fmt.Errorf("chart %s %s has no %s: box reads the platform pins from the release it installs, so it cannot install one that ships none",
			name, version, PlatformFile)
	}
	pins, err := ParseVersions(file)
	if err != nil {
		return Versions{}, fmt.Errorf("chart %s %s: %s: %w", name, version, PlatformFile, err)
	}
	return pins, nil
}
