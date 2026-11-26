package release

// plextoken.go is the one credential `box apply` fills in: the Plex token, read
// from Preferences.xml when the Secret has none, for Tautulli and Maintainerr.
//
// A token in the Secret is never overwritten, so a rotated one stays old until
// the key is removed and apply runs again (docs/box.md). Box only reads.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// plexTokenKey is the release Secret's `plex-token`, optional and never
// generated: it exists only when a person or this step wrote it.
const plexTokenKey = "plex-token"

// plexApp is the app and its config directory name; Plex takes one instance
// (charts/s5n/catalog.yaml, `instances: false`).
const plexApp = "plex"

// plexSeed is the token this run would write, and the handle it would write it
// through.
type plexSeed struct {
	// File is the Preferences.xml it was read from, named in the one line.
	File string
	// Token never leaves this run except into the Secret.
	Token string
	// store is the release Secret, opened only when there is a token.
	store SecretKeys
}

// needed reports whether this run has a token to seed.
func (s plexSeed) needed() bool { return s.Token != "" }

// planPlexToken reads the token before anything is asked or written, so a
// token-only run is still work and the upgrade has it. Every reason not to seed
// is quiet, and a failed read is one warning: a token never fails an apply.
func (a *Applier) planPlexToken(ctx context.Context, r say.Reporter, values map[string]any, vols []seedbox.Volume, namespace string, exists bool) plexSeed {
	// A first install has no Plex state; a migration seeds its own token.
	if !exists || a.OpenSecret == nil || !appEnabled(values, plexApp) {
		return plexSeed{}
	}
	// A person's Secret or values token is theirs; box writes neither.
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
		// Never claimed, or no settings yet: the wiring row stays pending.
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

// writePlexToken puts the token in the Secret before the release re-runs the
// wiring Job. One note line is the only record: later runs see just the key.
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

// secretIn is one secrets.<key>, trimmed.
func secretIn(values map[string]any, key string) string {
	raw, _ := valueAt(values, "secrets."+key)
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}
