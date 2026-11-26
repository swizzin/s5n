package platform

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
)

// AtPin reports whether a deployed chart version is the pinned one.
//
// Charts publish both spellings (Envoy Gateway "1.9.3", cert-manager "v1.21.3"), so
// the comparison is semver's, never the string: at the pin is verified, a real
// difference is an upgrade. semver's Equal ignores build metadata ("v1.37.2+k0s.1"
// equals "+k0s.2"), so the metadata is compared explicitly: a rebuilt k0s is a
// different release.
func AtPin(installed, pinned string) bool {
	want, err := semver.NewVersion(pinned)
	if err != nil {
		// A pin that is not a version is box's own bug; a test pins every entry as
		// semver.
		return false
	}
	got, err := semver.NewVersion(installed)
	if err != nil {
		// An unreadable deployed version is not at the pin: box plans the upgrade.
		return false
	}
	return got.Equal(want) && got.Metadata() == want.Metadata()
}

// notAtPin is how the plan explains a deployed release that is not at its pin,
// because a re-run that upgrades should say why. It is empty when the release
// is at the pin and when there is no deployed release yet (a first install).
func notAtPin(installed, pinned string) string {
	if installed == "" {
		return ""
	}
	if _, err := semver.NewVersion(pinned); err != nil {
		return fmt.Sprintf("the pin %q is not a version box can read", pinned)
	}
	if AtPin(installed, pinned) {
		return ""
	}
	if _, err := semver.NewVersion(installed); err != nil {
		return fmt.Sprintf("the deployed release reports %q, which is not a version box can read", installed)
	}
	return "upgrading from " + installed
}
