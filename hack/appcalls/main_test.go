package main

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/catalog"
)

// TestPairsAreTheEnabledEdges: the pairs are the catalog's own edges, both ends
// enabled, and nothing else -- a caller that is off contributes nothing even
// though its entry still names callees.
func TestPairsAreTheEnabledEdges(t *testing.T) {
	cat, err := catalog.Load("../../charts/s5n")
	if err != nil {
		t.Fatalf("load the chart's catalog: %v", err)
	}
	enabled := map[string]bool{"seerr": true, "plex": true, "sonarr": true, "radarr": true}
	var got []string
	for _, edge := range pairs(cat, enabled) {
		got = append(got, strings.Join(edge[:], ">"))
	}
	want := "seerr>plex,seerr>sonarr,seerr>radarr"
	if strings.Join(got, ",") != want {
		t.Fatalf("pairs = %v, want %s", got, want)
	}
	// Maintainerr is off in this release, so neither its own edges nor the
	// peers it would have called appear.
	for _, edge := range got {
		if strings.Contains(edge, "maintainerr") {
			t.Fatalf("an app that is off contributed %s", edge)
		}
	}
}
