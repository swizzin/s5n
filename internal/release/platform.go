package release

import (
	"bytes"
	"fmt"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// PlatformFile is every version box installs, inside the release
// (docs/box.md, "The platform layer"): nothing about the platform is compiled
// in, and one release installs one platform.
const PlatformFile = "platform.yaml"

// Versions is one pin per component, parsed from this release's platform.yaml,
// never a global: a release and the platform it installs cannot disagree.
type Versions struct {
	// Authelia is root's login image, tag and digest: box writes the Deployment,
	// so nothing else would pin the bytes.
	Authelia string
	// K0s is the release tag its download URL is built from.
	K0s string
	// K0sSHA256 is the release asset's sha256 per GOARCH, verified before box
	// runs it as root. Both amd64 and arm64 are required: pinning one would
	// install the other unchecked.
	K0sSHA256 map[string]string
	// EnvoyGateway is the chart's version, and with it the Gateway API CRD set.
	EnvoyGateway string
	// MetalLB is the chart's version in L2 mode; the upstream "v" tag is derived.
	MetalLB string
	// CertManager is the cert-manager chart's version.
	CertManager string
}

// k0sSums is each checksum key and its architecture; a list so a missing one
// is refused on a stable name.
var k0sSums = []struct{ Key, Arch string }{
	{"k0sSHA256amd64", "amd64"},
	{"k0sSHA256arm64", "arm64"},
}

// ParseVersions accepts exactly the five pins and two checksums: a typo would
// leave a component at zero and install nothing. The one parser: chartFrom,
// hack/versions and the tests all come through it.
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
		// A version must be a string: 1.10 is 1.1 to a YAML number reader.
		value, ok := raw.(string)
		if !ok {
			return Versions{}, fmt.Errorf("%q is a %T, not a string: quote it", key, raw)
		}
		if strings.TrimSpace(value) == "" {
			return Versions{}, fmt.Errorf("%q is empty", key)
		}
		*dst = strings.TrimSpace(value)
	}
	// A checksum that is not a sha256 verifies nothing, and would install a
	// root-run binary against a value no archive matches.
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

// isSHA256 is 64 lowercase hex characters; uppercase is refused, so the file
// stays byte-identical to the published sha256sums.txt line.
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

// platformPins refuses a missing or damaged platform.yaml at load time: box has
// no fallback, because a compiled-in pin would be a second place the platform
// lives.
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
