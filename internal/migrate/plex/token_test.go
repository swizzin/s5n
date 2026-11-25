package plex

import (
	"os"
	"path/filepath"
	"testing"
)

// tokenPrefs is Preferences.xml as a signed-in Plex writes it: the account's
// token sits beside the machine's own identity, and the rest of the file is
// what Plex put there.
const tokenPrefs = `<?xml version="1.0" encoding="utf-8"?>
<Preferences OldestPreviousVersion="1.43.5.11079" MachineIdentifier="synthetic-machine" FriendlyName="synthetic" PlexOnlineUsername="synthetic-account" PlexOnlineMail="synthetic@example.test" PlexOnlineToken="synthetic-token-0123456789" PlexOnlineHome="1" TranscoderTempDirectory="/srv/apps/plex/transcode" />
`

// writeHome fills a staged config directory with one Plex home: prefs is
// written when it is not empty, and withDB adds the library database, which is
// the other thing that makes a directory Plex's own.
func writeHome(t *testing.T, rel, prefs string, withDB bool) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if prefs != "" {
		if err := os.WriteFile(filepath.Join(home, prefsName), []byte(prefs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withDB {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash("Plug-in Support/Databases")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(dbRel)), []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The whole point of the read: the account token a migrated server is signed in
// with is in the server's own settings file, and that is the release Secret's
// plex-token once the migration has run.
func TestServerTokenReadsPlexOnlineToken(t *testing.T) {
	dir := writeHome(t, supportDir, tokenPrefs, true)
	token, ok, err := ServerToken(dir)
	if err != nil {
		t.Fatalf("ServerToken: %v", err)
	}
	if !ok || token != "synthetic-token-0123456789" {
		t.Fatalf("ServerToken = %q, %v; want the file's PlexOnlineToken", token, ok)
	}
}

// The image's own layout is not the only one a config can be in: a directory
// mounted as /config itself holds Preferences.xml at its root.
func TestServerTokenFindsTheHomeAtTheRoot(t *testing.T) {
	dir := writeHome(t, "", tokenPrefs, false)
	token, ok, err := ServerToken(dir)
	if err != nil || !ok || token != "synthetic-token-0123456789" {
		t.Fatalf("ServerToken = %q, %v, %v; want the token from the root layout", token, ok, err)
	}
}

// A token is read the way the file stores it: the attribute scanner decodes the
// entities Plex escapes and the value is trimmed, because a Secret is compared
// with what Plex accepts, not with what was quoted.
func TestServerTokenDecodesAndTrims(t *testing.T) {
	prefs := `<Preferences PlexOnlineToken=" synthetic&#45;token " />`
	dir := writeHome(t, supportDir, prefs, false)
	token, ok, err := ServerToken(dir)
	if err != nil || !ok || token != "synthetic-token" {
		t.Fatalf("ServerToken = %q, %v, %v; want the decoded token", token, ok, err)
	}
}

// An install that was never signed in to an account has an empty token, and a
// Plex copy with no Preferences.xml has none at all. Both are "no token" — the
// state the wiring reports pending for — and neither is an error.
func TestServerTokenWithoutAToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefs  string
		withDB bool
	}{
		{"empty attribute", `<Preferences PlexOnlineToken="" MachineIdentifier="m" />`, true},
		{"attribute absent", `<Preferences MachineIdentifier="m" />`, true},
		{"no Preferences.xml", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeHome(t, supportDir, tc.prefs, tc.withDB)
			token, ok, err := ServerToken(dir)
			if err != nil {
				t.Fatalf("ServerToken: %v", err)
			}
			if ok || token != "" {
				t.Fatalf("ServerToken = %q, %v; want no token", token, ok)
			}
		})
	}
}

// A config directory that is not a Plex home at all is not an error here: the
// pre-install pass reads a copy before Plex's own check has run, and that check
// is what refuses such a directory.
func TestServerTokenWithoutAPlexHome(t *testing.T) {
	token, ok, err := ServerToken(t.TempDir())
	if err != nil || ok || token != "" {
		t.Fatalf("ServerToken = %q, %v, %v; want no token and no error", token, ok, err)
	}
}

// A Preferences.xml that cannot be read as Plex's own element is a refusal: the
// token is in it, and a step that guessed would write a wrong one into the
// release Secret.
func TestServerTokenRefusesAnUnreadablePreferences(t *testing.T) {
	dir := writeHome(t, supportDir, `<Preferences PlexOnlineToken=unquoted />`, true)
	if _, _, err := ServerToken(dir); err == nil {
		t.Fatal("ServerToken accepted an unquoted attribute")
	}
}
