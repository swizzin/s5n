package main

import "testing"

// A dropped name is worth Seerr's note only when it looks like one of its instances: box
// names one `seerr`, or `seerr-<suffix>` before the domain.
func TestASeerrHostIsRecognisedByName(t *testing.T) {
	for host, want := range map[string]bool{
		"seerr.box.example.com":          true,
		"seerr-jellyfin.box.example.com": true,
		"requests.box.example.com":       false,
		"plex.box.example.com":           false,
		"box.example.com":                false,
	} {
		if got := seerrName(host); got != want {
			t.Fatalf("seerrName(%q) = %v, want %v", host, got, want)
		}
	}
}
