// Package platformassets embeds the versions every platform component is
// installed at. versions.yaml beside this file is the only home for them: box
// compiles the same bytes in, and the e2e scripts read them through
// hack/versions (which reads this file's embedded copy), so the platform the
// e2e accepts is the platform box installs.
//
// embed.go lives here rather than in a package directory of its own, because
// an embed directive cannot reach a sibling tree: the file has to sit in the directory
// that embeds it, and its path is the one docs/box.md names.
package platformassets

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// File is install/platform/versions.yaml.
//
//go:embed versions.yaml
var File []byte

// Versions is one pin per component box installs.
type Versions struct {
	// K0s is the k0s release box installs on a clean host: the release tag,
	// because that is what its download URL is built from.
	K0s string
	// EnvoyGateway is the Envoy Gateway chart's version. The chart ships the
	// Gateway API CRDs box depends on, so its version is the CRD set's too.
	EnvoyGateway string
	// MetalLB is the MetalLB chart's version, in L2 mode. The upstream
	// manifest is tagged "v"+this; the tag is derived, never pinned twice.
	MetalLB string
	// CertManager is the cert-manager chart's version.
	CertManager string
}

// Pins is the parsed file, read once at init. The bytes are compiled in, so a
// bad file is a bug in the release rather than anything an operator can cause:
// an init panic names the offending line instead of letting box install an
// empty version.
var Pins = mustParse(File)

func mustParse(b []byte) Versions {
	v, err := parseVersions(b)
	if err != nil {
		panic("install/platform/versions.yaml: " + err.Error())
	}
	return v
}

// parseVersions reads the file with the YAML decoder box already uses for the
// manifests it applies, and accepts exactly the four pins: a typo'd key would
// otherwise leave a component at the zero value, and box would plan an install
// of nothing.
func parseVersions(b []byte) (Versions, error) {
	var doc map[string]any
	if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096).Decode(&doc); err != nil {
		return Versions{}, fmt.Errorf("read: %w", err)
	}
	var v Versions
	pins := map[string]*string{
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
	for key := range doc {
		if _, ok := pins[key]; !ok {
			return Versions{}, fmt.Errorf("unknown key %q: the parser and this file disagree about what box installs", key)
		}
	}
	return v, nil
}
