package platformassets

import (
	"strings"
	"testing"
)

// The file is the only home for the pins, so the shipped bytes must already be
// the four values box installs -- this is the test that fails when someone adds
// a component to the parser and forgets the file, or the other way round.
func TestEmbeddedVersions(t *testing.T) {
	for key, got := range map[string]string{
		"k0s":          Pins.K0s,
		"envoyGateway": Pins.EnvoyGateway,
		"metallb":      Pins.MetalLB,
		"certManager":  Pins.CertManager,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s: no pin in the embedded file", key)
		}
		if strings.ContainsAny(got, " \t\"'") {
			t.Errorf("%s: %q is not a bare version", key, got)
		}
	}
	// The values are the ones in the file, not defaults compiled into Go.
	for _, pin := range []string{Pins.K0s, Pins.EnvoyGateway, Pins.MetalLB, Pins.CertManager} {
		if !strings.Contains(string(File), pin) {
			t.Errorf("%q is not in the embedded file:\n%s", pin, File)
		}
	}
}

// A missing, empty or unknown key is an error, not a zero value: box would
// otherwise plan an install at version "".
func TestParseVersionsRefusesADamagedFile(t *testing.T) {
	full := "k0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n"
	files := map[string]struct {
		doc  string
		want string
	}{
		"a missing key": {
			doc:  "k0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\n",
			want: `no "certManager"`,
		},
		"an empty value": {
			doc:  "k0s: v1.0.0\nenvoyGateway: 1.0.0\nmetallb: \"\"\ncertManager: v1.0.0\n",
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
}

// A value typed as a YAML number instead of a string is refused rather than
// silently rounded: a bare 1.10 is 1.1 to a YAML parser, and a chart version
// is not a number.
func TestParseVersionsKeepsTheVersionAsWritten(t *testing.T) {
	v, err := parseVersions([]byte("k0s: v1.37.2+k0s.1\nenvoyGateway: \"1.9.3\"\nmetallb: 0.16.1\ncertManager: v1.21.3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v.K0s != "v1.37.2+k0s.1" || v.EnvoyGateway != "1.9.3" || v.MetalLB != "0.16.1" || v.CertManager != "v1.21.3" {
		t.Fatalf("parsed %+v", v)
	}
	if _, err := parseVersions([]byte("k0s: 1.10\nenvoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n")); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Fatalf("a version written as a YAML float was accepted (it reads as 1.1, not 1.10): %v", err)
	}
}
