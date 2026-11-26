package release

// The Plex server token `box apply` fills in for itself: a claimed server keeps
// it in its own Preferences.xml, and the release Secret's plex-token is what
// the wiring Job hands Tautulli and Maintainerr.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecret counts reads and writes so a test proves the step stayed out.
type fakeSecret struct {
	data    map[string]string
	getErr  error
	setErr  error
	gets    int
	setKeys []string
}

func (s *fakeSecret) Get(_ context.Context, key string) (string, error) {
	s.gets++
	if s.getErr != nil {
		return "", s.getErr
	}
	return s.data[key], nil
}

func (s *fakeSecret) Set(_ context.Context, key, value string) error {
	if s.setErr != nil {
		return s.setErr
	}
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.data[key] = value
	s.setKeys = append(s.setKeys, key)
	return nil
}

// plexPrefs is a claimed server's token.
const plexPrefs = "table-token-abc123"

// plexBox is an installed box running Plex with a claimed settings file.
func plexBox(t *testing.T) (*fakes, *fakeSecret) {
	t.Helper()
	f := newFakes(t)
	rewrite(t, f.grantPath, map[string]string{
		"  sonarr:\n    enabled: true": "  sonarr:\n    enabled: true\n  plex:\n    enabled: true",
	})
	chart := testChart(t)
	f.rel.values = deployedFor(t, f, chart)
	f.rel.exists = true
	// An earlier run installed it: namespace, claims and release all exist.
	f.store.nsExists = true
	f.store.nsEnforce = podSecurityRestricted
	for _, claim := range []string{"testbox-s5n-data", "testbox-s5n-config"} {
		f.store.existing[claim] = true
	}
	sec := &fakeSecret{data: map[string]string{}}
	f.secret = sec
	return f, sec
}

// writePlexPrefs writes a claimed Preferences.xml as Plex does.
func writePlexPrefs(t *testing.T, configRoot, content string) string {
	t.Helper()
	dir := filepath.Join(configRoot, plexApp, "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Preferences.xml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func prefsWith(token string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<Preferences MachineIdentifier="machine-1" FriendlyName="box" PlexOnlineUsername="alice" PlexOnlineToken="` + token + `" />` + "\n"
}

func TestApplySeedsThePlexTokenFromPlexItself(t *testing.T) {
	f, sec := plexBox(t)
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if got := sec.data[plexTokenKey]; got != plexPrefs {
		t.Fatalf("the release Secret's %s = %q, want the token Plex holds", plexTokenKey, got)
	}
	if !res.PlexToken {
		t.Fatalf("result does not say the run wrote the token: %+v", res)
	}
	// It installs again anyway: the wiring Job turns the token into a link.
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the one install that re-runs the wiring", f.rel.upgrades)
	}
	out := f.out.String()
	if !strings.Contains(out, "· plex-token     read from Plex's Preferences.xml") {
		t.Fatalf("apply did not say where the token came from:\n%s", out)
	}
	if !strings.Contains(out, "running its hooks") {
		t.Fatalf("apply did not say what it installs the unchanged release for:\n%s", out)
	}
	// The token is never printed by the step, diff, values or receipt.
	if strings.Contains(out, plexPrefs) {
		t.Fatalf("apply printed the token:\n%s", out)
	}
	for _, values := range f.rel.upgraded {
		if strings.Contains(anyString(values), plexPrefs) {
			t.Fatalf("the token reached the values Helm stores: %v", values)
		}
	}
}

func TestApplyLeavesAPlexTokenTheSecretAlreadyHolds(t *testing.T) {
	f, sec := plexBox(t)
	sec.data[plexTokenKey] = "a-token-a-person-set"
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if got := sec.data[plexTokenKey]; got != "a-token-a-person-set" {
		t.Fatalf("the release Secret's %s = %q, want the one it already held", plexTokenKey, got)
	}
	if len(sec.setKeys) != 0 || res.PlexToken {
		t.Fatalf("apply wrote the Secret (%v) although it held a token", sec.setKeys)
	}
	// A no-op proves the token is not re-read into a write every apply.
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none", f.rel.upgrades)
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("apply did not say there was nothing to change:\n%s", f.out)
	}
}

func TestApplyNeverTouchesAnExistingSecret(t *testing.T) {
	f, sec := plexBox(t)
	f.writeValues(t, "secrets:\n  existingSecret: the-users-own-keys\n")
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if sec.gets != 0 || len(sec.setKeys) != 0 {
		t.Fatalf("apply touched the Secret a person named: %d read(s), writes %v", sec.gets, sec.setKeys)
	}
}

func TestApplyLeavesPlexToATokenInTheValues(t *testing.T) {
	f, sec := plexBox(t)
	f.writeValues(t, "secrets:\n  plexToken: a-token-from-the-values\n")
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if sec.gets != 0 || len(sec.setKeys) != 0 {
		t.Fatalf("apply touched the Secret although the values name a token: %d read(s), writes %v", sec.gets, sec.setKeys)
	}
}

func TestAFirstInstallSeedsNoPlexToken(t *testing.T) {
	f, sec := plexBox(t)
	f.rel.exists = false
	f.rel.values = nil
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if sec.gets != 0 || len(sec.setKeys) != 0 || res.PlexToken {
		t.Fatalf("a first install read or wrote the Secret: %d read(s), writes %v", sec.gets, sec.setKeys)
	}
}

func TestApplySkipsTheTokenWhenPlexIsOff(t *testing.T) {
	f := newFakes(t)
	chart := testChart(t)
	f.rel.values = deployedFor(t, f, chart)
	f.rel.exists = true
	sec := &fakeSecret{data: map[string]string{}}
	f.secret = sec
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if sec.gets != 0 || len(sec.setKeys) != 0 {
		t.Fatalf("apply touched the Secret for a box without Plex: %d read(s), writes %v", sec.gets, sec.setKeys)
	}
}

func TestAnUnclaimedPlexWritesNoToken(t *testing.T) {
	f, sec := plexBox(t)
	writePlexPrefs(t, f.configPath, prefsWith(""))

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if sec.gets != 0 || len(sec.setKeys) != 0 {
		t.Fatalf("apply touched the Secret for an unclaimed Plex: %d read(s), writes %v", sec.gets, sec.setKeys)
	}
	if strings.Contains(f.out.String(), plexTokenKey) {
		t.Fatalf("apply said something about a token it does not have:\n%s", f.out)
	}
}

func TestAGarbledPreferencesIsOneLineAndTheApplyGoesOn(t *testing.T) {
	f, sec := plexBox(t)
	path := writePlexPrefs(t, f.configPath, "<Preferences PlexOnlineToken=\"never closed\n")

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(sec.setKeys) != 0 || res.PlexToken {
		t.Fatalf("apply wrote the Secret from a file it could not read: %v", sec.setKeys)
	}
	out := f.out.String()
	if !strings.Contains(out, path) || !strings.Contains(out, "has no closing quote") {
		t.Fatalf("apply did not name the file it could not read:\n%s", out)
	}
	if strings.Count(out, path) != 1 {
		t.Fatalf("apply said more than one line about the file:\n%s", out)
	}
}

func TestASecretThatCannotBeReadIsOneLineAndTheApplyGoesOn(t *testing.T) {
	f, sec := plexBox(t)
	sec.getErr = errors.New("the server refused the read")
	writePlexPrefs(t, f.configPath, prefsWith(plexPrefs))

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(sec.setKeys) != 0 || res.PlexToken {
		t.Fatalf("apply wrote the Secret it could not read: %v", sec.setKeys)
	}
	if !strings.Contains(f.out.String(), plexTokenKey+": the server refused the read") {
		t.Fatalf("apply did not say what it could not do:\n%s", f.out)
	}
}

// anyString flattens values so a test can find one string.
func anyString(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, inner := range t {
				b.WriteString(k)
				b.WriteString(":")
				walk(inner)
			}
		case []any:
			for _, inner := range t {
				walk(inner)
			}
		default:
			fmt.Fprintf(&b, "%v ", v)
		}
	}
	walk(v)
	return b.String()
}
