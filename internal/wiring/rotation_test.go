package wiring

import (
	"context"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// stepDetails is the receipt's steps as name -> detail, which for an owned
// object is created, updated, unchanged or skipped: the exact thing a run did to
// it, rather than only how many objects moved.
func stepDetails(res *Result) map[string]string {
	out := make(map[string]string, len(res.Steps))
	for _, s := range res.Steps {
		out[s.Name] = s.Detail
	}
	return out
}

// updatedSteps is every step the run recorded as updated, which is the set of
// copies of a rotated secret the Job found stale.
func updatedSteps(res *Result) []string {
	var out []string
	for _, s := range res.Steps {
		if s.Detail == "updated" {
			out = append(out, s.Name)
		}
	}
	return out
}

func assertOnlyUpdated(t *testing.T, res *Result, want ...string) {
	t.Helper()
	got := updatedSteps(res)
	if len(got) != len(want) {
		t.Fatalf("updated steps = %v, want %v\n%s", got, want, Summary(res))
	}
	seen := map[string]bool{}
	for _, name := range got {
		seen[name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("updated steps = %v, want %v\n%s", got, want, Summary(res))
		}
	}
}

// A rotated Arr API key is copied into two apps -- Prowlarr's application for
// that Arr, and autobrr's client for it -- and both are invisible on read
// (masked), so both need the fingerprint. The other Arr's copies stay put.
func TestWiringFollowsOneArrKeyRotation(t *testing.T) {
	h := newHarness(t)
	fakes := h.withApps(t)
	ctx := context.Background()

	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	h.write(t, "sonarr.key", "sonarr-key-2")
	res, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("rotated run: %v", err)
	}
	if res.Writes.Created != 0 {
		t.Fatalf("rotated run created %d objects\n%s", res.Writes.Created, Summary(res))
	}
	assertOnlyUpdated(t, res, "prowlarr/application/sonarr", "autobrr/downloader/sonarr")

	// Prowlarr's application for Sonarr carries the new key; the one for Radarr
	// was not rewritten at all.
	sonarrApp := prowlarrApplication(t, h.stack, "sonarr")
	if got, _ := sonarrApp.FieldValue("apiKey"); got != "sonarr-key-2" {
		t.Fatalf("prowlarr's sonarr application holds %v, want the new key", got)
	}
	if got := stepDetails(res)["prowlarr/application/radarr"]; got != "unchanged" {
		t.Fatalf("prowlarr/application/radarr = %s, want unchanged", got)
	}
	if got := stepDetails(res)["radarr/downloadclient"]; got != "unchanged" {
		t.Fatalf("radarr/downloadclient = %s, want unchanged: it holds the qBittorrent password", got)
	}

	// autobrr's Sonarr client got the new key; its Radarr client kept the old
	// one, which is the proof the rotation is per Arr and not a rewrite of
	// everything.
	settings, _ := autobrrClient(t, fakes, "sonarr")["settings"].(map[string]any)
	if got, _ := settings["apikey"].(string); got != "sonarr-key-2" {
		t.Fatalf("autobrr's sonarr client holds %q, want the new key", got)
	}
	settings, _ = autobrrClient(t, fakes, "radarr")["settings"].(map[string]any)
	if got, _ := settings["apikey"].(string); got != "radarr-key" {
		t.Fatalf("autobrr's radarr client holds %q, want its own unchanged key", got)
	}
}

// autobrrClient is one autobrr download client by name, from the fake's store.
func autobrrClient(t *testing.T, f *appFakes, name string) apps.Item {
	t.Helper()
	for _, row := range f.autoRows {
		if row.Str("name") == name {
			return row
		}
	}
	t.Fatalf("autobrr has no client named %s: %+v", name, f.autoRows)
	return nil
}

// prowlarrApplication is one application by name, from the fake's store.
func prowlarrApplication(t *testing.T, f *fakeStack, name string) apps.Item {
	t.Helper()
	for _, row := range f.apps {
		if row.Str("name") == name {
			return row
		}
	}
	t.Fatalf("prowlarr has no application named %s: %+v", name, f.apps)
	return nil
}
