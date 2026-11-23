package wiring

// maintainerr_test.go is item 3 of the receipt's Plex rows: Maintainerr is wired
// to Plex the way Tautulli is, with the release Secret's plex-token, through the
// app's own settings API. The fake answers the shapes apps/maintainerr.go reads
// from v3.33.0, and the harness's Plex accepts exactly the token the Secret
// carries -- so a test can rotate one and watch the wiring notice.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// A fresh Maintainerr gets the release's Plex: the token, then the connection,
// then the app's own test. Manual mode is not cosmetic -- it is what stops
// Maintainerr re-discovering a plex.direct address the pod cannot reach.
func TestWiringConnectsMaintainerrToPlex(t *testing.T) {
	r, f := newMediaHarness(t, true)
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "maintainerr/ready"); s == nil || s.Result != "ok" {
		t.Fatalf("maintainerr/ready = %+v", s)
	}
	if s := step(res, "maintainerr/plex"); s == nil || s.Result != "ok" || s.Detail != "created" {
		t.Fatalf("maintainerr/plex = %+v, want ok/created", s)
	}
	if f.maintainerToken != mediaPlexToken {
		t.Fatalf("Maintainerr holds %q, want the release Secret's plex-token", f.maintainerToken)
	}
	// The release's own Plex address, which is what the other rows use too.
	wantHost, wantPort, err := hostPort(r.Config.Plex.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f.maintainerrSettings.PlexHostname != wantHost || f.maintainerrSettings.PlexPort != wantPort || f.maintainerrSettings.PlexSSL != 0 {
		t.Fatalf("Maintainerr's connection is %+v, want %s:%d over plain HTTP", f.maintainerrSettings, wantHost, wantPort)
	}
	if f.maintainerrSettings.PlexManualMode != 1 || f.maintainerrSettings.MediaServerType != "plex" {
		t.Fatalf("Maintainerr's connection is %+v, want manual mode and the plex media server", f.maintainerrSettings)
	}
	// Nothing about the token reaches the receipt: it is a credential, and the
	// receipt is a ConfigMap anyone in the namespace can read.
	noTokenAnywhere(t, res, mediaPlexToken)
}

// A second run is a no-op: the token is masked by the app, so the fingerprint the
// Job recorded in owned.json is what says the Secret's token is the one already
// there.
func TestWiringMaintainerrSecondRunWritesNothing(t *testing.T) {
	r, f := newMediaHarness(t, true)
	ctx := context.Background()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	saves, patches := f.maintainerrSaves, f.maintainerrPatches
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(res))
	}
	if s := step(res, "maintainerr/plex"); s == nil || s.Detail != "unchanged" {
		t.Fatalf("maintainerr/plex on the second run = %+v, want unchanged", s)
	}
	if f.maintainerrSaves != saves || f.maintainerrPatches != patches {
		t.Fatalf("the second run wrote: %d saves (%d), %d patches (%d)",
			f.maintainerrSaves, saves, f.maintainerrPatches, patches)
	}
	// And the fingerprint is a fingerprint: sha256-hex, never the token.
	var rec OwnedRecord
	if err := json.Unmarshal([]byte(f.cm["owned.json"]), &rec); err != nil {
		t.Fatalf("owned.json: %v", err)
	}
	if got, want := rec.MaintainerrPlexTokenFingerprint(), apps.PlexTokenFingerprint(mediaPlexToken); got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
	if strings.Contains(f.cm["owned.json"], mediaPlexToken) {
		t.Fatal("owned.json carries the token itself")
	}
}

// No token in the release Secret is a pending row with the reason and the fix,
// never a connection written without a credential.
func TestWiringMaintainerrPendingWithoutToken(t *testing.T) {
	r, f := newMediaHarness(t, true)
	r.Config.Maintainerr.PlexTokenFile = t.TempDir() + "/plex-token" // absent
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	s := step(res, "maintainerr/plex")
	if s == nil || s.Result != "pending" {
		t.Fatalf("maintainerr/plex = %+v, want pending", s)
	}
	const want = "no Plex token: set plex-token (from a claimed server) to let the wiring Job connect Maintainerr"
	if s.Detail != want {
		t.Fatalf("maintainerr/plex detail = %q, want %q", s.Detail, want)
	}
	if f.maintainerrSaves != 0 || f.maintainerrPatches != 0 {
		t.Fatalf("Maintainerr was written without a token: %d saves, %d patches", f.maintainerrSaves, f.maintainerrPatches)
	}
}

// Without Plex in the release there is no server to point it at: the row says so
// rather than leaving a connection nobody wrote.
func TestWiringMaintainerrPendingWithoutPlex(t *testing.T) {
	r, f := newMediaHarness(t, true)
	r.Config.Plex = nil
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	s := step(res, "maintainerr/plex")
	if s == nil || s.Result != "pending" || s.Detail != "plex is not deployed in this release" {
		t.Fatalf("maintainerr/plex = %+v, want the pending row about Plex", s)
	}
	if s := step(res, "maintainerr/ready"); s != nil {
		t.Fatal("Maintainerr was contacted with no Plex to connect it to")
	}
	if f.maintainerrSaves != 0 || f.maintainerrPatches != 0 {
		t.Fatalf("Maintainerr was written with no Plex in the release")
	}
}

// A token rotated in the release Secret is pushed again: Maintainerr masks what
// it holds, so the recorded fingerprint is the difference the Job can see.
func TestWiringMaintainerrNoticesARotatedPlexToken(t *testing.T) {
	r, f := newMediaHarness(t, true)
	ctx := context.Background()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	const rotated = mediaPlexToken + "-rotated"
	if err := os.WriteFile(f.tokenFile, []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.acceptToken = rotated
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run after rotation: %v\n%s", err, Summary(res))
	}
	if s := step(res, "maintainerr/plex"); s == nil || s.Detail != "updated" {
		t.Fatalf("maintainerr/plex after rotation = %+v, want updated", s)
	}
	if f.maintainerToken != rotated {
		t.Fatalf("Maintainerr holds %q, want the rotated token", f.maintainerToken)
	}
	noTokenAnywhere(t, res, rotated)
}

// A token Plex refuses stops the run before Maintainerr is written: the same
// rule Tautulli's link follows, so no app is configured against a server that
// will refuse every request.
func TestWiringMaintainerrRejectedTokenFailsFirst(t *testing.T) {
	r, f := newMediaHarness(t, true)
	f.acceptToken = "something-else"
	res, err := r.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with a token Plex rejects\n%s", Summary(res))
	}
	if f.maintainerrSaves != 0 || f.maintainerrPatches != 0 {
		t.Fatalf("Maintainerr was written with a token Plex rejects")
	}
}
