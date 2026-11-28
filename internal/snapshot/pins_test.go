package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	pinDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func onePin(app, tag, digest string) Pin {
	return Pin{
		App:        app,
		Repository: "ghcr.io/example/" + app,
		Tag:        tag,
		Digest:     digest,
		Resolved:   time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC),
		Chart:      "0.6.3",
	}
}

func TestAPinSurvivesTheRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := []Pin{onePin("qbittorrent", "5.3-lt2.1", pinDigestA), onePin("sonarr", "5.0", pinDigestB)}
	if err := SavePins(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPins(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pins = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, boxDir, PinsName)); err != nil {
		t.Fatalf("the file is not beside the holds file: %v", err)
	}
}

func TestABoxWithNoPinsHasNoFile(t *testing.T) {
	root := t.TempDir()
	pins, err := LoadPins(root)
	if err != nil || len(pins) != 0 {
		t.Fatalf("LoadPins on an empty box = %+v, %v; want none and no error", pins, err)
	}
	if err := SavePins(root, []Pin{onePin("sonarr", "5.0", pinDigestA)}); err != nil {
		t.Fatal(err)
	}
	if err := SavePins(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PinsPath(root)); !os.IsNotExist(err) {
		t.Fatalf("saving no pins left the file behind: %v", err)
	}
}

func TestAPinFileBoxCannotReadIsAnErrorNamingIt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, boxDir), 0o750); err != nil {
		t.Fatal(err)
	}
	path := PinsPath(root)
	for _, c := range []struct {
		name string
		body string
		want string
	}{
		{"not json", "{", "pins"},
		{"an unknown field", `{"pins":[{"app":"sonarr","repository":"r","tag":"t","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":"2026-11-27T10:00:00Z","chart":"1","extra":1}]}`, "unknown field"},
		{"one app twice", `{"pins":[{"app":"sonarr","repository":"r","tag":"t","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":"2026-11-27T10:00:00Z","chart":"1"},{"app":"sonarr","repository":"r","tag":"t","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":"2026-11-27T10:00:00Z","chart":"1"}]}`, "twice"},
		{"no tag", `{"pins":[{"app":"sonarr","repository":"r","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":"2026-11-27T10:00:00Z","chart":"1"}]}`, "tag"},
		{"a digest that is not one", `{"pins":[{"app":"sonarr","repository":"r","tag":"t","digest":"abc","resolved":"2026-11-27T10:00:00Z","chart":"1"}]}`, "digest"},
		{"no repository", `{"pins":[{"app":"sonarr","tag":"t","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resolved":"2026-11-27T10:00:00Z","chart":"1"}]}`, "repository"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPins(root)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadPins = %v, want an error naming %q", err, c.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("LoadPins error does not name %s: %v", path, err)
			}
		})
	}
}

func TestWithPinReplacesTheAppsOwn(t *testing.T) {
	pins := []Pin{onePin("radarr", "7.1", pinDigestB)}
	pins = WithPin(pins, onePin("sonarr", "5.0", pinDigestA))
	pins = WithPin(pins, onePin("sonarr", "5.1", pinDigestB))
	if len(pins) != 2 || pins[0].App != "radarr" || pins[1].App != "sonarr" || pins[1].Tag != "5.1" {
		t.Fatalf("pins = %+v, want radarr then sonarr at 5.1", pins)
	}
	if got, ok := PinFor(pins, "sonarr"); !ok || got.Tag != "5.1" {
		t.Fatalf("PinFor(sonarr) = %+v, %v", got, ok)
	}
	pins = WithoutPin(pins, "sonarr")
	if len(pins) != 1 || pins[0].App != "radarr" {
		t.Fatalf("pins after WithoutPin = %+v", pins)
	}
}
