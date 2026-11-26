package wiring

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/apps"
)

// jellyfinBoxAccount is the account the wiring Job signs in as. It is not the
// person's: the Job must be able to connect on every re-run, and the person may
// change their own password at any time. It is the same name the chart gives the
// automation account of an app that keeps its own user table (s5n.wiringAccount),
// because it is the same thing -- the box's own account, not a person's.
const jellyfinBoxAccount = "s5n"

// stepPending is the receipt's word for a step that left something for a person
// to decide. It is the same word wiring.go uses for Plex's claim.
const (
	stepPending = "pending"
	stepSkipped = "skipped"
)

// wireJellyfin makes a Jellyfin server usable and keeps it that way.
//
// It is the one app here whose first conversation is a write: Jellyfin keeps its
// users, API keys and libraries in data/jellyfin.db, so no file can be sealed
// before it starts. The order is the whole design:
//
//  1. wait for the server and read whether its wizard is done. The wizard is
//     *not* this Job's: the app's own init container completes it before the
//     server the Service reaches ever starts (templates/workloads.yaml), so a
//     server still reporting an open wizard is one whose pod is not the pod
//     this Job should be wiring, and this Job refuses rather than completing it
//     from outside the pod;
//  2. keep an automation account with a password from the release Secret and
//     sign in as it. The person's password is used only for the first run's
//     administrator -- the init container named that account, and the Job needs
//     an administrator to make its own -- so the person changing it does not
//     break a later run. When neither password opens the server, nothing below
//     can run: that is the normal state of a migrated box, so it is reported as
//     a pending login with every session-bound step skipped, and the Job carries
//     on. An unreachable server or a 5xx is still a failure;
//  3. check that the account the box login names is on the server. A fresh box
//     has it (the init container named the first user after it); a migrated
//     server keeps the accounts it had, and this is reported, never repaired by
//     renaming or resetting one;
//  4. prove the server's view of the client address: with KnownProxies empty
//     Jellyfin ignores X-Forwarded-For completely, so every request through the
//     Gateway looks local and the endpoints a local caller may use without a
//     credential would be open to the internet;
//  5. one library per Arr instance, adopted by path when it already exists.
//
// A second run writes nothing: the wizard is already complete, the account and
// the key are looked up before they are made, and a library is adopted by the
// directory it holds.
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
		// No session, so everything below it is a step this run cannot take.
		// The receipt says so and the Job goes on: the apps wired after this one
		// have nothing to do with Jellyfin's accounts, and a migrated box whose
		// administrator password box does not hold is the normal case, not a
		// broken release.
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

// jellyfinNoSession is the receipt of a server box holds no password for: every
// step that needs a session says why it did not run, as one sentence the person
// can act on, and nothing is invented to look as if it worked.
//
// The string is shared by all of them on purpose: each row is a different part
// of the server, and repeating the whole reason four times is what makes a
// receipt unreadable. The reason names no secret -- box never prints the
// password the Secret holds, only where it is -- and no box command that does
// not exist: the app's own dashboard is where an administrator changes a
// password, and this Job cannot open a session to do it for them.
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

// jellyfinWizard checks the one thing this Job will not do itself: the app's
// first-run wizard. The chart's init container completes it inside the pod,
// before the server the Service reaches starts, and a pod does not become Ready
// while an init container runs. A server the Job can reach with the wizard still
// open is therefore a server something else started with its window open -- and
// completing the wizard over the in-cluster Service would leave that server
// answering the Gateway. So this refuses and says so.
func (r *Runner) jellyfinWizard(res *Result, done bool) error {
	if !done {
		return r.ensure(res, "jellyfin/wizard", apps.Unchanged,
			errors.New("the server's first-run wizard is not complete; its pod's startup-wizard init container is what completes it before the server starts, so a server this Job can reach with the wizard open was not started by this release"))
	}
	r.okStep(res, "jellyfin/wizard", "complete")
	return nil
}

// jellyfinPerson checks that the account the person was told to sign in with is
// on this server. The app's init container names the server's first account
// after the box login, so a fresh box has it. A migrated box keeps the accounts
// the source had -- the database rewriter renames nothing -- and gets box's own
// automation account beside them, so a login name that names no row there is a
// decision for the person: this reports it and creates nothing, because an
// account box invents would have a password box shows to nobody.
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

// jellyfinAccount makes sure the automation account exists, is an
// administrator, and answers to the release Secret's password -- then signs in
// as it. Signing in as the person is the fallback rather than the rule: it is
// how a first run gets an administrator at all, and how a run recovers when the
// account's password was changed inside the app.
//
// It reports false, with no error, when neither account signs in: a server whose
// accounts are the ones it arrived with, which is what a migrated box is. Only
// the app's own refusal of the *credential* means that (a 401 for
// /Users/AuthenticateByName); an unreachable server, a 5xx or a session the app
// refused for another reason is still a failed step.
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
	// The list is read, never written: the app builds its forwarded-header
	// handling from it once, at startup, so the chart's init container puts it
	// in network.xml before the app reads it and this step is what proves the
	// running server has it. A server without it is not failed -- it works, and
	// everything through the Gateway just looks local -- so the step says
	// pending and what fixing it takes.
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
