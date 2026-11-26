package plex

import (
	"os"
	"path/filepath"
	"testing"
)

// tokenPrefs is Preferences.xml as a signed-in Plex writes it.
const tokenPrefs = `<?xml version="1.0" encoding="utf-8"?>
<Preferences OldestPreviousVersion="1.43.5.11079" MachineIdentifier="synthetic-machine" FriendlyName="synthetic" PlexOnlineUsername="synthetic-account" PlexOnlineMail="synthetic@example.test" PlexOnlineToken="synthetic-token-0123456789" PlexOnlineHome="1" TranscoderTempDirectory="/srv/apps/plex/transcode" />
`

// writeHome fills a config directory with one Plex home; withDB adds the database,
// the other thing that makes a directory Plex's.
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

// The account token in the server's own settings file is the release Secret's
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

// A directory mounted as /config itself holds Preferences.xml at its root.
func TestServerTokenFindsTheHomeAtTheRoot(t *testing.T) {
	dir := writeHome(t, "", tokenPrefs, false)
	token, ok, err := ServerToken(dir)
	if err != nil || !ok || token != "synthetic-token-0123456789" {
		t.Fatalf("ServerToken = %q, %v, %v; want the token from the root layout", token, ok, err)
	}
}

// The scanner decodes Plex's entities and the value is trimmed: a Secret is
// compared with what Plex accepts.
func TestServerTokenDecodesAndTrims(t *testing.T) {
	prefs := `<Preferences PlexOnlineToken=" synthetic&#45;token " />`
	dir := writeHome(t, supportDir, prefs, false)
	token, ok, err := ServerToken(dir)
	if err != nil || !ok || token != "synthetic-token" {
		t.Fatalf("ServerToken = %q, %v, %v; want the decoded token", token, ok, err)
	}
}

// An empty token and no Preferences.xml are both "no token" and neither an error.
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

// A directory that is not a Plex home is not an error: the check that refuses it
// runs later.
func TestServerTokenWithoutAPlexHome(t *testing.T) {
	token, ok, err := ServerToken(t.TempDir())
	if err != nil || ok || token != "" {
		t.Fatalf("ServerToken = %q, %v, %v; want no token and no error", token, ok, err)
	}
}

// An unreadable Preferences.xml is a refusal: a guess would write a wrong token
// into the release Secret.
func TestServerTokenRefusesAnUnreadablePreferences(t *testing.T) {
	dir := writeHome(t, supportDir, `<Preferences PlexOnlineToken=unquoted />`, true)
	if _, _, err := ServerToken(dir); err == nil {
		t.Fatal("ServerToken accepted an unquoted attribute")
	}
}
