package update

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/snapshot"
)

// Rollback is `box rollback <app>`: the hand-run version of the step-back update
// does by itself.
//
// A step rolls an app back when its new image keeps exiting, which is the one
// failure the health gate can see. A fault no gate sees -- an app that starts,
// serves and then mangles its own database -- is the operator's call, and this
// is the command for it. It restores the app's newest snapshot and then either
// leaves the hold a step-back would leave, which lapses when a release pins
// another image, or pins the image in the values file, which stays until the
// operator removes it.
//
// It is one app and one copy. The rest of the release stays where it is, and
// the release itself is never moved: the app runs its old image under the chart
// that is installed (docs/box.md, "Snapshot and rollback").
type Rollback struct {
	// Charts is the chart repository. A rollback reads only the release that
	// is installed -- it neither lists releases nor picks one.
	Charts release.ChartSource
	// Open returns the release handle for the grant's namespace.
	Open func(namespace string) (Release, error)
	// OpenCluster returns the release's StatefulSets. The namespace comes from
	// the grant, so it can only be opened after the files are read.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots returns the newest-copy store for the grant's config root,
	// with the effective values this run planned with (the instance names come
	// from them). nil is a box that cannot roll anything back.
	OpenSnapshots func(configRoot string, values map[string]any) (Snapshots, error)
	// Apply runs the one apply the rollback ends in.
	Apply ApplyFunc
	// Holds is the record of apps a rollback holds at an older image. The
	// default rollback writes one; nil is a box that keeps none, which only
	// the --pin path can finish.
	Holds HoldStore
	// Confirm asks the one question, before anything is stopped.
	Confirm release.Confirmer
	// Say is every line the rollback writes.
	Say say.Reporter

	// HealthWait bounds the wait for the release to be healthy after the
	// apply. Zero means thirty-five minutes, as a step's gate does: the app
	// that comes back has the chart's thirty minutes to open its port.
	HealthWait time.Duration
	// SettleWait bounds the wait for the scaled-down app's pods to go. Zero
	// means five minutes.
	SettleWait time.Duration
	// CleanupWait bounds what a failure does after it: starting the app again
	// when nothing was restored, and reading where Helm left the release. It
	// runs after the context that would have cancelled it is gone. Zero means
	// five minutes.
	CleanupWait time.Duration
	// Poll is how often the waits re-read state. Zero means two seconds.
	Poll time.Duration
	// Now is time.Now, so a test can pin the age box prints.
	Now func() time.Time
}

// RollbackOptions are one `box rollback`.
type RollbackOptions struct {
	// App is the app key the operator named: the update app, not one instance
	// of it.
	App string
	// Apply is the apply the rollback ends in, as apply's own options. Its
	// ChartVersion is overwritten with the installed release: a rollback never
	// chooses a release.
	Apply release.Options
	// Pin writes the image into the values file instead of the hold, so it
	// holds until the operator removes it rather than until a release pins
	// another image.
	Pin bool
}

// RollbackResult is what one rollback read and did.
type RollbackResult struct {
	// App is the app key, Namespace the seedbox's from the grant.
	App       string
	Namespace string
	// Installed is the chart release Helm reports deployed, which the
	// rollback re-applies: it never moves the release.
	Installed string
	// Image is the image the newest snapshot says the app ran, and Avoided
	// the image the step moved it to -- what the hold compares a release
	// against.
	Image   snapshot.ImageRef
	Avoided snapshot.ImageRef
	// Step is the step directory the snapshot came from, with the two chart
	// versions its name records and when the copy was taken.
	Step     string
	From, To string
	Taken    time.Time
	// Dirs is each instance's copy, for box's sentence when a rollback fails:
	// the operator needs to know where to look.
	Dirs []string
	// Pinned says this run wrote the pin into the values file instead of a
	// hold.
	Pinned bool

	// snaps, snap and globs are what the restore half needs and no caller
	// reads: the store the copy was found in, the copy itself, and the files
	// the installed release's catalog names under the app's config directory.
	snaps  Snapshots
	snap   AppSnapshot
	globs  []string
	root   string
	grant  string
	values string
}

// Snapshots is the copy a hand rollback reads: the newest snapshot of each of an
// app's instances, and putting it back. NewestSnapshots implements it over
// internal/snapshot; the interface is what lets a test put the restore in the
// same ordered log as the stop, the wait and the apply.
type Snapshots interface {
	// Newest reads the newest snapshot of every instance the values enable. An
	// instance no step copied comes back with an empty Dir; a store that cannot
	// read one at all is an error.
	Newest(ctx context.Context, app string) (AppSnapshot, error)
	// Restore puts each of the app's instances back to the copy its step took.
	// globs is what the installed release's catalog names under the app's
	// config directory: Restore removes the files they match that the snapshot
	// does not hold.
	Restore(ctx context.Context, snap AppSnapshot, globs []string) error
}

// AppSnapshot is one app's newest copy: the step that took it, one copy per
// enabled instance.
type AppSnapshot struct {
	// App is the app key the copies belong to.
	App       string
	Instances []InstanceSnapshot
}

// InstanceSnapshot is one instance's newest copy. Dir is empty when that
// instance has no snapshot at all, which is a refusal the caller names by app
// rather than a copy it restores.
type InstanceSnapshot struct {
	// Name is the config directory's name, which is the instance name
	// (sonarr, sonarr-4k).
	Name string
	// Dir is the snapshot directory, and Step the step directory holding it.
	Dir  string
	Step string
	// Manifest is the copy's manifest: the files it holds and the images the
	// app ran and moved to.
	Manifest snapshot.Manifest
}

func (r *Rollback) report() say.Reporter { return say.Or(r.Say, nil) }

func (r *Rollback) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rollback) healthWait() time.Duration { return orDuration(r.HealthWait, DefaultHealthWait) }

func (r *Rollback) settleWait() time.Duration { return orDuration(r.SettleWait, 5*time.Minute) }

func (r *Rollback) cleanupWait() time.Duration { return orDuration(r.CleanupWait, 5*time.Minute) }

func (r *Rollback) poll() time.Duration { return orDuration(r.Poll, 2*time.Second) }

// Run reads what the rollback would do, says it, asks once, and then stops the
// app, restores the copy, writes the hold or the pin, and re-applies the
// release that is installed.
//
// The order is the one thing that makes it safe. The app is stopped before
// anything is written, so the restore never runs against a writer and a pod
// that came back mid-restore cannot read half a database. The hold (or pin) is
// written before the apply, because that is what makes the chart render the
// image the snapshot recorded. Nothing is put back after the apply.
func (r *Rollback) Run(ctx context.Context, opts RollbackOptions) (RollbackResult, error) {
	res, err := r.plan(ctx, opts)
	if err != nil {
		return res, err
	}
	if r.Confirm == nil {
		return res, errors.New("rollback needs a confirm: it writes to a cluster")
	}
	if r.Apply == nil {
		return res, errors.New("rollback needs an apply: the release has to re-render the image it put back")
	}
	if r.OpenCluster == nil {
		return res, errors.New("rollback needs a workload client: it stops the app first")
	}
	// Before the question, because the answer loses data: everything the app
	// wrote since the copy is what a restore drops.
	r.report().Warn(fmt.Sprintf("%s's newest snapshot is %s old (taken %s, before %s -> %s). Rolling back puts %s back on %s and loses everything %s wrote since.",
		res.App, ageText(r.now().Sub(res.Taken)), res.Taken.UTC().Format("2006-01-02 15:04 UTC"),
		res.From, res.To, res.App, res.Image.Label(), res.App))
	ok, err := r.Confirm.Ask(fmt.Sprintf("roll %s back to %s?", res.App, res.Image.Label()))
	if err != nil {
		return res, err
	}
	if !ok {
		r.report().Line("oh heck, nvm")
		return res, release.ErrCancelled
	}
	rel, err := r.Open(res.Namespace)
	if err != nil {
		return res, err
	}
	cluster, err := r.OpenCluster(res.Namespace)
	if err != nil {
		return res, err
	}
	handle := r.report().Start(say.Op{Label: fmt.Sprintf("rolling %s back to %s", res.App, res.Image.Label())})

	// A failure down to the restore is this run's own doing: it stopped an app
	// nothing has been written for yet, so it starts it again rather than
	// leaving a seedbox with the app down.
	if err := scaleApps(ctx, cluster, []string{res.App}, 0); err != nil {
		r.startAgain(ctx, cluster, res.App)
		err = fmt.Errorf("stopping %s: %w", res.App, err)
		handle.Failed(err)
		return res, err
	}
	if err := waitGone(ctx, cluster, []string{res.App}, r.settleWait(), r.poll()); err != nil {
		r.startAgain(ctx, cluster, res.App)
		err = fmt.Errorf("waiting for %s to stop: %w", res.App, err)
		handle.Failed(err)
		return res, err
	}
	if err := res.snaps.Restore(ctx, res.snap, res.globs); err != nil {
		err = fmt.Errorf("restoring its config: %w", err)
		handle.Failed(err)
		return res, r.fail(ctx, rel, res, true, err)
	}
	if opts.Pin {
		if err := r.pin(res.grant, res.values, res.App, res.Image); err != nil {
			err = fmt.Errorf("pinning %s in your values: %w", res.App, err)
			handle.Failed(err)
			return res, r.fail(ctx, rel, res, true, err)
		}
		// The values file is the stronger claim, so the hold goes: leaving it
		// would be the same answer written twice, and a hold that lapses would
		// read as a pin that did not.
		if err := r.dropHold(res.root, res.App); err != nil {
			err = fmt.Errorf("removing %s's hold: %w", res.App, err)
			handle.Failed(err)
			return res, r.fail(ctx, rel, res, true, err)
		}
	} else if err := r.hold(res.root, res); err != nil {
		err = fmt.Errorf("holding %s on %s: %w", res.App, res.Image.Label(), err)
		handle.Failed(err)
		return res, r.fail(ctx, rel, res, true, err)
	}

	applyOpts := opts.Apply
	// The release that is installed, with the operator's files untouched.
	// Chart is nil so this apply pulls by version exactly as a bare apply
	// would, and the hold or the pin is what makes it render the old image.
	applyOpts.Chart = nil
	applyOpts.ChartVersion = res.Installed
	if _, err := r.Apply(ctx, applyOpts); err != nil {
		err = fmt.Errorf("applying %s: %w", res.Installed, err)
		handle.Failed(err)
		return res, r.fail(ctx, rel, res, false, err)
	}
	// The gate, and nothing behind it: this is the app the operator put back by
	// hand, so an old image that does not come up either is where box stops
	// rather than a second rollback.
	outcome, err := waitHealthy(ctx, rel, cluster, []string{res.App}, r.healthWait(), r.poll())
	var cause error
	switch {
	case err != nil:
		cause = err
	case len(outcome.Exiting) > 0:
		cause = fmt.Errorf("%s exited %d times on %s", res.App, outcome.Exiting[0].Restarts, res.Image.Label())
	case outcome.TimedOut:
		cause = fmt.Errorf("the release is not deployed and healthy after %s", r.healthWait())
	}
	if cause != nil {
		handle.Failed(cause)
		return res, r.fail(ctx, rel, res, false, cause)
	}
	res.Pinned = opts.Pin
	handle.Done(doneLine(res))
	return res, nil
}

// plan is every read a rollback makes, and every refusal: the files, the
// deployed release, its chart, the newest copy and what the release renders
// today. Nothing is stopped and nothing is written, so a refusal leaves the box
// exactly as it was.
func (r *Rollback) plan(ctx context.Context, opts RollbackOptions) (RollbackResult, error) {
	res := RollbackResult{App: opts.App}
	if r.Charts == nil {
		return res, errors.New("no chart source: box reads its chart from the registry")
	}
	if err := checkAppName(opts.App); err != nil {
		return res, err
	}
	// The files first: a grant that does not load, or a values file the chart
	// refuses, is the operator's problem and is not worth a registry round
	// trip.
	sb, err := seedbox.Load(opts.Apply.GrantPath, opts.Apply.ValuesPath)
	if err != nil {
		return res, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return res, err
	}
	res.Namespace = namespace
	root, err := ConfigRoot(sb)
	if err != nil {
		return res, err
	}
	res.root, res.grant, res.values = root, opts.Apply.GrantPath, opts.Apply.ValuesPath
	if r.Open == nil {
		return res, errors.New("rollback needs a release handle: it reads what Helm has deployed")
	}
	rel, err := r.Open(namespace)
	if err != nil {
		return res, err
	}
	installed, err := rel.State(ctx)
	if err != nil {
		return res, err
	}
	if !installed.Exists {
		return res, fmt.Errorf("no %s release is deployed in %s: a rollback puts one app back inside the release that is there",
			release.ReleaseName, namespace)
	}
	res.Installed = installed.ChartVersion
	chart, err := r.Charts.Load(ctx, release.ChartRef{Repo: opts.Apply.ChartRepo, Version: installed.ChartVersion})
	if err != nil {
		return res, fmt.Errorf("the box is on chart %s, which cannot be read: %w", installed.ChartVersion, err)
	}
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return res, err
	}
	if err := checkEnabledApp(root, values, opts.App); err != nil {
		return res, err
	}
	holds, err := loadHolds(r.Holds, root)
	if err != nil {
		return res, err
	}
	if r.OpenSnapshots == nil {
		return res, fmt.Errorf("%s cannot be rolled back on this box: it has no snapshot store", opts.App)
	}
	snaps, err := r.OpenSnapshots(root, values)
	if err != nil {
		return res, err
	}
	if snaps == nil {
		return res, fmt.Errorf("%s cannot be rolled back on this box: it has no snapshot store", opts.App)
	}
	found, err := snaps.Newest(ctx, opts.App)
	if err != nil {
		return res, err
	}
	latest, err := newestOf(opts.App, found)
	if err != nil {
		return res, err
	}
	res.Image = latest.Manifest.Image
	res.Avoided = latest.Manifest.MovedTo
	res.Step = latest.Step
	res.Taken = latest.Manifest.Taken
	from, to, ok := stepVersions(latest.Step)
	if !ok {
		return res, fmt.Errorf("%s's newest snapshot is in %s, which does not name the releases it moved between",
			opts.App, latest.Step)
	}
	res.From, res.To = from, to
	for _, inst := range found.Instances {
		if inst.Dir != "" {
			res.Dirs = append(res.Dirs, inst.Dir)
		}
	}

	// Nothing to do is worth saying before the app goes down: the release
	// already renders the image the snapshot would put back.
	if rendered, ok := renderedImage(chart, values, holds, opts.App); ok && sameImage(rendered, res.Image) {
		return res, fmt.Errorf("%s already runs %s", opts.App, res.Image.Label())
	}
	// A hand pin is the operator's decision about this app, so only --pin
	// replaces it: silently writing over one would undo a choice nobody asked
	// box to revisit.
	if tag, digest := pinOf(values, opts.App); !opts.Pin && (tag != "" || digest != "") {
		return res, fmt.Errorf("%s is pinned in your values (%s); remove the pin, or pass --pin to replace it",
			opts.App, pinLabel(values, opts.App))
	}
	// The copy is restored with the globs it was taken with, not with what the
	// installed release's catalog names now: a restore removes every match the
	// copy lacks, so a glob added since would delete files it never held.
	res.snaps, res.snap, res.globs = snaps, found, latest.Manifest.Globs
	return res, nil
}

// newestOf is the one copy a rollback uses: the step every enabled instance has
// a copy in.
//
// It refuses rather than choosing. A step that holds only some of an app's
// instances would restore one database over another instance's schema, and
// instances whose newest copies come from different steps have no single step
// box can put back -- an older step that happens to hold them all is state the
// app has already written past.
func newestOf(app string, found AppSnapshot) (InstanceSnapshot, error) {
	var have []InstanceSnapshot
	var missing []string
	for _, inst := range found.Instances {
		if inst.Dir == "" {
			missing = append(missing, inst.Name)
			continue
		}
		have = append(have, inst)
	}
	if len(have) == 0 {
		return InstanceSnapshot{}, fmt.Errorf("%s has no snapshot to roll back to", app)
	}
	if len(missing) > 0 {
		return InstanceSnapshot{}, fmt.Errorf("%s's newest snapshot is missing %s", app, strings.Join(missing, ", "))
	}
	first := have[0]
	for _, inst := range have[1:] {
		if inst.Step != first.Step {
			return InstanceSnapshot{}, fmt.Errorf("%s's instances were last copied by different steps, so there is no one copy to put back: %s",
				app, stepsText(have))
		}
	}
	if first.Manifest.Image.IsZero() {
		return InstanceSnapshot{}, fmt.Errorf("%s's newest snapshot does not record which image wrote it, so box cannot say what to roll back to", app)
	}
	if len(first.Manifest.Globs) == 0 {
		return InstanceSnapshot{}, fmt.Errorf("%s's newest snapshot does not record what it copied, so box cannot restore it without risking files it never held", app)
	}
	return first, nil
}

// stepsText names which step each instance's newest copy came from, in
// instance order: the one fact that makes "different steps" actionable.
func stepsText(instances []InstanceSnapshot) string {
	parts := make([]string, 0, len(instances))
	for _, inst := range instances {
		parts = append(parts, fmt.Sprintf("%s from %s", inst.Name, inst.Step))
	}
	return strings.Join(parts, ", ")
}

// hold writes the hold a step-back would write: the app stays on the image the
// snapshot recorded until a release pins another, and the release the rollback
// applies is what renders it.
func (r *Rollback) hold(root string, res RollbackResult) error {
	if r.Holds == nil {
		return errors.New("this box keeps no holds, so the app could not stay on its old image; pass --pin to write the image into your values instead")
	}
	holds, err := r.Holds.Load(root)
	if err != nil {
		return err
	}
	holds = snapshot.WithHold(holds, snapshot.Hold{
		App:      res.App,
		At:       res.Image,
		Avoiding: res.Avoided,
		Snapshot: res.Step,
		Since:    r.now(),
		By:       snapshot.ByRollback,
	})
	return r.Holds.Save(root, holds)
}

// dropHold removes one app's hold after a pin has taken its place. A hold it
// did not have is not a change.
func (r *Rollback) dropHold(root, app string) error {
	if r.Holds == nil {
		return nil
	}
	holds, err := r.Holds.Load(root)
	if err != nil {
		return err
	}
	kept := snapshot.WithoutHold(holds, app)
	if len(kept) == len(holds) {
		return nil
	}
	return r.Holds.Save(root, kept)
}

// pin writes the image into the values file as an ordinary hand pin: the digest
// the release pins, and the tag it was published under, in the one write that
// keeps the reference whole. A pin is an override on whatever release is
// installed, so the release keeps updating around it.
func (r *Rollback) pin(grantPath, valuesPath, app string, image snapshot.ImageRef) error {
	sb, err := seedbox.Load(grantPath, valuesPath)
	if err != nil {
		return err
	}
	var edits []seedbox.Edit
	if image.Digest != "" {
		edits = append(edits, seedbox.Edit{Path: "apps." + app + ".image.digest", Value: image.Digest})
	}
	if image.Tag != "" {
		edits = append(edits, seedbox.Edit{Path: "apps." + app + ".image.tag", Value: image.Tag})
	}
	if len(edits) == 0 {
		// A snapshot records an image or it is refused, so this is a
		// reference no manifest box accepted could carry.
		return fmt.Errorf("%s: the snapshot's image names neither a tag nor a digest", image.String())
	}
	return sb.SetValues(edits)
}

// startAgain puts the app back at one replica after a failure that never
// restored anything.
func (r *Rollback) startAgain(ctx context.Context, cluster Cluster, app string) {
	startApps(ctx, cluster, []string{app}, r.cleanupWait())
}

// fail ends a rollback that did not finish. It says what the app is on now --
// Helm's own answer, read after the failure -- and where the snapshot is, which
// is what the operator needs to finish the job by hand. Nothing is retried: a
// second restore over a half-restored config is worse than the failure it would
// be undoing.
func (r *Rollback) fail(ctx context.Context, rel Release, res RollbackResult, stopped bool, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cleanupWait())
	defer cancel()
	msg := fmt.Sprintf("rolling %s back to %s: %v", res.App, res.Image.Label(), cause)
	if stopped {
		// The app was scaled to zero for the restore and this run never got as
		// far as the apply that starts it again.
		msg += fmt.Sprintf("; %s is stopped", res.App)
	} else {
		state, stateErr := rel.State(cctx)
		switch {
		case stateErr != nil:
			msg += fmt.Sprintf("; the release's state could not be read (%v)", stateErr)
		case !state.Exists:
			msg += "; no release is deployed"
		default:
			msg += fmt.Sprintf("; %s is on %s (revision %d, %s)", release.ReleaseName, state.ChartVersion, state.Revision, state.Status)
		}
	}
	return errors.New(msg + fmt.Sprintf("; %s's snapshot is at %s", res.App, strings.Join(res.Dirs, ", ")))
}

// doneLine is the rollback's last line: where the app is, and what holds it
// there.
func doneLine(res RollbackResult) string {
	if res.Pinned {
		return fmt.Sprintf("%s is on %s, pinned in %s. Remove the pin from apps.%s.image when you want it to move again.",
			res.App, res.Image.Label(), res.values, res.App)
	}
	return fmt.Sprintf("%s is on %s. It stays there until a release pins something other than %s.",
		res.App, res.Image.Label(), res.Avoided.Label())
}

// ageText is how old a snapshot is, in the one unit the operator reads it in:
// minutes under an hour, hours under a day, days after that, always rounded
// down because "2 days" for a copy taken yesterday evening is the honest
// answer.
func ageText(d time.Duration) string {
	if d < 0 {
		// A copy stamped in the future is a clock that moved, not a younger
		// snapshot; saying "0 minutes" is the only answer that is not a lie.
		d = 0
	}
	switch {
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute", "minutes")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour", "hours")
	default:
		return plural(int(d.Hours()/24), "day", "days")
	}
}

// stepVersions reads the two chart releases a step directory's name records:
// <from>-<to>-<timestamp>.
//
// A version can carry a hyphen of its own (0.5.0-rc1), so the split is the first
// one whose halves both parse as versions -- the leftmost, because a version
// starts with digits and the prerelease tail of the left half cannot.
func stepVersions(step string) (from, to string, ok bool) {
	end := strings.LastIndex(step, "-")
	if end < 0 {
		return "", "", false
	}
	rest := step[:end]
	for i, r := range rest {
		if r != '-' {
			continue
		}
		from, to := rest[:i], rest[i+1:]
		if isVersion(from) && isVersion(to) {
			return from, to, true
		}
	}
	return "", "", false
}

func isVersion(s string) bool {
	_, err := semver.NewVersion(s)
	return err == nil
}

// renderedImage is the image the installed release renders for one app today:
// the variant the values select, resolved the way
// charts/s5n/templates/_helpers.tpl resolves it -- a pinned digest replaces the
// variant's, a pinned tag replaces its tag and drops its digest -- and then the
// hold overlay, which only applies where the values pin nothing (the precedence
// HoldValues uses).
//
// A chart with no variants, or one without this app's variant, says nothing:
// box then cannot tell whether the app already runs the image, and the rollback
// goes ahead rather than refusing on a guess.
func renderedImage(chart release.Chart, values map[string]any, holds []snapshot.Hold, app string) (snapshot.ImageRef, bool) {
	if len(chart.Variants) == 0 {
		return snapshot.ImageRef{}, false
	}
	images, err := admission.Images(chart.Variants)
	if err != nil {
		return snapshot.ImageRef{}, false
	}
	base, ok := images[app][variantOf(values, app)]
	if !ok {
		return snapshot.ImageRef{}, false
	}
	ref := snapshot.ImageRef{Repository: base.Repository, Tag: base.Tag, Digest: base.Digest}
	tag, digest := pinOf(values, app)
	switch {
	case digest != "":
		// The digest replaces the variant's, and a pinned tag replaces its
		// tag too: a pin box wrote (a rollback's, a hold's) names both, and
		// the chart renders both (charts/s5n/templates/_helpers.tpl).
		ref.Digest = digest
		if tag != "" {
			ref.Tag = tag
		}
	case tag != "":
		ref.Tag = tag
		ref.Digest = ""
	}
	if h, ok := holdFor(holds, app); ok && tag == "" && digest == "" && !holdLapsed(ref, h) {
		ref = h.At
	}
	return ref, true
}

// sameImage reports whether two references name the same image: the repository
// must match, and the digest when either side names one, because the chart pins
// digest and a re-pushed tag is new bytes. Without a digest on either side the
// tag is what names the image.
func sameImage(a, b snapshot.ImageRef) bool {
	if a.Repository != b.Repository {
		return false
	}
	if a.Digest != "" || b.Digest != "" {
		return a.Digest == b.Digest
	}
	return a.Tag == b.Tag
}

// checkEnabledApp refuses a name that is not an app this seedbox runs. An
// instance name (sonarr-4k) is the likely slip: every instance of an app runs the
// one image the app pins, so a rollback is per app and puts all of them back
// together, and the refusal says which app to name.
func checkEnabledApp(root string, values map[string]any, app string) error {
	enabled := release.EnabledApps(values)
	for _, name := range enabled {
		if name == app {
			return nil
		}
	}
	for _, name := range enabled {
		for _, dir := range setup.SettingsDirs(root, []string{name}, values) {
			if filepath.Base(dir) == app {
				return fmt.Errorf("%s is an instance of %s: every instance of an app runs one image, so `box rollback %s` puts all of them back together", app, name, name)
			}
		}
	}
	return fmt.Errorf("%s is not an app this seedbox runs: box rollback takes an app from your values (%s)", app, strings.Join(enabled, ", "))
}

// checkAppName refuses an app that is not one path element. The app key names a
// config directory under the grant's root, which is a write a restore performs,
// so a name with a separator (or a "..") would point that write somewhere else.
func checkAppName(app string) error {
	switch {
	case app == "":
		return errors.New("no app: box rollback takes one app, `box rollback <app>`")
	case app == "." || app == ".." || strings.ContainsAny(app, `/\`):
		return fmt.Errorf("%q is not an app name: an app is one directory name under the config root", app)
	}
	return nil
}
