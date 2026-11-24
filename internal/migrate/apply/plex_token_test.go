package apply

// plex_token_test.go is item 8 of the receipt's Plex rows: a migrated box knows
// its Plex server token. The token lives in the migrated server's own
// Preferences.xml, and the release Secret's plex-token is what every
// Plex-dependent app reads -- so the run reads it out of the file it is copying
// and seeds the Secret, never over a token a person supplied.
//
// Every fixture here is synthetic: a Preferences.xml shaped the way Plex writes
// one, an invented token, and the seedbox's two host paths.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// testPlexToken is the account token the synthetic source server is signed in
// with. Deliberately not shaped like a real one.
const testPlexToken = "synthetic-server-token-0000"

// plexPrefs is Preferences.xml as a claimed Plex writes it, in the image's own
// layout.
const plexPrefs = `<?xml version="1.0" encoding="utf-8"?>
<Preferences OldestPreviousVersion="1.43.5.11079" MachineIdentifier="synthetic-machine" FriendlyName="synthetic" PlexOnlineUsername="synthetic-account" PlexOnlineMail="synthetic@example.test" PlexOnlineToken="` + testPlexToken + `" PlexOnlineHome="1" TranscoderTempDirectory="/srv/apps/plex/transcode" />
`

// plexFixture is a snapshot holding one Plex and nothing else: the source's
// config directory, the seedbox's empty config root, and the plan that connects
// them. The library database is left out on purpose -- a Preferences.xml is the
// whole of what the token comes from, and a config with no database is one
// Plex's rewriter only notes.
type plexFixture struct {
	t          *testing.T
	configRoot string
	configs    string
	doc        string
	keys       *fakeKeys
}

func newPlexFixture(t *testing.T, prefs string) *plexFixture {
	t.Helper()
	root := t.TempDir()
	f := &plexFixture{
		t:          t,
		configRoot: filepath.Join(root, "config"),
		configs:    filepath.Join(root, "snapshot", "config"),
		keys:       newFakeKeys(),
	}
	home := filepath.Join(f.configs, "plex", "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.configRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if prefs != "" {
		writeFile(t, filepath.Join(home, "Preferences.xml"), prefs, 0o600)
	}
	f.doc = `version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    namespace: media
    ports: [32400]
    paths:
      - from: /srv/apps/plex
        to: /config
`
	return f
}

// env is one run's world: no data renames (this plan names no data), Plex's
// runner in place because preflight requires one for an app whose rewriter can
// need the image, and the fake Secret.
func (f *plexFixture) env(ask confirmFunc) Env {
	return Env{
		ConfigRoot: f.configRoot,
		Configs:    f.configs,
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets:    map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Enabled:    func(string) bool { return false },
		Owned:      &fakeOwned{},
		Secret:     f.keys,
		Values:     &fakeValues{},
		Confirm:    ask,
		Catalog:    []string{"plex", "sonarr", "radarr", "prowlarr", "tautulli", "seerr", "maintainerr"},
		Chown:      func(string, int, int) error { return nil },
		// The library database is absent from this fixture, so the rewriter
		// never runs a tool; a runner that refuses to be called proves it.
		Run: func(context.Context, string, []string, []byte) ([]byte, error) {
			return nil, errors.New("this fixture's Plex has no library database: the runner must not be called")
		},
	}
}

// run performs one migration of the fixture's plan.
func (f *plexFixture) run(ask confirmFunc) (Result, error) {
	f.t.Helper()
	p, raw := f.plan()
	return Run(context.Background(), p, raw, f.env(ask))
}

// plan parses the fixture's plan and returns it with its bytes.
func (f *plexFixture) plan() (*plan.Plan, []byte) {
	f.t.Helper()
	p, err := plan.Parse([]byte(f.doc))
	if err != nil {
		f.t.Fatalf("parse the fixture plan: %v", err)
	}
	return p, []byte(f.doc)
}

// A migrated Plex that was signed in to an account carries the account's token
// in its own settings file, and the release Secret is where every Plex-dependent
// app reads it from: the run seeds it, says so on the confirm's Plex row, and
// names the key it wrote.
func TestMigrateSeedsThePlexTokenFromPreferences(t *testing.T) {
	f := newPlexFixture(t, plexPrefs)
	var shown Summary
	res, err := f.run(yes(t, &shown))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.keys.values[plexTokenKey]; got != testPlexToken {
		t.Fatalf("the release Secret's %s = %q, want the migrated Preferences.xml's token", plexTokenKey, got)
	}
	if len(f.keys.sets) != 1 || f.keys.sets[0] != plexTokenKey {
		t.Fatalf("the Secret was written %v, want exactly %s", f.keys.sets, plexTokenKey)
	}
	if len(res.Keys) != 1 || res.Keys[0] != plexTokenKey {
		t.Fatalf("the result names %v, want %s", res.Keys, plexTokenKey)
	}
	// The confirm said it would, in the credential's own words.
	if lines := strings.Join(shown.Lines(), "\n"); !strings.Contains(lines, "plex: keep its Plex token ("+plexTokenKey+")") {
		t.Fatalf("the confirm does not say the Plex token is kept:\n%s", lines)
	}
	// And the token is nowhere in what a run publishes or prints.
	if strings.Contains(strings.Join(shown.Lines(), "\n"), testPlexToken) {
		t.Fatal("the summary carries the token itself")
	}
	for _, k := range res.Keys {
		if strings.Contains(k, testPlexToken) {
			t.Fatalf("a key name carries the token: %q", k)
		}
	}
}

// A person's own token wins. The release Secret's plex-token is the one key a
// box can be given before a migration, and a run that wrote the source's over it
// would silently move every Plex-dependent app onto a different account.
func TestMigrateNeverWritesOverAPlexTokenTheBoxAlreadyHas(t *testing.T) {
	f := newPlexFixture(t, plexPrefs)
	const handSet = "token-a-person-supplied"
	f.keys.values[plexTokenKey] = handSet
	res, err := f.run(yes(t, &Summary{}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.keys.values[plexTokenKey]; got != handSet {
		t.Fatalf("the release Secret's %s = %q, want the token the box already had", plexTokenKey, got)
	}
	if len(f.keys.sets) != 0 {
		t.Fatalf("the Secret was written %v, want no write", f.keys.sets)
	}
	if len(res.Keys) != 0 {
		t.Fatalf("the result names %v, want nothing kept", res.Keys)
	}
	// The run says once that the token it found was not used, so an operator
	// who reads the receipt knows which account the box is on.
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "already holds a "+plexTokenKey) {
		t.Fatalf("the run's notes do not say the existing token was kept: %v", res.Notes)
	}
	if strings.Contains(joined, testPlexToken) || strings.Contains(joined, handSet) {
		t.Fatalf("a note carries a token: %q", joined)
	}
}

// A Plex that was never signed in has no token to seed: the Secret keeps no
// plex-token, the confirm says nothing about one, and the rows that need it
// report pending -- which is the wiring's business, not this run's. (A copy with
// no Preferences.xml at all is Plex's own check's business: it refuses a config
// directory that holds neither that file nor the library database.)
func TestMigrateSeedsNothingWhenPlexWasNeverSignedIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prefs string
	}{
		{"empty token", strings.Replace(plexPrefs, `PlexOnlineToken="`+testPlexToken+`"`, `PlexOnlineToken=""`, 1)},
		{"attribute absent", strings.Replace(plexPrefs, ` PlexOnlineToken="`+testPlexToken+`"`, "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlexFixture(t, tc.prefs)
			var shown Summary
			res, err := f.run(yes(t, &shown))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(f.keys.sets) != 0 || len(res.Keys) != 0 {
				t.Fatalf("the run kept %v, want nothing", f.keys.sets)
			}
			if lines := strings.Join(shown.Lines(), "\n"); strings.Contains(lines, "Plex token") {
				t.Fatalf("the confirm mentions a Plex token there is none of:\n%s", lines)
			}
		})
	}
}

// A rehearsal is the same read: the token comes from the snapshot's own copy, so
// the box's lock (no internet egress) changes nothing here. What it wrote before
// the lock is what the wiring Job then reads.
func TestMigrateSeedsThePlexTokenInARehearsalToo(t *testing.T) {
	f := newPlexFixture(t, plexPrefs)
	env := f.env(yes(t, &Summary{}))
	env.Rehearsal = true
	p, raw := f.plan()
	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.keys.values[plexTokenKey]; got != testPlexToken {
		t.Fatalf("a rehearsal left the release Secret's %s = %q, want the migrated token", plexTokenKey, got)
	}
	if len(res.Keys) != 1 || res.Keys[0] != plexTokenKey {
		t.Fatalf("the rehearsal names %v, want %s", res.Keys, plexTokenKey)
	}
}

// The pass before an install promises the Plex token too. That pass stages
// nothing of a Plex whose whole data travels -- the snapshot is counted, not
// copied -- so a token read out of the staged copy is there for the run and not
// for the question, and the run refuses itself for saying more than it was
// agreed to. Both passes read the token out of the snapshot's own copy.
func TestThePassBeforeAnInstallPromisesThePlexToken(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	keys := newFakeKeys()
	env.Secret = keys
	const prefs = "Library/Application Support/Plex Media Server/Preferences.xml"
	for _, at := range []string{
		filepath.Join(bulk.tree, filepath.FromSlash(prefs)),
		filepath.Join(env.Configs, "plex", filepath.FromSlash(prefs)),
	} {
		writeFile(t, at, plexPrefs, 0o600)
	}
	agreed, err := Survey(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	want := "plex: keep its Plex token (" + plexTokenKey + ")"
	if lines := strings.Join(agreed.Lines(), "\n"); !strings.Contains(lines, want) {
		t.Fatalf("the question does not say the Plex token is kept:\n%s", lines)
	}
	bulk.measured, bulk.staged = nil, nil
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if why := shown.Holds(agreed); why != "" {
		t.Fatalf("the run does not hold what the question promised: %s", why)
	}
	if got := keys.values[plexTokenKey]; got != testPlexToken {
		t.Fatalf("the release Secret's %s = %q, want the migrated token", plexTokenKey, got)
	}
}
