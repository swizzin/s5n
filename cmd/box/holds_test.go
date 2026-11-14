package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

const (
	holdDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	holdDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func holdFor(app string) snapshot.Hold {
	return snapshot.Hold{
		App:      app,
		At:       snapshot.ImageRef{Repository: "registry.example.com/seedbox/" + app, Tag: "1.0.0", Digest: holdDigestA},
		Avoiding: snapshot.ImageRef{Repository: "registry.example.com/seedbox/" + app, Tag: "1.1.0", Digest: holdDigestB},
		Snapshot: "0.4.0-0.5.0-20260102T030405Z",
		Since:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		By:       snapshot.ByUpdate,
	}
}

// TestALapsedHoldIsForgottenOnlyAfterASuccessfulApply: the file is box's only
// record of an app held back. A run whose apply failed must leave it alone, or
// the next apply moves the app onto the image the rollback avoided.
func TestALapsedHoldIsForgottenOnlyAfterASuccessfulApply(t *testing.T) {
	root := t.TempDir()
	lapsed := holdFor("sonarr")
	if err := snapshot.SaveHolds(root, []snapshot.Hold{lapsed, holdFor("radarr")}); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	b := &boxHolds{
		out:    &say.Plain{W: out},
		root:   root,
		lapsed: []update.Lapse{{Hold: lapsed, Text: "sonarr moves with the release again: s5n 0.5.0 pins 1.1.0, not 1.0.0"}},
	}

	failed := func(context.Context, release.Options) (release.Result, error) {
		return release.Result{}, errors.New("the apply refused")
	}
	if _, err := applyThenSettle(context.Background(), failed, release.Options{}, b); err == nil {
		t.Fatal("a failed apply reported success")
	}
	holds, err := snapshot.LoadHolds(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 {
		t.Fatalf("holds after a failed apply = %+v, want both kept", holds)
	}
	if out.Len() != 0 {
		t.Fatalf("a failed apply said the hold was over:\n%s", out)
	}

	applied := func(context.Context, release.Options) (release.Result, error) {
		return release.Result{Applied: true}, nil
	}
	if _, err := applyThenSettle(context.Background(), applied, release.Options{}, b); err != nil {
		t.Fatal(err)
	}
	holds, err = snapshot.LoadHolds(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].App != "radarr" {
		t.Fatalf("holds after the successful apply = %+v, want sonarr's gone and radarr's kept", holds)
	}
	if !strings.Contains(out.String(), "sonarr moves with the release again: s5n 0.5.0 pins 1.1.0, not 1.0.0") {
		t.Fatalf("transcript does not say the hold lapsed:\n%s", out)
	}
}

// TestBoxHoldsReadsTheFileBesideTheGrant: the config root is the grant's, so the
// holds file is found from the seedbox apply loaded rather than from a flag or a
// default.
func TestBoxHoldsReadsTheFileBesideTheGrant(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.MkdirAll(config, 0o750); err != nil {
		t.Fatal(err)
	}
	grant := filepath.Join(dir, "grant.yaml")
	values := filepath.Join(dir, "values.yaml")
	grantYAML := "namespace: testbox\nstorage:\n  data:\n    path: " + filepath.Join(dir, "data") +
		"\n  config:\n    path: " + config + "\n"
	if err := os.WriteFile(grant, []byte(grantYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(values, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.SaveHolds(config, []snapshot.Hold{holdFor("sonarr")}); err != nil {
		t.Fatal(err)
	}
	sb, err := seedbox.Load(grant, values)
	if err != nil {
		t.Fatal(err)
	}
	chart := release.Chart{Name: "s5n", Version: "0.5.0", Variants: []byte(`sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
    tag: "1.0.0"
    digest: ` + holdDigestB + "\n")}

	b := newBoxHolds(&say.Plain{W: &bytes.Buffer{}})
	overlay, err := b.values(context.Background(), sb, chart, map[string]any{
		"apps": map[string]any{"sonarr": map[string]any{"enabled": true}},
	})
	if err != nil {
		t.Fatalf("boxholds: %v", err)
	}
	if len(b.lapsed) != 0 {
		t.Fatalf("lapsed = %+v, want none: the release still pins the avoided image", b.lapsed)
	}
	apps, _ := overlay["apps"].(map[string]any)
	sonarr, _ := apps["sonarr"].(map[string]any)
	image, _ := sonarr["image"].(map[string]any)
	if image["digest"] != holdDigestA {
		t.Fatalf("overlay = %#v, want the held digest", overlay)
	}
}
