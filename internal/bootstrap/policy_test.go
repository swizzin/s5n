package bootstrap

import (
	"encoding/hex"
	"runtime"
	"strings"
	"testing"
)

func TestK0sPinsAreReal(t *testing.T) {
	if len(k0sSHA256) == 0 {
		t.Fatal("no k0s checksums pinned")
	}
	for arch, sum := range k0sSHA256 {
		if len(sum) != 64 {
			t.Errorf("%s: checksum %q is not 64 hex characters", arch, sum)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			t.Errorf("%s: checksum %q is not hex: %v", arch, sum, err)
		}
		if sum != strings.ToLower(sum) {
			t.Errorf("%s: checksum %q must be lowercase, as sha256sums.txt writes it", arch, sum)
		}
		rel, err := k0sReleaseFor(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if rel.Version != K0sVersion {
			t.Errorf("%s: release version = %q, want the single pin %q", arch, rel.Version, K0sVersion)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, ok := k0sSHA256[arch]; !ok {
			t.Errorf("%s has no pinned checksum; box documents amd64 and arm64 as supported", arch)
		}
	}
}

func TestK0sReleaseForRefusesUnpinnedArch(t *testing.T) {
	if _, err := k0sReleaseFor("riscv64"); err == nil {
		t.Fatal("an unpinned architecture resolved a release")
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
	plan := k0sPlan()
	if plan.Version != K0sVersion {
		t.Errorf("version = %q, want %q", plan.Version, K0sVersion)
	}
	if !plan.CleanHost {
		t.Error("a plan built for the clean-host branch must say so")
	}
	if plan.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", plan.Arch, runtime.GOARCH)
	}
	if sum, ok := k0sSHA256[runtime.GOARCH]; ok && plan.SHA256 != sum {
		t.Errorf("sha256 = %q, want the pin %q", plan.SHA256, sum)
	}
	// A plan from a binary with no pin still names the version, so the
	// installer can refuse it on the architecture rather than on an empty plan.
	if _, ok := k0sSHA256[runtime.GOARCH]; !ok && plan.SHA256 != "" {
		t.Errorf("no pin for %s but the plan carries %q", runtime.GOARCH, plan.SHA256)
	}
}

func TestInstallRefusal(t *testing.T) {
	clean := InstallPlan{Version: K0sVersion, CleanHost: true}
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
			plan: InstallPlan{Version: K0sVersion},
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
