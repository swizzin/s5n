package release

import (
	"errors"
	"strings"
	"testing"
)

// errTest is the registry failing to answer for one release.
var errTest = errors.New("registry said no")

// TestSelectVersion: which release box installs. Ordering is semver (0.10.0 is
// newer than 0.9.0), anything that is not a version is ignored, prereleases are
// not picked unasked, and a release whose floor this box does not meet is
// skipped with the reason recorded — never silently, and never by falling
// through to an older one when the registry failed to answer.
func TestSelectVersion(t *testing.T) {
	floors := map[string]string{
		"0.1.0":  "",
		"0.2.0":  "0.1.0",
		"0.9.0":  "0.2.0",
		"0.10.0": "9.0.0",
	}
	minBoxOf := func(version string) (string, error) {
		floor, ok := floors[version]
		if !ok {
			t.Fatalf("selectVersion asked about %q, which is not a release", version)
		}
		return floor, nil
	}

	got, skipped, err := selectVersion([]string{"0.9.0", "0.1.0", "0.10.0", "0.2.0", "latest", "0.3.0-rc.1"}, "0.5.0", minBoxOf)
	if err != nil {
		t.Fatalf("selectVersion: %v", err)
	}
	if got != "0.9.0" {
		t.Fatalf("selected %q, want 0.9.0: 0.10.0 needs box 9.0.0", got)
	}
	if len(skipped) != 1 || skipped[0].Version != "0.10.0" || skipped[0].MinBox != "9.0.0" {
		t.Fatalf("skipped = %+v, want exactly 0.10.0 (needs 9.0.0)", skipped)
	}

	// A box that meets everything takes the newest by semver, not by string:
	// "0.10.0" sorts after "0.9.0".
	got, skipped, err = selectVersion([]string{"0.9.0", "0.10.0"}, "9.0.0", minBoxOf)
	if err != nil {
		t.Fatalf("selectVersion: %v", err)
	}
	if got != "0.10.0" || len(skipped) != 0 {
		t.Fatalf("selected %q (skips %+v), want 0.10.0 with no skips", got, skipped)
	}

	// Nothing but prereleases: a tag is how a release is published, and an rc
	// on the list is not one box installs unasked.
	_, _, err = selectVersion([]string{"0.3.0-rc.1", "0.3.0-rc.2"}, "0.5.0", minBoxOf)
	if err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Fatalf("error = %v, want a refusal when only prereleases exist", err)
	}

	// Every release needs a newer box: the refusal names the box, so a user
	// knows the answer is "update box", not "the registry is empty".
	_, skipped, err = selectVersion([]string{"0.10.0"}, "0.1.0", minBoxOf)
	if err == nil || !strings.Contains(err.Error(), "needs a newer box than 0.1.0") {
		t.Fatalf("error = %v, want it to name this box", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want the one release it passed over", skipped)
	}

	// An empty repository is not the same as "every release is too new".
	_, _, err = selectVersion(nil, "0.1.0", minBoxOf)
	if err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Fatalf("error = %v, want a refusal for an empty repository", err)
	}
}

// TestSelectVersionDoesNotFallThroughAFailure: one release's annotations cannot
// be read means the selection stops. Going on to an older release would
// silently install something other than the newest release that exists.
func TestSelectVersionDoesNotFallThroughAFailure(t *testing.T) {
	_, _, err := selectVersion([]string{"0.2.0", "0.1.0"}, "0.5.0", func(v string) (string, error) {
		if v == "0.2.0" {
			return "", errTest
		}
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "registry said no") {
		t.Fatalf("error = %v, want the pull failure", err)
	}
}
