package release

// plextoken.go is the one credential `box apply` fills in for itself: the Plex
// server token.
//
// A released Plex is signed in to its owner's Plex.tv account, and it keeps the
// account's token in its own Preferences.xml (`PlexOnlineToken`). That token is
// what the wiring Job hands Tautulli for its PMS link and Maintainerr for its
// Plex connection, and it is the only place either of them can get one: the
// release Secret's `plex-token` is otherwise supplied by hand. A fresh box is
// claimed after it is installed -- the claim token expires in minutes, so init
// asks for it last and it reaches only the first apply -- and a box migrated
// before migrate learned to seed the token never gets one at all. Either way the
// wiring reports `tautulli/plex: pending (no Plex token: set plex-token ...)`
// on a server that can answer, and asks for a Secret patched by hand.
//
// So every apply reads the live config directory Plex keeps its settings in and,
// when the Secret holds no token yet, writes the one Plex has. The chart's
// `lookup` keeps it from then on (charts/s5n/templates/secret.yaml), and the
// release this same run installs re-runs the wiring Job with it, which is what
// changes the pending row into a link.
//
// The limits are the point of the read: a token already in the Secret is never
// written over -- that is the person's own, or one an earlier apply took from
// Plex -- so a server that rotates its token later keeps the box on the old one
// until the key is removed by hand and apply runs again (docs/box.md). And this
// reads, it never writes: Plex owns that file.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// plexTokenKey is the release Secret's `plex-token`
// (charts/s5n/templates/secret.yaml). It is optional for the chart and never
// generated, so on a plain box it exists only when a person supplied one or
// this step wrote it.
const plexTokenKey = "plex-token"

// plexApp is the app whose token this is, and the name its config directory
// has: the chart mounts each instance's /config at
// <storage.config.path>/<instance>, and Plex takes no second instance
// (charts/s5n/catalog.yaml, `instances: false`), so its one instance is named
// for the app.
const plexApp = "plex"

// plexSeed is the token this run would write, and the handle it would write it
// through.
type plexSeed struct {
	// File is the Preferences.xml it was read from: the one line that says what
	// was done names it, and an unreadable one is reported by it.
	File string
	// Token never leaves this run except into the Secret. Nothing says it, logs
	// it or puts it in an error.
	Token string
	// store is the release Secret, opened only once there is a token to look
	// for or write.
	store SecretKeys
}

// needed reports whether this run has a token to seed.
func (s plexSeed) needed() bool { return s.Token != "" }

// planPlexToken decides whether this apply has a token to seed, and reads it
// before anything is asked or written: a run whose only work is that token still
// has work, so it cannot answer "nothing to change", and the token is there for
// the upgrade that follows the question.
//
// Every reason not to seed is a quiet one -- the box is not running Plex, a
// person's token is already the answer, or Plex has never been claimed -- and
// a read that fails is one warning line: a token is a convenience, and never a
// reason to fail an apply.
func (a *Applier) planPlexToken(ctx context.Context, r say.Reporter, values map[string]any, vols []seedbox.Volume, namespace string, exists bool) plexSeed {
	// A first install has no Plex state to read: the release does not exist, so
	// nothing has started Plex yet, and a migration seeds its own token before
	// its install.
	if !exists || a.OpenSecret == nil || !appEnabled(values, plexApp) {
		return plexSeed{}
	}
	// A Secret a person named is theirs: box reads no key of it and writes
	// none. A token in the values file is theirs too, and the chart renders it
	// over whatever the Secret holds.
	if secretIn(values, "existingSecret") != "" || secretIn(values, "plexToken") != "" {
		return plexSeed{}
	}
	root := configRoot(vols)
	if root == "" {
		return plexSeed{}
	}
	token, file, ok, err := plex.ServerTokenFile(filepath.Join(root, plexApp))
	if err != nil {
		r.Warn(fmt.Sprintf("%s: %v", file, err))
		return plexSeed{}
	}
	if !ok {
		// Never claimed, or Plex has not written its settings yet: the wiring
		// row stays pending, exactly as it is today.
		return plexSeed{}
	}
	store, err := a.OpenSecret(namespace)
	if err != nil {
		r.Warn(fmt.Sprintf("%s: %v", plexTokenKey, err))
		return plexSeed{}
	}
	existing, err := store.Get(ctx, plexTokenKey)
	if err != nil {
		r.Warn(fmt.Sprintf("%s: %v", plexTokenKey, err))
		return plexSeed{}
	}
	if strings.TrimSpace(existing) != "" {
		// A present token is never touched, whoever wrote it.
		return plexSeed{}
	}
	return plexSeed{File: file, Token: token, store: store}
}

// writePlexToken puts the token in the release Secret, before the release this
// run installs re-runs the wiring Job with it. The one line it prints is the
// record of a write that a later run cannot see: from then on the key is just
// there, and the next apply says nothing about it. It is a note (`·`): nothing
// is asked of the person, and the file it came from is the one fact worth
// keeping.
func (a *Applier) writePlexToken(ctx context.Context, r say.Reporter, seed plexSeed) bool {
	if err := seed.store.Set(ctx, plexTokenKey, seed.Token); err != nil {
		r.Warn(fmt.Sprintf("%s could not be kept: %v", plexTokenKey, err))
		return false
	}
	r.Rows([]say.Row{{
		Mark:  say.MarkNote,
		Name:  plexTokenKey,
		Facts: []string{"read from Plex's " + filepath.Base(seed.File)},
	}})
	return true
}

// secretIn is one secrets.<key> out of the effective values, trimmed.
func secretIn(values map[string]any, key string) string {
	raw, _ := valueAt(values, "secrets."+key)
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}
