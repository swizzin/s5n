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

// wireJellyfin makes a Jellyfin server usable and keeps it that way.
//
// It is the one app here whose first conversation is a write: Jellyfin keeps its
// users, API keys and libraries in data/jellyfin.db, so no file can be sealed
// before it starts. The order is the whole design:
//
//  1. complete the first-run wizard with the person's account. Until that call
//     lands the server's /Startup/* and library endpoints answer anyone; the
//     chart is what keeps those paths off the Gateway (templates/workloads.yaml),
//     and this is the only caller that can reach them at all;
//  2. keep an automation account (jellyfinBoxAccount) with a password from the
//     release Secret, and sign in as it. The person's password is used for
//     step 1 and for recovery, and nothing else, so the person changing it does
//     not break wiring;
//  3. prove the server's view of the client address: with KnownProxies empty
//     Jellyfin ignores X-Forwarded-For completely, so every request through the
//     Gateway looks local and the endpoints a local caller may use without a
//     credential would be open to the internet;
//  4. one library per Arr instance, adopted by path when it already exists.
//
// A second run writes nothing: the wizard answers 403 once the first user has a
// password, the account and the key are looked up before they are made, and a
// library is adopted by the directory it holds.
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
	// /System/Info/Public answers before the wizard is complete and there is no
	// /health route at the pinned tag, so this is the readiness probe and the
	// wizard check at once.
	var ready apps.PublicInfo
	waitErr := apps.Retry(ctx, timeoutDuration(r.Config.TimeoutSeconds), func(ctx context.Context) error {
		info, err := j.Info(ctx)
		if err != nil {
			return err
		}
		ready = info
		return nil
	})
	if waitErr != nil {
		return r.ensure(res, "jellyfin/ready", apps.Unchanged, waitErr)
	}
	r.okStep(res, "jellyfin/ready", fmt.Sprintf("version=%s wizard=%v", ready.Version, ready.StartupWizardCompleted))

	if !ready.StartupWizardCompleted {
		// The first user exists from the server's own initialisation, but a
		// request that arrives in the moment before it does answers 404, so the
		// call is retried over a short budget rather than read as a failure.
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
			// The first account already had a password: the wizard had run, or
			// a source box's database brought one. Either way the account is not
			// ours to rename.
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
		if err := j.CompleteWizard(ctx); err != nil {
			return r.ensure(res, "jellyfin/wizard", apps.Unchanged, err)
		}
	} else {
		r.okStep(res, "jellyfin/wizard", "complete")
	}

	// The automation account, and the session every later call uses. Signing in
	// as the person is the fallback rather than the rule: it is how a first run
	// gets an administrator at all, and how a run recovers when the account's
	// password was changed inside the app.
	adminToken, err := j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
	change := apps.Unchanged
	if err != nil {
		personToken, perr := j.AuthenticateByName(ctx, c.Username, userPassword)
		if perr != nil {
			return r.ensure(res, "jellyfin/login", apps.Unchanged,
				fmt.Errorf("neither the box account nor %s signs in: %w", c.Username, err))
		}
		j.SetToken(personToken)
		account, found, err := j.FindUser(ctx, jellyfinBoxAccount)
		switch {
		case err != nil:
			return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
		case !found:
			if account, err = j.CreateUser(ctx, jellyfinBoxAccount, boxPassword); err != nil {
				return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
			}
			change = apps.Created
		default:
			// The account exists and the release Secret's password is not its
			// own: put it back. An administrator may set another account's
			// password without knowing the old one (UserController.cs,
			// UpdateUserPassword), which is why this needs no session of the
			// account itself.
			if err := j.SetPassword(ctx, account.ID, boxPassword); err != nil {
				return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
			}
			change = apps.Updated
		}
		if _, err := j.SetAdministrator(ctx, account.ID); err != nil {
			return r.ensure(res, "jellyfin/account", apps.Unchanged, err)
		}
		if err := r.ensure(res, "jellyfin/account", change, nil); err != nil {
			return err
		}
		adminToken, err = j.AuthenticateByName(ctx, jellyfinBoxAccount, boxPassword)
		if err != nil {
			return r.ensure(res, "jellyfin/login", apps.Unchanged, err)
		}
	}
	j.SetToken(adminToken)
	r.okStep(res, "jellyfin/login", jellyfinBoxAccount)

	// The forwarded header. An empty list is not "no proxies": it switches
	// X-Forwarded-For off entirely, and this server is only ever reached through
	// the Gateway, so the person's real address would never arrive.
	if len(c.KnownProxies) == 0 {
		res.Steps = append(res.Steps, Step{Name: "jellyfin/known-proxies", Result: "pending",
			Detail: "apps.jellyfin.knownProxies is empty, so Jellyfin ignores X-Forwarded-For and every request through the Gateway counts as local; box writes the cluster's pod network on every apply, and a plain install sets its own"})
		r.stepLog("jellyfin/known-proxies", "pending (empty)")
	} else {
		change, obs, err := j.EnsureKnownProxies(ctx, c.KnownProxies)
		if err := r.detailStep(res, "jellyfin/known-proxies", change,
			fmt.Sprintf("%s (%s)", change, strings.Join(obs.After, ", ")), err); err != nil {
			return err
		}
	}

	// The box's own API key: minted once, then re-read on every run. Nothing in
	// the release calls Jellyfin with it, so it is the person's handle on their
	// own server -- and the proof this Job can reach the API as an administrator
	// without a session.
	key, change, err := j.EnsureAPIKey(ctx, jellyfinBoxAccount)
	if err := r.ensure(res, "jellyfin/apikey", change, err); err != nil {
		return err
	}
	// The key is a credential: it is used here and never recorded. The receipt
	// says which account it belongs to, not what it is.
	_ = key
	j.SetToken(adminToken)

	// One library per Arr instance. A Jellyfin library is a virtual folder over
	// directories, and the directory this one holds is the Arr's own library
	// root -- the same path the data contract creates, so an import and a
	// played file are the same file on disk.
	for _, lib := range c.Libraries {
		change, err := j.EnsureLibrary(ctx, lib.Name, lib.CollectionType, lib.Path)
		if err := r.ensure(res, "jellyfin/library/"+lib.Name, change, err); err != nil {
			return err
		}
	}
	return nil
}
