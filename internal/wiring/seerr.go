package wiring

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/apps"
)

// wireSeerr waits for Seerr and leaves it in the one state this release can reach: an
// admin that is the person's own media-server account, the libraries this release made
// switched on, and the instance initialized.
//
// Which server it is set up against decides how much of that is the Job's. Plex creates
// its first admin only from an interactive sign-in through a browser, so there the run
// reports the one thing a person has to do; Jellyfin's admin is an account the Job
// already holds the password of (`wireJellyfin` ran first), so there it does the setup
// the setup page would.
func (r *Runner) wireSeerr(ctx context.Context, res *Result, owned *OwnedRecord, c *Config) error {
	seerr := apps.NewSeerr(c.Seerr.URL)
	r.logf("waiting for Seerr at %s", c.Seerr.URL)
	if err := seerr.WaitReady(ctx, timeoutDuration(c.TimeoutSeconds)); err != nil {
		return r.ensure(res, "seerr/ready", apps.Unchanged, err)
	}
	r.okStep(res, "seerr/ready", c.Seerr.URL)
	public, err := seerr.Settings(ctx)
	if err != nil {
		return r.ensure(res, "seerr/settings", apps.Unchanged, err)
	}
	if c.Seerr.MediaServer == mediaServerJellyfin {
		return r.seerrJellyfin(ctx, res, owned, c, seerr, public)
	}
	// Every settings endpoint needs an authenticated admin, and the only way to create
	// Seerr's first admin on Plex is an interactive sign-in through a browser: a claim
	// token authenticates the server to plex.tv, not an account into Seerr. So the only
	// answer this run can give is whether an admin exists; a migrated Seerr brings its
	// admin in its snapshot, and that box gets no row.
	if !public.Initialized {
		res.Steps = append(res.Steps, Step{Name: "seerr/plex", Result: stepPending,
			Detail: fmt.Sprintf("Seerr has no admin account yet: sign in once at %s with Plex to let the wiring Job take over", c.Seerr.PublicURL)})
		r.stepLog("seerr/plex", "pending (needs one interactive Plex sign-in)")
	}
	return nil
}

// seerrJellyfin is the Jellyfin-mode setup, in the order the setup page makes it: sign
// in as the person's server account (which makes it user 1), sync the server's
// libraries and switch on the ones this release made, store the address people reach
// the server at, and initialize.
//
// A Seerr that is already initialized is the person's, not this Job's: a migrated
// Seerr brought its settings and a box that has been set up once keeps them. So is one
// this Job initialized on an earlier run, whatever its flag says now.
func (r *Runner) seerrJellyfin(ctx context.Context, res *Result, owned *OwnedRecord, c *Config, seerr *apps.Seerr, public apps.Public) error {
	if public.Initialized {
		r.okStep(res, "seerr/setup", "already initialized: left as it is")
		return nil
	}
	if owned.SeerrInitialized() {
		res.Steps = append(res.Steps, Step{Name: "seerr/setup", Result: stepSkipped,
			Detail: "this Job initialized this Seerr in an earlier run and does not set it up again: sign in to it to make an admin"})
		r.stepLog("seerr/setup", stepSkipped+" (initialized by an earlier run)")
		return nil
	}
	password, err := ReadSecret(c.Jellyfin.UserPasswordFile)
	if err != nil {
		return r.ensure(res, "seerr/login", apps.Unchanged, err)
	}
	host, port, err := hostPort(c.Jellyfin.URL)
	if err != nil {
		return r.ensure(res, "seerr/login", apps.Unchanged, err)
	}
	if err := seerr.LoginJellyfin(ctx, c.Jellyfin.Username, password, host, port); err != nil {
		var refused *apps.SeerrSignInRefusedError
		if !errors.As(err, &refused) {
			return r.ensure(res, "seerr/login", apps.Unchanged, err)
		}
		// The account is the person's, on their server: a password changed there is
		// their fix, not a broken release (the same rule as jellyfinNoSession).
		res.Steps = append(res.Steps, Step{Name: "seerr/login", Result: stepPending,
			Detail: seerrSignInDetail(refused, c.Jellyfin.Username)})
		r.stepLog("seerr/login", stepPending+" ("+refused.Code+")")
		return nil
	}
	r.okStep(res, "seerr/login", c.Jellyfin.Username)

	if err := r.seerrLibraries(ctx, res, c, seerr); err != nil {
		return err
	}
	if err := seerr.SetExternalHostname(ctx, c.Seerr.MediaServerURL); err != nil {
		return r.ensure(res, "seerr/media-server", apps.Unchanged, err)
	}
	r.okStep(res, "seerr/media-server", c.Seerr.MediaServerURL)
	if err := seerr.Initialize(ctx); err != nil {
		return r.ensure(res, "seerr/initialize", apps.Unchanged, err)
	}
	// Recorded only now: a setup recorded for an initialize nothing accepted would stop
	// every later run from finishing it.
	owned.SetSeerrInitialized()
	return r.ensure(res, "seerr/initialize", apps.Created, nil)
}

// seerrLibraries syncs the server's libraries into Seerr and switches on the ones this
// release made. Seerr keeps an enabled flag per library it already knows, so a library
// the person switched off stays off.
//
// One enabled library is the setup page's own condition for finishing: with none, the
// request page would show nothing and the instance is not set up at all.
func (r *Runner) seerrLibraries(ctx context.Context, res *Result, c *Config, seerr *apps.Seerr) error {
	libraries, err := seerr.SyncLibraries(ctx)
	if err != nil {
		return r.ensure(res, "seerr/libraries", apps.Unchanged, err)
	}
	wanted := map[string]bool{}
	for _, lib := range c.Jellyfin.Libraries {
		wanted[lib.Name] = true
	}
	var enabled, left []string
	for _, lib := range libraries {
		if !wanted[lib.Name] {
			left = append(left, lib.Name)
			continue
		}
		if !lib.Enabled {
			if err := seerr.SetLibraryEnabled(ctx, lib.ID, true); err != nil {
				return r.ensure(res, "seerr/libraries", apps.Unchanged, err)
			}
		}
		enabled = append(enabled, lib.Name)
	}
	if len(enabled) == 0 {
		return r.ensure(res, "seerr/libraries", apps.Unchanged, fmt.Errorf(
			"Seerr's library sync lists %s and this release's Jellyfin libraries are %s, so nothing would be enabled and the request page would show nothing",
			quoted(nameList(libraries)), quoted(libraryNames(c.Jellyfin.Libraries))))
	}
	detail := strings.Join(enabled, ", ")
	if len(left) > 0 {
		// Not a failure: a migrated server keeps libraries this release does not make,
		// and enabling them is the person's choice.
		detail = fmt.Sprintf("%s (left off: %s)", detail, strings.Join(left, ", "))
	}
	r.okStep(res, "seerr/libraries", detail)
	return nil
}

// seerrSignInDetail is the receipt's sentence for a Jellyfin account Seerr would not
// sign in. Seerr's code decides which of the two states it is.
func seerrSignInDetail(refused *apps.SeerrSignInRefusedError, username string) string {
	switch refused.Code {
	case "INVALID_CREDENTIALS":
		return fmt.Sprintf("Seerr cannot sign %q in to Jellyfin: that account's password is not the release Secret's jellyfin-user-password. Set it in Jellyfin, then apply again", username)
	case "NOT_ADMIN":
		return fmt.Sprintf("Seerr needs %q to be an administrator of Jellyfin: make that account an administrator there, then apply again", username)
	default:
		return fmt.Sprintf("Seerr refused %q: %s. Check the account in Jellyfin, then apply again", username, refused.Code)
	}
}

// nameList names the libraries a sync returned, for a refusal that compares them.
func nameList(libraries []apps.SeerrLibrary) []string {
	names := make([]string, 0, len(libraries))
	for _, lib := range libraries {
		names = append(names, lib.Name)
	}
	return names
}

// libraryNames names the libraries this release asked the server for.
func libraryNames(libraries []JellyfinLibrary) []string {
	names := make([]string, 0, len(libraries))
	for _, lib := range libraries {
		names = append(names, lib.Name)
	}
	return names
}

// quoted is a list as one clause, with "(none)" for a list that is empty: a server
// whose sync lists nothing is a server with no libraries, not a name mismatch.
func quoted(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
