package wiring

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/apps"
)

// startUpBudget is how long a fresh server gets to have created its first user:
// /Startup/User answers 404 in the moment between the listener opening and the
// user table being seeded, and that moment is not a failure.
const startUpBudget = 60 * time.Second

// jellyfinBoxAccount is the account the wiring Job signs in as. It is not the
// person's: the Job must be able to connect on every re-run, and the person may
// change their own password at any time. It is the same name the chart gives the
// automation account of an app that keeps its own user table (s5n.wiringAccount),
// because it is the same thing -- the box's own account, not a person's.
const jellyfinBoxAccount = "s5n"

// stepPending is the receipt's word for a step that left something for a person
// to decide. It is the same word wiring.go uses for Plex's claim.
const stepPending = "pending"

// detailStep is ensure with a sentence of its own in place of the bare change
// word: the step's name says what was checked, and this says what it found.
func (r *Runner) detailStep(res *Result, name string, change apps.Change, detail string, err error) error {
	if err := r.ensure(res, name, change, err); err != nil {
		return err
	}
	if detail != "" {
		res.Steps[len(res.Steps)-1].Detail = detail
	}
	return nil
}

// wireJellyfin makes a Jellyfin server usable and keeps it that way.
//
// It is the one app here whose first conversation is a write: Jellyfin keeps its
// users, API keys and libraries in data/jellyfin.db, so no file can be sealed
// before it starts. The order is the whole design:
//
//  1. wait for the server and read whether its wizard is done;
//  2. complete the wizard with the person's account. Until that call lands the
//     server's /Startup/* and library endpoints answer anyone; the chart is what
//     keeps those paths off the Gateway (templates/workloads.yaml), and this is
//     the only caller that can reach them at all;
//  3. keep an automation account with a password from the release Secret and
//     sign in as it. The person's password is used for step 2 and for recovery,
//     and nothing else, so the person changing it does not break wiring;
//  4. prove the server's view of the client address: with KnownProxies empty
//     Jellyfin ignores X-Forwarded-For completely, so every request through the
//     Gateway looks local and the endpoints a local caller may use without a
//     credential would be open to the internet;
//  5. one library per Arr instance, adopted by path when it already exists.
//
// A second run writes nothing: the wizard answers 403 once the first user has a
// password, the account and the key are looked up before they are made, and a
// library is adopted by the directory it holds.
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
	if err := r.jellyfinWizard(ctx, res, c, j, userPassword, wizardDone); err != nil {
		return err
	}
	if err := r.jellyfinAccount(ctx, res, c, j, userPassword, boxPassword); err != nil {
		return err
	}
	if err := r.jellyfinProxies(ctx, res, c, j); err != nil {
		return err
	}
	return r.jellyfinLibraries(ctx, res, j)
}

// jellyfinReady waits for /System/Info/Public, which carries no [Authorize] at
// the pinned tag and is where a client discovers a server, and reports whether
// the wizard is complete.
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

// jellyfinWizard names the person's account and closes the wizard, once.
func (r *Runner) jellyfinWizard(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin, userPassword string, done bool) error {
	if done {
		r.okStep(res, "jellyfin/wizard", "complete")
		return nil
	}
	started := false
	err := apps.Retry(ctx, startUpBudget, func(ctx context.Context) error {
		var e error
		started, e = j.SetFirstUser(ctx, c.Username, userPassword)
		return e
	})
	if err != nil {
		return r.ensure(res, "jellyfin/wizard", apps.Unchanged, err)
	}
	change := apps.Created
	detail := "account " + c.Username
	if started {
		// The first account already had a password: the wizard had run, or a
		// source box's database brought one. Either way it is not ours to rename.
		change = apps.Unchanged
		if name, nerr := j.StartupUser(ctx); nerr == nil && name != "" && !strings.EqualFold(name, c.Username) {
			detail = "first account is " + name
		}
	}
	if err := r.detailStep(res, "jellyfin/wizard", change, detail, nil); err != nil {
		return err
	}
	if err := j.AllowRemoteAccess(ctx); err != nil {
		return r.ensure(res, "jellyfin/remote-access", apps.Unchanged, err)
	}
	return r.ensure(res, "jellyfin/wizard-complete", apps.Unchanged, j.CompleteWizard(ctx))
}

// jellyfinAccount makes sure the automation account exists, is an
// administrator, and answers to the release Secret's password -- then signs in
// as it. Signing in as the person is the fallback rather than the rule: it is
// how a first run gets an administrator at all, and how a run recovers when the
// account's password was changed inside the app.
func (r *Runner) jellyfinAccount(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin, userPassword, boxPassword string) error {
	token, err := j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
	if err == nil {
		j.SetToken(token)
		r.okStep(res, "jellyfin/login", jellyfinBoxAccount)
		return r.jellyfinAPIKey(ctx, res, j)
	}
	person, err := j.AuthenticateByName(ctx, c.Username, userPassword)
	if err != nil {
		return r.ensure(res, "jellyfin/login", apps.Unchanged,
			fmt.Errorf("neither the box account nor %s signs in: %w", c.Username, err))
	}
	j.SetToken(person)
	if err := r.jellyfinMakeAccount(ctx, res, j, boxPassword); err != nil {
		return err
	}
	token, err = j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
	if err != nil {
		return r.ensure(res, "jellyfin/login", apps.Unchanged, err)
	}
	j.SetToken(token)
	r.okStep(res, "jellyfin/login", jellyfinBoxAccount)
	return r.jellyfinAPIKey(ctx, res, j)
}

// jellyfinMakeAccount creates the automation account, or puts the release
// Secret's password back on it. An administrator may set another account's
// password without knowing the old one (UserController.cs, UpdateUserPassword),
// which is why this needs no session of the account itself.
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

// jellyfinAPIKey mints the box's key once and re-reads it on every later run.
// Nothing in the release calls Jellyfin with it, so it is the person's handle on
// their own server -- and the proof this Job reaches the API as an
// administrator. The key is a credential: it is used and never recorded.
func (r *Runner) jellyfinAPIKey(ctx context.Context, res *Result, j *apps.Jellyfin) error {
	_, change, err := j.EnsureAPIKey(ctx, jellyfinBoxAccount)
	return r.ensure(res, "jellyfin/apikey", change, err)
}

// jellyfinProxies tells the server whose forwarded address to trust. An empty
// list is not "no proxies": it switches X-Forwarded-For off entirely, and this
// server is only ever reached through the Gateway, so the person's real address
// would never arrive and every request would look local.
func (r *Runner) jellyfinProxies(ctx context.Context, res *Result, c *JellyfinConfig, j *apps.Jellyfin) error {
	if len(c.KnownProxies) == 0 {
		res.Steps = append(res.Steps, Step{Name: "jellyfin/known-proxies", Result: stepPending,
			Detail: "apps.jellyfin.knownProxies is empty, so Jellyfin ignores X-Forwarded-For and every request through the Gateway counts as local; box writes the cluster's pod network on every apply, and a plain install sets its own"})
		r.stepLog("jellyfin/known-proxies", stepPending+" (empty)")
		return nil
	}
	change, obs, err := j.EnsureKnownProxies(ctx, c.KnownProxies)
	return r.detailStep(res, "jellyfin/known-proxies", change,
		fmt.Sprintf("%s (%s)", change, strings.Join(obs.After, ", ")), err)
}

// jellyfinLibraries gives each Arr instance's library directory a virtual
// folder. One library per directory, adopted when it already exists, so a
// migrated server keeps the name the person gave it.
func (r *Runner) jellyfinLibraries(ctx context.Context, res *Result, j *apps.Jellyfin) error {
	for _, lib := range r.Config.Jellyfin.Libraries {
		change, err := j.EnsureLibrary(ctx, lib.Name, lib.CollectionType, lib.Path)
		if err := r.ensure(res, "jellyfin/library/"+lib.Name, change, err); err != nil {
			return err
		}
	}
	return nil
}
