package release

// The chart generates the Jellyfin password and preserves it by lookup; box's
// only job is to show it on the run that made it, so it reads the key before
// (absent) and after (present) and never prints a later run's.

import (
	"context"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// chartSecret generates the key on the first cast and preserves it after.
type chartSecret struct {
	data      map[string]string
	installed *fakeRelease
	generate  map[string]string
}

func (s *chartSecret) Get(_ context.Context, key string) (string, error) {
	if s.installed.upgrades > 0 {
		if v, ok := s.generate[key]; ok {
			s.data[key] = v
		}
	}
	return s.data[key], nil
}

func (s *chartSecret) Set(_ context.Context, key, value string) error {
	s.data[key] = value
	return nil
}

// jellyfinBox has Jellyfin on and no release yet.
func jellyfinBox(t *testing.T) (*fakes, *chartSecret) {
	t.Helper()
	f := newFakes(t)
	rewrite(t, f.grantPath, map[string]string{
		"  sonarr:\n    enabled: true": "  sonarr:\n    enabled: true\n  jellyfin:\n    enabled: true",
	})
	secret := &chartSecret{
		data:      map[string]string{},
		installed: f.rel,
		generate:  map[string]string{"jellyfin-user-password": "generated-person-password"},
	}
	f.secret = secret
	return f, secret
}

func TestTheJellyfinPasswordIsShownByTheApplyThatMakesIt(t *testing.T) {
	f, _ := jellyfinBox(t)
	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.FirstInstall {
		t.Fatal("the fixture's first apply was not an install")
	}
	if res.JellyfinPassword != "generated-person-password" {
		t.Errorf("JellyfinPassword = %q, want the Secret's generated value", res.JellyfinPassword)
	}
	if res.JellyfinLogin != "alice" {
		t.Errorf("JellyfinLogin = %q, want the grant's login", res.JellyfinLogin)
	}
}

func TestALaterApplyNeverShowsAStoredJellyfinPassword(t *testing.T) {
	f, secret := jellyfinBox(t)
	if _, err := f.apply(t); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// The second run is the same box with unchanged values.
	f.rel.exists = true
	f.store.nsExists = true
	f.store.nsEnforce = podSecurityRestricted
	for _, claim := range []string{"testbox-s5n-data", "testbox-s5n-config"} {
		f.store.existing[claim] = true
	}
	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.JellyfinPassword != "" {
		t.Errorf("a later apply showed %q again; the password is the Secret's and the person already has it", res.JellyfinPassword)
	}
	if len(res.Changes) != 0 {
		t.Errorf("the second apply changed %s", strings.Join(changePaths(res.Changes), ", "))
	}
	if secret.data["jellyfin-user-password"] != "generated-person-password" {
		t.Errorf("the Secret's key moved to %q: a re-run rewrote the person's password", secret.data["jellyfin-user-password"])
	}
}

// changePaths is a diff's paths, for a failure message.
func changePaths(changes []seedbox.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Path
	}
	return out
}
