package wiring

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/apps"
)

// jellyfinBoxAccount is the account the Job signs in as: not the person's, because
// the Job must connect on every re-run and the person may change their password. It
// is the chart's automation account name (s5n.wiringAccount).
const jellyfinBoxAccount = "s5n"

// stepPending is the receipt's word for a step that left something for a person to
// decide, the same word wiring.go uses for Plex's claim.
const (
	stepPending = "pending"
	stepSkipped = "skipped"
)

// wireJellyfin makes a Jellyfin server usable and keeps it that way.
//
// It is the one app whose first conversation is a write, because users, keys and
// libraries are rows in data/jellyfin.db. The order is the design:
//
//  1. wait and read whether the wizard is done. The wizard is the app's init
//     container's job (templates/workloads.yaml), so a reachable server still
//     reporting it open is refused rather than completed from outside the pod;
//  2. keep an automation account with a password from the release Secret and sign
//     in as it. The person's password is used only for the first run's administrator.
//     When neither opens the server, every session-bound step is skipped and
//     reported: that is a migrated box, not a failure. An unreachable server or a
//     5xx is still a failure;
//  3. check that the box login's account is on the server; a migrated server keeps
//     its own, and this reports, never renames;
//  4. prove the server's known proxies: with none it ignores X-Forwarded-For, so
//     every request through the Gateway looks local;
//  5. one library per Arr instance, adopted by path when it exists.
func (r *Runner) wireJellyfin(ctx context.Context, res *Result, c *JellyfinConfig) error {
	userPassword, err := ReadSecret(c.UserPasswordFile)
	if err != nil {
		return r.ensure(res, "jellyfin/credentials", apps.Unchanged, err)
	}
	boxPassword, err := ReadSecret(c.BoxPasswordFile)
	if err != nil {
		return r.ensure(res, "jellyfin/credentials", apps.Unchanged, err)
	}

	j := apps.NewJellyfin(c.URL)
	r.logf("waiting for Jellyfin at %s", c.URL)
	wizardDone, err := r.jellyfinReady(ctx, res, j)
	if err != nil {
		return err
	}
	if err := r.jellyfinWizard(res, wizardDone); err != nil {
		return err
	}
	signedIn, err := r.jellyfinAccount(ctx, res, c, j, userPassword, boxPassword)
	if err != nil {
		return err
	}
	if !signedIn {
		// No session, so every step below is one this run cannot take: the apps
		// wired after this one do not depend on Jellyfin's accounts, and a migrated
		// box whose administrator password box lacks is normal.
		r.jellyfinNoSession(res, c)
		return nil
	}
	if err := r.jellyfinPerson(ctx, res, c, j); err != nil {
		return err
	}
	if err := r.jellyfinProxies(ctx, res, c, j); err != nil {
		return err
	}
	return r.jellyfinLibraries(ctx, res, j)
}

// jellyfinNoSession is the receipt of a server box holds no password for: one shared
// sentence for every step that needed a session. It names no secret and no box command
// that does not exist.
func (r *Runner) jellyfinNoSession(res *Result, c *JellyfinConfig) {
	reason := fmt.Sprintf("neither the box's account %q nor %q signs in, so this server holds a password box does not: sign in to Jellyfin as one of its administrators and set %q's password to the release Secret's jellyfin-box-password, or create that account there, then apply again",
		jellyfinBoxAccount, c.Username, jellyfinBoxAccount)
	res.Steps = append(res.Steps, Step{Name: "jellyfin/login", Result: stepPending, Detail: reason})
	r.stepLog("jellyfin/login", stepPending+" (no account signs in)")
	skipped := []string{"jellyfin/account", "jellyfin/apikey", "jellyfin/person"}
	for _, lib := range c.Libraries {
		skipped = append(skipped, "jellyfin/library/"+lib.Name)
	}
	for _, name := range skipped {
		res.Steps = append(res.Steps, Step{Name: name, Result: stepSkipped, Detail: reason})
		r.stepLog(name, "skipped (no session)")
	}
}

// jellyfinReady waits for /System/Info/Public, which carries no [Authorize] and is
// where a client discovers a server, and reports whether the wizard is complete.
func (r *Runner) jellyfinReady(ctx context.Context, res *Result, j *apps.Jellyfin) (bool, error) {
	var info apps.PublicInfo
	err := apps.Retry(ctx, timeoutDuration(r.Config.TimeoutSeconds), func(ctx context.Context) error {
		got, err := j.Info(ctx)
		if err != nil {
			return err
		}
		info = got
		return nil
	})
	if err != nil {
		return false, r.ensure(res, "jellyfin/ready", apps.Unchanged, err)
	}
	r.okStep(res, "jellyfin/ready", fmt.Sprintf("version=%s wizard=%v", info.Version, info.StartupWizardCompleted))
	return info.StartupWizardCompleted, nil
}

// jellyfinWizard checks the one thing this Job will not do: the first-run wizard,
// which the init container completes before the server the Service reaches starts. A
// reachable server with the wizard open was started by something else, so this
// refuses.
func (r *Runner) jellyfinWizard(res *Result, done bool) error {
	if !done {
		return r.ensure(res, "jellyfin/wizard", apps.Unchanged,
			errors.New("the server's first-run wizard is not complete; its pod's startup-wizard init container is what completes it before the server starts, so a server this Job can reach with the wizard open was not started by this release"))
	}
	r.okStep(res, "jellyfin/wizard", "complete")
	return nil
}

// jellyfinPerson checks that the account the person was told to sign in with exists.
// A fresh box has it; a migrated server keeps its own, and this reports rather than
// inventing an account whose password nobody knows.
func (r *Runner) jellyfinPerson(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin) error {
	account, found, err := j.FindUser(ctx, c.Username)
	if err != nil {
		return r.ensure(res, "jellyfin/person", apps.Unchanged, err)
	}
	if found {
		r.okStep(res, "jellyfin/person", account.Name)
		return nil
	}
	detail := fmt.Sprintf("this server has no account called %q; a migrated server keeps the accounts it had, plus box's own %s, so sign in with one of them or rename it in the dashboard", c.Username, jellyfinBoxAccount)
	if users, uerr := j.Users(ctx); uerr == nil {
		names := make([]string, 0, len(users))
		for _, u := range users {
			names = append(names, u.Name)
		}
		sort.Strings(names)
		detail = fmt.Sprintf("this server has no account called %q; its accounts are %s. A migrated server keeps the accounts it had, plus box's own %s, so sign in with one of them or rename it in the dashboard", c.Username, strings.Join(names, ", "), jellyfinBoxAccount)
	}
	res.Steps = append(res.Steps, Step{Name: "jellyfin/person", Result: stepPending, Detail: detail})
	r.stepLog("jellyfin/person", stepPending)
	return nil
}

// jellyfinAccount makes sure the automation account exists, is an administrator, and
// answers to the release Secret's password, then signs in as it. Signing in as the
// person is the fallback that gives a first run an administrator.
//
// It reports false, with no error, when neither account signs in: only the app's
// refusal of the credential means that. An unreachable server or a 5xx is still a
// failed step.
func (r *Runner) jellyfinAccount(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin, userPassword, boxPassword string) (bool, error) {
	token, err := j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
	if err == nil {
		j.SetToken(token)
		r.okStep(res, "jellyfin/login", jellyfinBoxAccount)
		return true, r.jellyfinAPIKey(ctx, res, j)
	}
	if !isRejected(err) {
		return false, r.ensure(res, "jellyfin/login", apps.Unchanged, err)
	}
	person, err := j.AuthenticateByName(ctx, c.Username, userPassword)
	if err != nil {
		if !isRejected(err) {
			return false, r.ensure(res, "jellyfin/login", apps.Unchanged, err)
		}
		return false, nil
	}
	j.SetToken(person)
	if err := r.jellyfinMakeAccount(ctx, res, j, boxPassword); err != nil {
		return false, err
	}
	token, err = j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
	if err != nil {
		return false, r.ensure(res, "jellyfin/login", apps.Unchanged, err)
	}
	j.SetToken(token)
	r.okStep(res, "jellyfin/login", jellyfinBoxAccount)
	return true, r.jellyfinAPIKey(ctx, res, j)
}

// isRejected reports whether an app refused the credential it was given, as
// opposed to being unreachable or refusing the session for another reason.
func isRejected(err error) bool {
	var rejected *apps.LoginRejectedError
	return errors.As(err, &rejected)
}

// jellyfinMakeAccount creates the automation account, or puts the Secret's password
// back on it. An administrator may set another account's password without knowing the
// old one (UserController.cs, UpdateUserPassword).
func (r *Runner) jellyfinMakeAccount(ctx context.Context, res *Result, j *apps.Jellyfin, boxPassword string) error {
	account, found, err := j.FindUser(ctx, jellyfinBoxAccount)
	if err != nil {
		return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
	}
	var change apps.Change
	switch {
	case !found:
		if account, err = j.CreateUser(ctx, jellyfinBoxAccount, boxPassword); err != nil {
			return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
		}
		change = apps.Created
	default:
		if err := j.SetPassword(ctx, account.ID, boxPassword); err != nil {
			return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
		}
		change = apps.Updated
	}
	admin, err := j.SetAdministrator(ctx, account.ID)
	if err != nil {
		return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
	}
	if change == apps.Unchanged && admin == apps.Updated {
		change = apps.Updated
	}
	return r.ensure(res, "jellyfin/account", change, nil)
}

// jellyfinAPIKey mints the box's key once and re-reads it later: it is the person's
// handle on their server, and the proof this Job reaches the API as an administrator.
// It is used and never recorded.
func (r *Runner) jellyfinAPIKey(ctx context.Context, res *Result, j *apps.Jellyfin) error {
	_, change, err := j.EnsureAPIKey(ctx, jellyfinBoxAccount)
	return r.ensure(res, "jellyfin/apikey", change, err)
}

// jellyfinProxies tells the server whose forwarded address to trust. An empty list
// switches X-Forwarded-For off entirely, and this server is only reached through the
// Gateway, so every request would look local.
func (r *Runner) jellyfinProxies(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin) error {
	if len(c.KnownProxies) == 0 {
		res.Steps = append(res.Steps, Step{Name: "jellyfin/known-proxies", Result: stepPending,
			Detail: "apps.jellyfin.knownProxies is empty, so Jellyfin ignores X-Forwarded-For and every request through the Gateway counts as local; box writes the cluster's pod network on every apply, and a plain install sets its own"})
		r.stepLog("jellyfin/known-proxies", stepPending+" (empty)")
		return nil
	}
	// The list is read, never written: the app builds its forwarded-header handling
	// from it at startup, so this proves the running server has it. A server without
	// it is not failed -- it works, and everything through the Gateway looks local.
	live, err := j.Proxies(ctx)
	if err != nil {
		return r.ensure(res, "jellyfin/known-proxies", apps.Unchanged, err)
	}
	want := append([]string(nil), c.KnownProxies...)
	sort.Strings(want)
	if strings.Join(live, ",") == strings.Join(want, ",") {
		r.okStep(res, "jellyfin/known-proxies", strings.Join(live, ", "))
		return nil
	}
	res.Steps = append(res.Steps, Step{Name: "jellyfin/known-proxies", Result: stepPending,
		Detail: fmt.Sprintf("the running server trusts %q and this release serves %q: it reads the list at startup, so the next start of its pod takes it (the chart writes it before the app runs)",
			strings.Join(live, ", "), strings.Join(want, ", "))})
	r.stepLog("jellyfin/known-proxies", stepPending+" ("+strings.Join(live, ", ")+")")
	return nil
}

// jellyfinLibraries gives each Arr instance's directory a virtual folder, adopted
// when it exists, so a migrated server keeps the name the person gave it.
func (r *Runner) jellyfinLibraries(ctx context.Context, res *Result, j *apps.Jellyfin) error {
	for _, lib := range r.Config.Jellyfin.Libraries {
		change, err := j.EnsureLibrary(ctx, lib.Name, lib.CollectionType, lib.Path)
		if err := r.ensure(res, "jellyfin/library/"+lib.Name, change, err); err != nil {
			return err
		}
	}
	return nil
}
