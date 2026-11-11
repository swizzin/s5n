package bootstrap

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/release/releasetest"
)

// pins is the platform pins of the chart in this checkout: the k0s the release
// names, read from charts/s5n/platform.yaml through the parser box uses.
var pins = releasetest.MustPins()

func TestK0sPinsAreReal(t *testing.T) {
	if len(k0sSHA256) == 0 {
		t.Fatal("no k0s checksums pinned")
	}
	for version, sums := range k0sSHA256 {
		for arch, sum := range sums {
			if len(sum) != 64 {
				t.Errorf("%s/%s: checksum %q is not 64 hex characters", version, arch, sum)
			}
			if _, err := hex.DecodeString(sum); err != nil {
				t.Errorf("%s/%s: checksum %q is not hex: %v", version, arch, sum, err)
			}
			if sum != strings.ToLower(sum) {
				t.Errorf("%s/%s: checksum %q must be lowercase, as sha256sums.txt writes it", version, arch, sum)
			}
			rel, err := k0sReleaseFor(version, arch)
			if err != nil {
				t.Fatalf("%s/%s: %v", version, arch, err)
			}
			if rel.Version != version {
				t.Errorf("%s/%s: release version = %q, want %q", version, arch, rel.Version, version)
			}
		}
	}
	// The chart pins one k0s, and box must carry the sums for it: a release
	// that bumps the pin without the checksums installs nothing, loudly.
	if _, ok := k0sSHA256[pins.K0s]; !ok {
		t.Errorf("the chart pins k0s %s, and box has no checksum for it", pins.K0s)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, ok := k0sSHA256[pins.K0s][arch]; !ok {
			t.Errorf("k0s %s has no checksum for %s; box documents amd64 and arm64 as supported", pins.K0s, arch)
		}
	}
}

func TestK0sReleaseForRefusesUnpinnedArch(t *testing.T) {
	if _, err := k0sReleaseFor(pins.K0s, "riscv64"); err == nil {
		t.Fatal("an unpinned architecture resolved a release")
	}
}

// The version is the chart's, so a chart that pins a k0s this box carries no
// sum for is refused by name rather than downloaded unchecked: the checksum is
// the half of that promise box keeps.
func TestK0sReleaseForRefusesAVersionWithoutAChecksum(t *testing.T) {
	_, err := k0sReleaseFor("v9.9.9+k0s.9", runtime.GOARCH)
	if err == nil {
		t.Fatal("a k0s release with no checksum resolved")
	}
	if !strings.Contains(err.Error(), "v9.9.9+k0s.9") || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %q, want it to name the release and the missing checksum", err)
	}
}

func TestK0sReleaseURL(t *testing.T) {
	rel := k0sRelease{Version: "v9.9.9+k0s.7", Arch: "arm64"}
	want := "https://github.com/k0sproject/k0s/releases/download/v9.9.9+k0s.7/k0s-v9.9.9+k0s.7-arm64"
	if got := rel.URL(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestK0sPlanCarriesThePinAndTheChoice(t *testing.T) {
	plan := k0sPlan(pins)
	if plan.Version != pins.K0s {
		t.Errorf("version = %q, want the release's pin %q", plan.Version, pins.K0s)
	}
	if !plan.CleanHost {
		t.Error("a plan built for the clean-host branch must say so")
	}
	if plan.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", plan.Arch, runtime.GOARCH)
	}
	if sum, ok := k0sSHA256[pins.K0s][runtime.GOARCH]; ok && plan.SHA256 != sum {
		t.Errorf("sha256 = %q, want the pin %q", plan.SHA256, sum)
	}
	// A plan for an architecture box has no sum for still names the version,
	// so the installer can refuse it there rather than on an empty plan.
	if _, ok := k0sSHA256[pins.K0s][runtime.GOARCH]; !ok && plan.SHA256 != "" {
		t.Errorf("no pin for %s but the plan carries %q", runtime.GOARCH, plan.SHA256)
	}

	// The version is a value passed in, never a constant: a release that pins
	// another k0s moves the plan with it.
	other := k0sPlan(release.Versions{K0s: "v9.9.9+k0s.9"})
	if other.Version != "v9.9.9+k0s.9" {
		t.Errorf("version = %q, want the release's %q", other.Version, "v9.9.9+k0s.9")
	}
	if other.SHA256 != "" {
		t.Errorf("a k0s box carries no checksum for got one anyway: %q", other.SHA256)
	}
}

func TestK0sStatePresent(t *testing.T) {
	dir := t.TempDir()
	if k0sStatePresent(dir) {
		t.Error("an empty directory must not read as a k0s install")
	}
	if k0sStatePresent(filepath.Join(dir, "missing")) {
		t.Error("a missing directory must not read as a k0s install")
	}
	if err := os.WriteFile(filepath.Join(dir, "k0s.yaml"), []byte("apiVersion: k0s.k0sproject.io/v1beta1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !k0sStatePresent(dir) {
		t.Error("a state directory with content must read as a k0s install")
	}
}

func TestInstallRefusal(t *testing.T) {
	clean := InstallPlan{Version: pins.K0s, CleanHost: true}
	cases := []struct {
		name     string
		plan     InstallPlan
		root     bool
		findings []Finding
		want     string
	}{
		{name: "root, clean host, no evidence", plan: clean, root: true, want: ""},
		{name: "not root", plan: clean, root: false, want: "as root"},
		{
			name: "the decision did not choose to install",
			plan: InstallPlan{Version: pins.K0s},
			root: true,
			want: "did not choose to install",
		},
		{
			name: "k0s already present",
			plan: clean,
			root: true,
			findings: []Finding{
				{Kind: "binary", Name: K0sBinaryPath},
				{Kind: "port", Name: "6443"},
			},
			want: "never installs over a k0s it did not create",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := installRefusal(tc.plan, tc.root, tc.findings)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("refusal = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("refusal = %q, want it to contain %q", got, tc.want)
			}
		})
	}
	// The undo instruction belongs in the refusal: an operator who is told no
	// needs to know what to remove first, not to guess.
	got := installRefusal(clean, true, []Finding{{Kind: "state", Name: K0sStateDir}})
	if !strings.Contains(got, "k0s stop; k0s reset") {
		t.Errorf("refusal = %q, want the undo command named", got)
	}
}
