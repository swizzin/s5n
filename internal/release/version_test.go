package release

import (
	"errors"
	"strings"
	"testing"
)

// errTest is the registry failing for one release.
var errTest = errors.New("registry said no")

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

	got, skipped, err := SelectVersion([]string{"0.9.0", "0.1.0", "0.10.0", "0.2.0", "latest", "0.3.0-rc.1"}, "0.5.0", minBoxOf, nil)
	if err != nil {
		t.Fatalf("selectVersion: %v", err)
	}
	if got != "0.9.0" {
		t.Fatalf("selected %q, want 0.9.0: 0.10.0 needs box 9.0.0", got)
	}
	if len(skipped) != 1 || skipped[0].Version != "0.10.0" || skipped[0].MinBox != "9.0.0" {
		t.Fatalf("skipped = %+v, want exactly 0.10.0 (needs 9.0.0)", skipped)
	}

	// Newest by semver: 0.10.0 sorts after 0.9.0.
	got, skipped, err = SelectVersion([]string{"0.9.0", "0.10.0"}, "9.0.0", minBoxOf, nil)
	if err != nil {
		t.Fatalf("selectVersion: %v", err)
	}
	if got != "0.10.0" || len(skipped) != 0 {
		t.Fatalf("selected %q (skips %+v), want 0.10.0 with no skips", got, skipped)
	}

	// Only prereleases: box never installs an rc unasked.
	_, _, err = SelectVersion([]string{"0.3.0-rc.1", "0.3.0-rc.2"}, "0.5.0", minBoxOf, nil)
	if err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Fatalf("error = %v, want a refusal when only prereleases exist", err)
	}

	// Every release needs a newer box, so the refusal says update box.
	_, skipped, err = SelectVersion([]string{"0.10.0"}, "0.1.0", minBoxOf, nil)
	if err == nil || !strings.Contains(err.Error(), "needs a newer box than 0.1.0") {
		t.Fatalf("error = %v, want it to name this box", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want the one release it passed over", skipped)
	}

	// An empty repository differs from every release being too new.
	_, _, err = SelectVersion(nil, "0.1.0", minBoxOf, nil)
	if err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Fatalf("error = %v, want a refusal for an empty repository", err)
	}
}

func TestSelectVersionSkipsAFailedRelease(t *testing.T) {
	floors := map[string]string{"0.4.0": "", "0.5.0": "", "0.6.0": ""}
	minBoxOf := func(version string) (string, error) { return floors[version], nil }
	exclude := func(version string) bool { return version == "0.6.0" }

	got, _, err := SelectVersion([]string{"0.4.0", "0.5.0", "0.6.0"}, "0.5.0", minBoxOf, exclude)
	if err != nil {
		t.Fatalf("SelectVersion: %v", err)
	}
	if got != "0.5.0" {
		t.Fatalf("selected %q, want 0.5.0: 0.6.0 failed on this box", got)
	}

	_, _, err = SelectVersion([]string{"0.6.0"}, "0.5.0", minBoxOf, exclude)
	if err == nil || !strings.Contains(err.Error(), "already failed on this box") {
		t.Fatalf("error = %v, want a refusal that names the failures", err)
	}
}

func TestSelectVersionDoesNotFallThroughAFailure(t *testing.T) {
	_, _, err := SelectVersion([]string{"0.2.0", "0.1.0"}, "0.5.0", func(v string) (string, error) {
		if v == "0.2.0" {
			return "", errTest
		}
		return "", nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "registry said no") {
		t.Fatalf("error = %v, want the pull failure", err)
	}
}
