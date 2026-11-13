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
// names and the checksums of its assets, read from charts/s5n/platform.yaml
// through the parser box uses.
var pins = releasetest.MustPins()

// The sums the chart pins are the only ones box has: nothing about k0s is
// compiled in any more, so each architecture's checksum has to be in that file,
// has to be a sha256, and has to be the one k0sReleaseFor hands the installer.
// A chart that bumps the k0s line without writing its sums installs nothing,
// loudly.
func TestK0sPinsAreReal(t *testing.T) {
	if pins.K0s == "" {
		t.Fatal("the chart pins no k0s version")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		sum, ok := pins.K0sSHA256[arch]
		if !ok || sum == "" {
			t.Fatalf("k0s %s has no checksum for %s: box documents amd64 and arm64 as supported", pins.K0s, arch)
		}
		if len(sum) != 64 {
			t.Errorf("%s: checksum %q is not 64 hex characters", arch, sum)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			t.Errorf("%s: checksum %q is not hex: %v", arch, sum, err)
		}
		if sum != strings.ToLower(sum) {
			t.Errorf("%s: checksum %q must be lowercase, as sha256sums.txt writes it", arch, sum)
		}
		rel, err := k0sReleaseFor(pins, arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if rel.Version != pins.K0s || rel.Arch != arch || rel.SHA256 != sum {
			t.Errorf("k0sReleaseFor(%s) = %+v, want the chart's pin %s with its %s checksum", arch, rel, pins.K0s, arch)
		}
	}
}

// An architecture the file pins no sum for is refused by name, before anything
// is downloaded: box verifies what it installs, and an unchecked root binary is
// the one thing this policy exists to prevent.
func TestK0sReleaseForRefusesUnpinnedArch(t *testing.T) {
	_, err := k0sReleaseFor(pins, "riscv64")
	if err == nil {
		t.Fatal("an unpinned architecture resolved a release")
	}
	if !strings.Contains(err.Error(), `"riscv64"`) || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %q, want it to name the architecture and the missing checksum", err)
	}
}

// A release with no k0s line is refused too: box has no version to install and
// no sum to check it against, and inventing either is what the pins replace.
func TestK0sReleaseForRefusesAVersionWithoutAChecksum(t *testing.T) {
	_, err := k0sReleaseFor(release.Versions{K0s: "v9.9.9+k0s.9"}, runtime.GOARCH)
	if err == nil {
		t.Fatal("a k0s release with no checksum resolved")
	}
	if !strings.Contains(err.Error(), "v9.9.9+k0s.9") || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %q, want it to name the release and the missing checksum", err)
	}
	_, err = k0sReleaseFor(release.Versions{}, runtime.GOARCH)
	if err == nil || !strings.Contains(err.Error(), "pins no k0s version") {
		t.Fatalf("error = %q, want the missing k0s pin named", err)
	}
}

func TestK0sReleaseURL(t *testing.T) {
	rel := k0sRelease{Version: "v9.9.9+k0s.7", Arch: "arm64"}
	want := "https://github.com/k0sproject/k0s/releases/download/v9.9.9+k0s.7/k0s-v9.9.9+k0s.7-arm64"
	if got := rel.URL(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

// testUplink is the host the plan tests install on: ethernet, 1500, the
// address a default route would source.
var testUplink = Uplink{Interface: "eth0", Address: testNodeIP, MTU: 1500}

func TestK0sPlanCarriesThePinAndTheChoice(t *testing.T) {
	plan := k0sPlan(pins, testUplink)
	if plan.Version != pins.K0s {
		t.Errorf("version = %q, want the release's pin %q", plan.Version, pins.K0s)
	}
	if !plan.CleanHost {
		t.Error("a plan built for the clean-host branch must say so")
	}
	if plan.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", plan.Arch, runtime.GOARCH)
	}
	// The checksum is the one pinned beside the version: the plan is not a
	// second place a sum could be written, so it cannot disagree with the file.
	if want := pins.K0sSHA256[runtime.GOARCH]; plan.SHA256 != want {
		t.Errorf("sha256 = %q, want the pin %q", plan.SHA256, want)
	}

	// The version is a value passed in, never a constant: a release that pins
	// another k0s moves the plan with it. Without a sum in that release the
	// plan still names the version, so the installer refuses it by name rather
	// than on an empty plan.
	other := k0sPlan(release.Versions{K0s: "v9.9.9+k0s.9"}, testUplink)
	if other.Version != "v9.9.9+k0s.9" {
		t.Errorf("version = %q, want the release's %q", other.Version, "v9.9.9+k0s.9")
	}
	if other.SHA256 != "" {
		t.Errorf("a release with no checksum got one anyway: %q", other.SHA256)
	}

	// The plan is also where the node identity travels from the decision to
	// the installer: the address, the link it came from, and the config that
	// pins the API address to it.
	if plan.NodeIP != testUplink.Address {
		t.Errorf("node IP = %q, want the uplink's %q", plan.NodeIP, testUplink.Address)
	}
	if plan.Uplink != testUplink {
		t.Errorf("uplink = %+v, want %+v", plan.Uplink, testUplink)
	}
	if plan.ConfigPath != K0sConfigPath {
		t.Errorf("config path = %q, want %q", plan.ConfigPath, K0sConfigPath)
	}
	if got := plan.ConfigText; !strings.Contains(got, "address: "+testUplink.Address) || !strings.Contains(got, "kind: ClusterConfig") {
		t.Errorf("config text = %q, want a k0s ClusterConfig pinning %s", got, testUplink.Address)
	}
}

// A host with no default route has nothing to pin, so the plan names no
// address and the installer refuses it rather than letting the kubelet choose.
func TestK0sPlanWithoutAnUplinkPinsNothing(t *testing.T) {
	plan := k0sPlan(pins, Uplink{})
	if plan.NodeIP != "" || plan.ConfigText != "" {
		t.Errorf("plan = %+v, want no node IP and no config", plan)
	}
	if why := installRefusal(plan, true, nil); !strings.Contains(why, "this plan pins none") {
		t.Errorf("refusal = %q, want the missing pin named", why)
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
	clean := InstallPlan{Version: pins.K0s, CleanHost: true, NodeIP: testNodeIP, ConfigText: k0sNodeConfig(testNodeIP)}
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
			plan: InstallPlan{Version: pins.K0s, NodeIP: testNodeIP, ConfigText: k0sNodeConfig(testNodeIP)},
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
