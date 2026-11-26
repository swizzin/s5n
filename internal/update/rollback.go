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

// Rollback is `box rollback <app>`: the hand-run step back for a fault no gate
// sees. It restores the app's newest snapshot, then leaves a hold that lapses
// when a release pins another image, or a values pin that stays until removed.
// It moves one app, never the release (docs/box.md, "Snapshot and rollback").
type Rollback struct {
	// Charts reads the installed release; it never lists or picks one.
	Charts release.ChartSource
	// Open returns the release handle for the grant's namespace.
	Open func(namespace string) (Release, error)
	// OpenCluster returns the release's StatefulSets, after the grant is read.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots returns the newest-copy store for the config root; nil means
	// this box cannot roll anything back.
	OpenSnapshots func(configRoot string, values map[string]any) (Snapshots, error)
	// Apply runs the one apply the rollback ends in.
	Apply ApplyFunc
	// Holds is the record of apps held at an older image; nil means only the
	// --pin path can finish.
	Holds HoldStore
	// Confirm asks the one question, before anything is stopped.
	Confirm release.Confirmer
	// Say is every line the rollback writes.
	Say say.Reporter

	// HealthWait bounds the health gate after the apply; zero is thirty-five
	// minutes, the chart's thirty plus room to settle.
	HealthWait time.Duration
	// SettleWait bounds the wait for the scaled-down app's pods to go. Zero
	// means five minutes.
	SettleWait time.Duration
	// CleanupWait bounds a failure's cleanup -- starting the app again, reading
	// where Helm left the release -- after the run's context is gone.
	CleanupWait time.Duration
	// Poll is how often the waits re-read state. Zero means two seconds.
	Poll time.Duration
	// Now is time.Now, so a test can pin the age box prints.
	Now func() time.Time
}

// RollbackOptions are one `box rollback`.
type RollbackOptions struct {
	// App is the app key, not one instance of it.
	App string
	// Apply is the apply the rollback ends in; its ChartVersion is overwritten
	// with the installed release.
	Apply release.Options
	// Pin writes the image into the values file, where it stays until removed.
	Pin bool
}

// RollbackResult is what one rollback read and did.
type RollbackResult struct {
	// App is the app key, Namespace the seedbox's from the grant.
	App       string
	Namespace string
	// Installed is the release Helm reports deployed, which is re-applied.
	Installed string
	// Image is what the newest snapshot says the app ran; Avoided is what the
	// step moved it to, which the hold compares a release against.
	Image   snapshot.ImageRef
	Avoided snapshot.ImageRef
	// Step is the step directory, with the versions its name records.
	Step     string
	From, To string
	Taken    time.Time
	// Dirs is each instance's copy, named when a rollback fails.
	Dirs []string
	// Pinned says this run wrote the pin into the values file instead of a
	// hold.
	Pinned bool

	// The restore half: the store, the copy, and the installed catalog's globs.
	snaps  Snapshots
	snap   AppSnapshot
	globs  []string
	root   string
	grant  string
	values string
}

// Snapshots is the copy a hand rollback reads and puts back; NewestSnapshots
// implements it.
type Snapshots interface {
	// Newest reads every enabled instance's newest snapshot; an uncopied one
	// has an empty Dir.
	Newest(ctx context.Context, app string) (AppSnapshot, error)
	// Restore puts the instances back; globs are what the snapshot's files are
	// matched against, and unmatched matches are removed.
	Restore(ctx context.Context, snap AppSnapshot, globs []string) error
}

// AppSnapshot is one app's newest copy: the step that took it, one copy per
// enabled instance.
type AppSnapshot struct {
	// App is the app key the copies belong to.
	App       string
	Instances []InstanceSnapshot
}

// InstanceSnapshot is one instance's newest copy; an empty Dir is a refusal
// the caller names by app.
type InstanceSnapshot struct {
	// Name is the config directory's name, the instance name.
	Name string
	// Dir is the snapshot directory, and Step the step directory holding it.
	Dir  string
	Step string
	// Manifest is the files the copy holds and the images it ran and moved to.
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

// Run reads what the rollback would do, asks once, then stops the app, restores
// the copy, writes the hold or pin, and re-applies the installed release.
//
// The order is what makes it safe: stopping before the restore means no writer
// and no pod can see half a database; writing the hold before the apply is what
// makes the chart render the snapshot's image.
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
	// Before the question: the answer loses everything written since the copy.
	r.report().Warn(fmt.Sprintf("%s's newest snapshot is %s old (taken %s, before %s -> %s). Rolling back puts %s back on %s and loses everything %s wrote since.",
		res.App, ageText(r.now().Sub(res.Taken)), res.Taken.UTC().Format("2006-01-02 15:04 UTC"),
		res.From, res.To, res.App, res.Image.Label(), res.App))
	ok, err := r.Confirm.Ask(fmt.Sprintf("roll %s back to %s?", res.App, res.Image.Label()), false)
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

	// Nothing was written yet, so start the app again rather than leaving it
	// down.
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
		// The pin is the stronger claim, so drop the hold: one that lapses would
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
	// Chart is nil so this pulls by version as a bare apply would; the hold or
	// pin makes it render the old image.
	applyOpts.Chart = nil
	applyOpts.ChartVersion = res.Installed
	if _, err := r.Apply(ctx, applyOpts); err != nil {
		err = fmt.Errorf("applying %s: %w", res.Installed, err)
		handle.Failed(err)
		return res, r.fail(ctx, rel, res, false, err)
	}
	// The gate, and nothing behind it: an old image that does not come up is
	// where box stops, not a second rollback.
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

// plan is every read and refusal, before anything is stopped or written.
func (r *Rollback) plan(ctx context.Context, opts RollbackOptions) (RollbackResult, error) {
	res := RollbackResult{App: opts.App}
	if r.Charts == nil {
		return res, errors.New("no chart source: box reads its chart from the registry")
	}
	if err := checkAppName(opts.App); err != nil {
		return res, err
	}
	// Files first: a bad grant is not worth a registry round trip.
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

	// Say nothing to do before the app goes down.
	if rendered, ok := renderedImage(chart, values, holds, opts.App); ok && sameImage(rendered, res.Image) {
		return res, fmt.Errorf("%s already runs %s", opts.App, res.Image.Label())
	}
	// Only --pin replaces a hand pin: overwriting one would undo the operator's
	// choice.
	if tag, digest := pinOf(values, opts.App); !opts.Pin && (tag != "" || digest != "") {
		return res, fmt.Errorf("%s is pinned in your values (%s); remove the pin, or pass --pin to replace it",
			opts.App, pinLabel(values, opts.App))
	}
	// Restore with the copy's own globs: a glob added since would delete files
	// the copy never held.
	res.snaps, res.snap, res.globs = snaps, found, latest.Manifest.Globs
	return res, nil
}

// newestOf is the one step every enabled instance has a copy in. It refuses
// rather than choosing: a partial step would restore one database over another
// instance's schema, and different steps have no single copy to put back.
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

// stepsText names each instance's step, which makes "different steps" actionable.
func stepsText(instances []InstanceSnapshot) string {
	parts := make([]string, 0, len(instances))
	for _, inst := range instances {
		parts = append(parts, fmt.Sprintf("%s from %s", inst.Name, inst.Step))
	}
	return strings.Join(parts, ", ")
}

// hold keeps the app on the snapshot's image until a release pins another.
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

// pin writes the image into the values file as a hand pin, keeping the
// reference whole; the release keeps updating around it.
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
		// A snapshot records an image or it is refused; this cannot happen.
		return fmt.Errorf("%s: the snapshot's image names neither a tag nor a digest", image.String())
	}
	return sb.SetValues(edits)
}

// startAgain puts the app back at one replica after a failure that never
// restored anything.
func (r *Rollback) startAgain(ctx context.Context, cluster Cluster, app string) {
	startApps(ctx, cluster, []string{app}, r.cleanupWait())
}

// fail ends a rollback that did not finish, saying what the app is on now and
// where the snapshot is. Nothing is retried: a second restore over a
// half-restored config is worse.
func (r *Rollback) fail(ctx context.Context, rel Release, res RollbackResult, stopped bool, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cleanupWait())
	defer cancel()
	msg := fmt.Sprintf("rolling %s back to %s: %v", res.App, res.Image.Label(), cause)
	if stopped {
		// Scaled to zero for the restore, and the apply never started it.
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

// ageText is a snapshot's age in one unit, rounded down.
func ageText(d time.Duration) string {
	if d < 0 {
		// A future stamp is a moved clock; "0 minutes" is the honest answer.
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

// stepVersions splits <from>-<to>-<timestamp>. A version can carry a hyphen
// (0.5.0-rc1), so the split is the first whose halves both parse as versions.
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

// renderedImage is what the installed chart renders for one app, resolved as
// charts/s5n/templates/_helpers.tpl does: a pinned digest replaces the
// variant's, a pinned tag drops the digest, then the hold overlay where nothing
// is pinned. A missing chart or variant says nothing, so the rollback goes ahead
// rather than refusing on a guess.
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
		// A pin box wrote names both; the chart renders both.
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

// sameImage: the repository must match, then the digest when either side names
// one (a re-pushed tag is new bytes), else the tag.
func sameImage(a, b snapshot.ImageRef) bool {
	if a.Repository != b.Repository {
		return false
	}
	if a.Digest != "" || b.Digest != "" {
		return a.Digest == b.Digest
	}
	return a.Tag == b.Tag
}

// checkEnabledApp refuses an app this seedbox does not run; an instance name is
// the likely slip, so the refusal names the app that holds it.
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

// checkAppName refuses an app that is not one path element: the key names a
// config directory a restore writes, so a separator or ".." would escape it.
func checkAppName(app string) error {
	switch {
	case app == "":
		return errors.New("no app: box rollback takes one app, `box rollback <app>`")
	case app == "." || app == ".." || strings.ContainsAny(app, `/\`):
		return fmt.Errorf("%q is not an app name: an app is one directory name under the config root", app)
	}
	return nil
}
