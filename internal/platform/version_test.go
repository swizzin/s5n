package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"

	platformassets "github.com/swizzin/s5n/install/platform"
	"github.com/swizzin/s5n/internal/bootstrap"
)

// Helm stores the chart version it installed verbatim, so the same release can
// be spelled "1.9.3" (Envoy Gateway's tags) or "v1.21.3" (cert-manager's). The
// comparison is semver, never the string, and build metadata is compared
// explicitly because semver's Equal ignores it.
func TestAtPin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed string
		pinned    string
		want      bool
	}{
		{"equal, no v", "1.9.3", "1.9.3", true},
		{"deployed carries the v", "v0.16.1", "0.16.1", true},
		{"the pin carries the v", "1.9.3", "v1.9.3", true},
		{"both carry it", "v1.21.3", "v1.21.3", true},
		{"different patch", "0.16.0", "0.16.1", false},
		{"different minor, not a string prefix", "0.1.6", "0.16.1", false},
		{"different major", "1.9.3", "2.9.3", false},
		{"a prerelease is not the release", "1.9.3-rc.1", "1.9.3", false},
		{"the same prerelease", "1.9.3-rc.1", "1.9.3-rc.1", true},
		{"k0s metadata, same build", "v1.37.2+k0s.1", "v1.37.2+k0s.1", true},
		{"k0s metadata, another build", "v1.37.2+k0s.2", "v1.37.2+k0s.1", false},
		{"metadata on one side only", "1.37.2", "v1.37.2+k0s.1", false},
		{"metadata, one side with v", "v1.37.2+k0s.1", "1.37.2+k0s.1", true},
		{"garbage deployed", "not-a-version", "0.16.1", false},
		{"empty deployed (a first install)", "", "0.16.1", false},
		{"garbage pin", "0.16.1", "released-yesterday", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AtPin(tc.installed, tc.pinned); got != tc.want {
				t.Fatalf("AtPin(%q, %q) = %v, want %v", tc.installed, tc.pinned, got, tc.want)
			}
		})
	}
}

// The plan says what an upgrade replaces, and a deployed version it cannot read
// is named as such rather than silently upgraded as "a different version".
func TestNotAtPin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed string
		pinned    string
		want      string
	}{
		{"at the pin", "v0.16.1", "0.16.1", ""},
		{"not deployed yet", "", "0.16.1", ""},
		{"another version", "0.15.0", "0.16.1", "upgrading from 0.15.0"},
		{"unreadable deployed", "corrupt", "0.16.1", `the deployed release reports "corrupt", which is not a version box can read`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notAtPin(tc.installed, tc.pinned); got != tc.want {
				t.Fatalf("notAtPin(%q, %q) = %q, want %q", tc.installed, tc.pinned, got, tc.want)
			}
		})
	}
}

// Every pin box ships is a version: the file is data, and a typo in it would
// otherwise reach Helm as a chart version nobody publishes.
func TestEveryPinIsSemver(t *testing.T) {
	pins := map[string]string{
		"k0s":          platformassets.Pins.K0s,
		"envoyGateway": platformassets.Pins.EnvoyGateway,
		"metallb":      platformassets.Pins.MetalLB,
		"certManager":  platformassets.Pins.CertManager,
	}
	for key, pin := range pins {
		if pin == "" {
			t.Errorf("%s: the embedded versions file has no pin for it", key)
			continue
		}
		if _, err := semver.NewVersion(pin); err != nil {
			t.Errorf("%s: %q is not a semver version: %v", key, pin, err)
			continue
		}
		// AtPin treats the pin as the identity, so a pin that parses has to
		// compare equal to itself in both spellings Helm might hand back.
		bare := strings.TrimPrefix(pin, "v")
		for _, spelling := range []string{pin, "v" + bare} {
			if !AtPin(spelling, pin) || !AtPin(pin, spelling) {
				t.Errorf("%s: AtPin does not match %q to the pin %q", key, spelling, pin)
			}
		}
	}
}

// The audit's live failure: a release whose stored chart version carries a "v"
// the pin does not was upgraded on every bootstrap, because the comparison was
// a string. With a semver comparison the plan verifies it and Helm is never
// called.
func TestApplyVerifiesAReleaseWhoseVersionCarriesAV(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{
		"envoy-gateway-system/envoy-gateway": "v" + platformassets.Pins.EnvoyGateway,
		"cert-manager/cert-manager":          platformassets.Pins.CertManager,
		"metallb-system/metallb":             "v" + platformassets.Pins.MetalLB,
	}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	if _, err := f.d.Apply(context.Background(), allInstall); err != nil {
		t.Fatal(err)
	}
	if len(f.rel.installed) != 0 {
		t.Fatalf("Helm was called for releases already at their pin (the v-prefix bug): %+v", f.rel.installed)
	}
	for _, name := range []string{EnvoyGatewayRelease, CertManagerRelease, MetalLBRelease} {
		if !strings.Contains(f.log.String(), name+": already at ") {
			t.Fatalf("%s was not verified in the plan: %q", name, f.log.String())
		}
	}
}

// An unreadable deployed version is not at the pin, so box installs the pin --
// and the plan says the version it found could not be read, rather than
// upgrading quietly.
func TestApplyReplacesAnUnreadableVersion(t *testing.T) {
	f := newFixture()
	f.rel.versions = map[string]string{"metallb-system/metallb": "not-a-version"}
	f.probe.answers = []GatewayAnswer{{Address: "192.0.2.10"}}

	if _, err := f.d.Apply(context.Background(), notProbed(bootstrap.ModeAuto)); err != nil {
		t.Fatal(err)
	}
	var replaced bool
	for _, r := range f.rel.installed {
		if r.Name == MetalLBRelease && r.Version == platformassets.Pins.MetalLB {
			replaced = true
		}
	}
	if !replaced {
		t.Fatalf("box did not install the pin over an unreadable version: %+v", f.rel.installed)
	}
	if !strings.Contains(f.log.String(), `the deployed release reports "not-a-version", which is not a version box can read`) {
		t.Fatalf("the plan did not name the unreadable version: %q", f.log.String())
	}
}
