package platform

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
)

// AtPin reports whether a deployed chart version is the pinned one.
//
// A Helm release stores the chart version it was installed from verbatim, and
// charts publish both spellings: Envoy Gateway tags "1.9.3", cert-manager tags
// "v1.21.3". So the comparison is the semver one and never the string: a
// release at its pin is verified, and only a real version difference is an
// upgrade (a Helm revision, plus a chart pull, per bootstrap). The pins used to
// carry a "v" by hand so that a string compare would happen to hold; they no
// longer need to.
//
// semver's Equal covers the precedence, and deliberately ignores build metadata
// ("v1.37.2+k0s.1" and "v1.37.2+k0s.2" compare equal), so the metadata is
// compared explicitly: k0s's tag is exactly that shape, and a rebuilt k0s is a
// different release.
func AtPin(installed, pinned string) bool {
	want, err := semver.NewVersion(pinned)
	if err != nil {
		// A pin that is not a version is box's own bug; no deployed release
		// can be at it, and a test pins every entry in the file as semver.
		return false
	}
	got, err := semver.NewVersion(installed)
	if err != nil {
		// An unreadable deployed version is not at the pin: box plans the
		// upgrade, exactly as it did when the comparison was a string.
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
