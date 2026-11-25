package wiring

// seerr_test.go is the wiring's Seerr row: it is a to-do only for a Seerr with no
// admin, and it names the address a person opens Seerr at -- which is Seerr's own
// host, not the /seerr path an older release routed it under.

import (
	"context"
	"strings"
	"testing"
)

// A Seerr nobody has signed in to yet has no admin, and the only way to make one
// is an interactive Plex sign-in through a browser. The row asks for that once,
// at the address the chart serves Seerr on: its own host, because its bundle
// loads its assets from the site root.
func TestWiringSeerrRowNamesTheAddressPeopleUse(t *testing.T) {
	r, f := newMediaHarness(t, true)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if f.initialized {
		t.Fatal("the harness's Seerr is initialized: this test is about a box with no admin")
	}
	s := step(res, "seerr/plex")
	if s == nil || s.Result != "pending" {
		t.Fatalf("seerr/plex = %+v, want pending", s)
	}
	const want = "Seerr has no admin account yet: sign in once at " + mediaSeerrPublicURL + " with Plex to let the wiring Job take over"
	if s.Detail != want {
		t.Fatalf("seerr/plex detail = %q, want %q", s.Detail, want)
	}
	if strings.Contains(s.Detail, "/seerr") {
		t.Fatalf("seerr/plex detail = %q: it names a path, and Seerr is served at its own host", s.Detail)
	}
}

// A migrated Seerr brings its admin in the settings the snapshot carried, and
// there is nothing left for the wiring Job to ask of it: the row is not printed
// at all, because a box with a signed-in Seerr has no to-do there.
func TestWiringSeerrWithAnAdminPrintsNoRow(t *testing.T) {
	r, f := newMediaHarness(t, true)
	f.initialized = true
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "seerr/plex"); s != nil {
		t.Fatalf("seerr/plex = %+v, want no row: Seerr has its admin", s)
	}
	if s := step(res, "seerr/ready"); s == nil || s.Result != "ok" {
		t.Fatalf("seerr/ready = %+v, want ok", s)
	}
	if strings.Contains(Summary(res), "seerr/plex") {
		t.Fatalf("the receipt still lists a seerr/plex row:\n%s", Summary(res))
	}
}

// The readiness wait and the admin question are one endpoint, so a Seerr that
// answers readiness and then refuses the read is a failed step, not a silent
// one: the row this run would print depends on the answer.
func TestWiringSeerrSettingsFailureStopsTheRun(t *testing.T) {
	r, f := newMediaHarness(t, true)
	// Readiness passes (the first call), and the read the row depends on is
	// refused (every call after it): the run stops there rather than printing a
	// to-do or dropping the row on an answer it never got.
	f.seerrFailFrom = 2
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded without reading Seerr's settings\n%s", Summary(res))
	}
	if s := step(res, "seerr/ready"); s == nil || s.Result != "ok" {
		t.Fatalf("seerr/ready = %+v, want ok", s)
	}
	s := step(res, "seerr/settings")
	if s == nil || s.Result != "failed" {
		t.Fatalf("seerr/settings = %+v, want failed", s)
	}
	if x := step(res, "seerr/plex"); x != nil {
		t.Fatalf("seerr/plex = %+v, want no row: the answer was never read", x)
	}
}

// The address is part of the rendered config, not a guess: a Seerr entry with no
// publicUrl is refused before the Job starts, the same way an empty url is.
func TestSeerrConfigValidationRequiresThePublicAddress(t *testing.T) {
	cfg := Config{Seerr: &SeerrConfig{URL: "http://seerr:5055"}}
	err := cfg.validate()
	if err == nil {
		t.Fatal("a seerr block without publicUrl was accepted")
	}
	if !strings.Contains(err.Error(), "publicUrl") {
		t.Fatalf("the refusal does not name publicUrl: %v", err)
	}
}
