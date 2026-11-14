// Package update is `box update`: it moves a seedbox from the chart release it
// is running to a newer one.
//
// The installed release is what Helm has deployed, never what box remembers
// installing. The target is the newest published release this box meets
// `box.s5n/min-box` for, or the release `--chart-version` names. The releases'
// own `box.s5n/upgrade-from` floors decide how many steps there are: an old box
// is carried through the intermediate releases it may be upgraded from, each
// with its own apply, wiring gate and health gate, because deleting an old
// migration is only safe if every box that needs it has passed through it.
//
// The values file is the operator's and is never rewritten. What a step moves
// is the release: image digests, platform pins, migrations. A step that moves
// an image snapshots that app's config first, with the app scaled to zero, so
// the operator's own rollback has a copy of what the new image migrated; that
// snapshot is the only reason going back over a forward-only database is real
// rather than nominal.
//
// box does not go back by itself. A gate that fails stops the update and says
// where the release is, because the health gate cannot tell a slow migration
// from a failed one and undoing a migration that was about to finish is worse
// than waiting for someone to look. The only thing this package puts back is
// the apps a step stopped before the release ever moved.
//
// Like internal/release, the decision is made over small interfaces -- a chart
// source, a release handle, the workload set, a snapshotter, an apply and a
// state store -- so every branch, including "a failed gate writes nothing to
// the release", is tested from fakes without a registry or a cluster.
package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/wait"
)

// ChartSource is where update reads the repository: one release by version
// (apply's ChartSource), and every published release with the annotations the
// update plan needs. release.OCIChart implements both.
type ChartSource interface {
	release.ChartSource
	Releases(ctx context.Context, ref release.ChartRef) ([]release.Published, error)
}

// Release is the deployed release as update reads it: what Helm says is
// running. release.Helm implements it. This package never moves the release
// back -- a failed gate stops the update where it is and says where the box
// was left -- so the interface holds the read alone.
type Release interface {
	State(ctx context.Context) (release.ReleaseState, error)
}

// Cluster is the release's StatefulSets, as an update writes and reads them.
// It is one interface rather than three clients because the three questions
// are one conversation: scale an app down, wait for its pods to go, and later
// ask whether everything is ready.
type Cluster interface {
	// Scale sets the replicas of every StatefulSet of one app -- both Arr
	// instances included -- in the release's namespace. An app with no
	// StatefulSet there (the values just enabled it) is not an error: there
	// is nothing of it to stop.
	Scale(ctx context.Context, app string, replicas int32) error
	// Gone reports whether every pod of that app is gone.
	Gone(ctx context.Context, app string) (bool, error)
	// Healthy reports whether every StatefulSet of the release is ready and
	// has rolled out its current generation.
	Healthy(ctx context.Context) (bool, error)
}

// Snapshotter copies the apps a step moves out of the way, so the operator has
// a copy of whatever the new image migrated and can put it back with a command
// of their own. internal/snapshot implements it; this package ships only the
// interface and a test fake, and cmd/box wires the implementation in.
type Snapshotter interface {
	// Take copies the named apps' config for this step, with the apps
	// stopped. It must fail rather than leave a partial snapshot.
	Take(ctx context.Context, step Step, apps []string) error
	// Prune keeps the newest two snapshots per app.
	Prune(ctx context.Context, step Step, apps []string) error
}

// ApplyFunc runs one step's apply. It is release.Applier.Apply in cmd/box: the
// step installs its release through the same plan, diff, write and wiring gate
// an operator's `box apply` does, with the operator's files unchanged.
type ApplyFunc func(ctx context.Context, opts release.Options) (release.Result, error)

// Options are one `box update`.
type Options struct {
	// Apply is the apply every step runs. Its ChartVersion is the operator's
	// --chart-version, which pins the final target; each step overwrites it
	// with the release that step installs. The grant and values paths are
	// apply's own.
	Apply release.Options
	// Check is `box update --check`: plan and report, write nothing but
	// lastCheck.
	Check bool
}

// The gates one step has to pass, as the record and box's sentence name them.
// A step fails at exactly one of them: the apply never moved the release, the
// wiring receipt says the release did not work, or the release never became
// healthy.
const (
	gateApply  = "apply"
	gateWiring = "wiring"
	gateHealth = "health"
)

// StepPlan is one step and the apps its image move touches.
type StepPlan struct {
	Step
	// Moving is the enabled apps whose image (repository and digest) differs
	// between the two releases. They are the ones scaled down and
	// snapshotted; a step that moves no image takes no snapshot.
	Moving []string
	// Held is the enabled apps the operator's values pin, so they run the
	// same image on both sides of the step and do not move. A held app is
	// not stopped, not checked and not copied; the plan names it so the pin
	// does not read as a release that forgot the app.
	Held []string
}

// Result is what one run decided and did.
type Result struct {
	// Name is the chart's name, for box's sentences ("s5n 0.4.0 -> 0.5.0").
	Name string
	// Namespace is the seedbox's, from the grant.
	Namespace string
	// Installed is the chart version Helm reports deployed, Target the
	// release this run moves to.
	Installed string
	Target    string
	// Current says there is nothing to do: the target is the installed
	// release (or the box is on a newer one than anything published).
	Current bool
	// Steps is the ordered path to the target.
	Steps []StepPlan
	// Skips are releases box passed over because their min-box is above it.
	Skips []release.VersionSkip
	// Skipped are release versions selection passed over because a previous
	// run failed on them.
	Skipped []string
	// Taken is the steps this run completed, in order.
	Taken []Step

	// state is the record this run read and keeps current, carried so a step
	// does not have to re-read a file it may have written itself.
	state State
	// pins is the pin each held app is held at, for the plan's held rows: the
	// values' own label, carried from the plan to the report, which has no
	// values of its own.
	pins map[string]string
}

// Updater is `box update`'s whole decision.
type Updater struct {
	// Charts is the chart repository. A published release only: box never
	// reads charts/ from its own tree.
	Charts ChartSource
	// Open returns the release handle for the grant's namespace.
	Open func(namespace string) (Release, error)
	// OpenCluster returns the release's StatefulSets. The namespace comes from
	// the grant, so it can only be opened after the files are read; `--check`
	// never opens it, because planning reads no workload.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots returns the copy of the moving apps' config for the
	// grant's namespace. It is opened only when a step moves an image: a
	// release change that moves no image has nothing to copy, and a box
	// without a snapshotter still takes those. A step that does move an image
	// on such a box refuses before it stops anything.
	OpenSnapshots func(namespace string) (Snapshotter, error)
	// Apply runs one step.
	Apply ApplyFunc
	// Confirm asks the one question, before the first write. Every step's
	// own apply runs pre-confirmed: this is the update the operator said yes
	// to, and a step that stopped halfway through a multi-step path to ask
	// again would be asking after the first step had already moved the box.
	Confirm release.Confirmer
	// State is update.json.
	State StateStore
	// BoxVersion is this binary's version, which every release's min-box is
	// compared with. "dev" skips the check.
	BoxVersion string
	// Say is every line update writes.
	Say say.Reporter

	// HealthWait bounds the wait for the release and its StatefulSets to be
	// ready after a step. Zero means five minutes.
	HealthWait time.Duration
	// SettleWait bounds the wait for scaled-down pods to go. Zero means five
	// minutes.
	SettleWait time.Duration
	// CleanupWait bounds what a failed step does after the failure: starting
	// the apps this run stopped again, and reading where Helm left the
	// release. It runs after the context that would have cancelled it is
	// gone. Zero means five minutes.
	CleanupWait time.Duration
	// Poll is how often the waits re-read state. Zero means two seconds.
	Poll time.Duration
	// Now is time.Now, so a test can pin the record's timestamps.
	Now func() time.Time
}

func (u *Updater) report() say.Reporter { return say.Or(u.Say, nil) }

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) healthWait() time.Duration {
	if u.HealthWait > 0 {
		return u.HealthWait
	}
	return 5 * time.Minute
}

func (u *Updater) settleWait() time.Duration {
	if u.SettleWait > 0 {
		return u.SettleWait
	}
	return 5 * time.Minute
}

func (u *Updater) cleanupWait() time.Duration {
	if u.CleanupWait > 0 {
		return u.CleanupWait
	}
	return 5 * time.Minute
}

func (u *Updater) poll() time.Duration {
	if u.Poll > 0 {
		return u.Poll
	}
	return 2 * time.Second
}

// Run plans the update, reports it, asks once, and takes each step in order.
func (u *Updater) Run(ctx context.Context, opts Options) (Result, error) {
	res, err := u.plan(ctx, opts)
	if err != nil {
		return res, err
	}
	u.reportPlan(res, opts.Check)
	if opts.Check || res.Current {
		return res, nil
	}
	// A box that cannot copy the moving apps gives the operator no way to put
	// a migrated database back, so it says so before asking anything.
	if aStepMoves(res.Steps) && u.OpenSnapshots == nil {
		return res, fmt.Errorf("%s needs to snapshot %s to go to %s and this box cannot: update with a box that has snapshot support",
			res.Name, movingText(res.Steps), res.Target)
	}
	if u.Confirm == nil {
		return res, errors.New("update needs a confirm: it writes to a cluster")
	}
	if u.Apply == nil {
		return res, errors.New("update needs an apply: a step installs its release")
	}
	if u.OpenCluster == nil {
		return res, errors.New("update needs a workload client: it stops what it snapshots")
	}
	ok, err := u.Confirm.Ask(fmt.Sprintf("update %s from %s to %s?", res.Name, res.Installed, res.Target))
	if err != nil {
		return res, err
	}
	if !ok {
		u.report().Line("oh heck, nvm")
		return res, release.ErrCancelled
	}
	rel, err := u.Open(res.Namespace)
	if err != nil {
		return res, err
	}
	cluster, err := u.OpenCluster(res.Namespace)
	if err != nil {
		return res, err
	}
	var snaps Snapshotter
	if aStepMoves(res.Steps) {
		snaps, err = u.OpenSnapshots(res.Namespace)
		if err != nil {
			return res, err
		}
		if snaps == nil {
			return res, fmt.Errorf("%s needs to snapshot %s to go to %s and this box cannot: update with a box that has snapshot support",
				res.Name, movingText(res.Steps), res.Target)
		}
	}
	for _, step := range res.Steps {
		if err := u.take(ctx, rel, cluster, snaps, &res, step, opts.Apply); err != nil {
			return res, err
		}
		res.Taken = append(res.Taken, step.Step)
	}
	return res, nil
}

// plan is every read an update makes: the files, the deployed release, the
// published releases, the target and the path. It writes nothing but the
// check time, so `--check` and a declined update leave the box as they found
// it.
func (u *Updater) plan(ctx context.Context, opts Options) (Result, error) {
	res := Result{}
	if u.Charts == nil {
		return res, errors.New("no chart source: box reads its chart from the registry")
	}
	if u.State == nil {
		return res, errors.New("no state store: update records what it last checked and failed")
	}
	// The files first: a grant key in values, a value the chart refuses or a
	// missing namespace is the operator's problem and is not worth a
	// registry round trip.
	sb, err := seedbox.Load(opts.Apply.GrantPath, opts.Apply.ValuesPath)
	if err != nil {
		return res, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return res, err
	}
	res.Namespace = namespace
	state, err := u.State.Load()
	if err != nil {
		return res, err
	}
	res.state = state

	rel, err := u.Open(namespace)
	if err != nil {
		return res, err
	}
	installed, err := rel.State(ctx)
	if err != nil {
		return res, err
	}
	if !installed.Exists {
		return res, fmt.Errorf("no %s release is deployed in %s: `box init` installs one, and `box apply` re-applies the one that is there",
			release.ReleaseName, namespace)
	}
	res.Installed = installed.ChartVersion

	repo := opts.Apply.ChartRepo
	releases, err := u.Charts.Releases(ctx, release.ChartRef{Repo: repo})
	if err != nil {
		return res, err
	}
	// The installed chart is loaded even when there is nothing to do: its name
	// is what box calls the product, and its variants are the old side of
	// every step's image comparison.
	fromChart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: installed.ChartVersion})
	if err != nil {
		return res, fmt.Errorf("the box is on chart %s, which cannot be read: %w", installed.ChartVersion, err)
	}
	res.Name = fromChart.Name
	if res.Name == "" {
		res.Name = release.ReleaseName
	}

	target, skipped, skips, err := u.target(ctx, opts, releases, state)
	if err != nil {
		return res, err
	}
	res.Target = target
	res.Skipped = skipped
	res.Skips = skips

	// A target the operator named can be a release the listing leaves out --
	// a prerelease, which is only ever taken when it is named. Its floors are
	// the chart's own annotations, so it is planned with them.
	if !published(releases, target) {
		chart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: target})
		if err != nil {
			return res, err
		}
		releases = append(releases, release.Published{
			Version:     target,
			MinBox:      strings.TrimSpace(chart.Annotations[release.MinBoxAnnotation]),
			UpgradeFrom: strings.TrimSpace(chart.Annotations[release.UpgradeFromAnnotation]),
		})
	}

	cmp, err := compareVersions(installed.ChartVersion, target)
	if err != nil {
		return res, err
	}
	if cmp > 0 && strings.TrimSpace(opts.Apply.ChartVersion) != "" {
		// Selecting the newest release can land on one older than what is
		// installed (a hand-installed future release), and that is "current";
		// naming one explicitly is a request, and this command never moves a
		// release backwards.
		return res, fmt.Errorf("%s is on %s and --chart-version names %s: box update never moves a release backwards",
			release.ReleaseName, installed.ChartVersion, target)
	}
	if cmp >= 0 {
		res.Current = true
		res.state.LastCheck = u.now()
		if err := u.State.Save(res.state); err != nil {
			return res, err
		}
		return res, nil
	}

	steps, err := planSteps(installed.ChartVersion, target, u.BoxVersion, releases)
	if err != nil {
		return res, err
	}
	// The effective values are what apply installs: chart defaults, the
	// user's keys, then the grant. The target's schema is what this run is
	// checked against, because that is the release every step ends on.
	toChart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: target})
	if err != nil {
		return res, err
	}
	values, err := sb.Effective(seedbox.Chart{Values: toChart.Values, Schema: toChart.Schema})
	if err != nil {
		return res, err
	}
	res.Steps, res.pins, err = u.stepsPlan(ctx, repo, fromChart, steps, values)
	if err != nil {
		return res, err
	}

	res.state.LastCheck = u.now()
	if err := u.State.Save(res.state); err != nil {
		return res, err
	}
	return res, nil
}

// stepsPlan loads each step's release and compares its images with the release
// before it, which is what decides what that step moves and what the operator's
// pins hold. It also carries each held app's pin label, because the report has
// no values of its own.
func (u *Updater) stepsPlan(ctx context.Context, repo string, from release.Chart, steps []Step, values map[string]any) ([]StepPlan, map[string]string, error) {
	var plans []StepPlan
	pins := make(map[string]string)
	for _, s := range steps {
		to, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: s.To})
		if err != nil {
			return nil, nil, err
		}
		moving, held, err := movingApps(from, to, values)
		if err != nil {
			return nil, nil, err
		}
		for _, app := range held {
			pins[app] = pinLabel(values, app)
		}
		plans = append(plans, StepPlan{Step: s, Moving: moving, Held: held})
		from = to
	}
	return plans, pins, nil
}

// target picks the release this run moves to: the version the operator named,
// or the newest published release this box can use. Releases a previous run
// failed on are passed over with their versions recorded, unless the operator
// named one, which is the override.
func (u *Updater) target(ctx context.Context, opts Options, releases []release.Published, state State) (target string, skipped []string, skips []release.VersionSkip, err error) {
	repo := opts.Apply.ChartRepo
	if pin := strings.TrimSpace(opts.Apply.ChartVersion); pin != "" {
		chart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: pin})
		if err != nil {
			return "", nil, nil, err
		}
		return chart.Version, nil, nil, nil
	}

	byVersion := make(map[string]string, len(releases))
	tags := make([]string, 0, len(releases))
	for _, r := range releases {
		byVersion[r.Version] = r.MinBox
		tags = append(tags, r.Version)
	}
	failed := make(map[string]bool, len(state.Failed))
	for _, v := range state.Failed {
		failed[v] = true
	}
	exclude := func(version string) bool {
		if failed[version] {
			return true
		}
		for bad := range failed {
			if versionsEqual(version, bad) {
				return true
			}
		}
		return false
	}
	chosen, skips, err := release.SelectVersion(tags, u.BoxVersion, func(version string) (string, error) {
		return byVersion[version], nil
	}, exclude)
	if err != nil {
		return "", nil, nil, err
	}
	for _, bad := range state.Failed {
		if versionsEqual(chosen, bad) {
			continue
		}
		// Only a failure ahead of the chosen release is worth naming: one
		// the box has already moved past is history, not a skip.
		if cmp, err := compareVersions(bad, chosen); err == nil && cmp > 0 {
			skipped = append(skipped, bad)
		}
	}
	return chosen, skipped, skips, nil
}

// reportPlan is the first line and the plan, in box's words.
func (u *Updater) reportPlan(res Result, check bool) {
	r := u.report()
	if res.Current {
		r.Line(fmt.Sprintf("%s %s in %s is current", res.Name, res.Installed, res.Namespace))
	} else {
		steps := plural(len(res.Steps), "step", "steps")
		r.Line(fmt.Sprintf("%s %s -> %s in %s: %s", res.Name, res.Installed, res.Target, res.Namespace, steps))
		rows := make([]string, 0, len(res.Steps))
		var heldRows []string
		for _, step := range res.Steps {
			row := step.From + " -> " + step.To
			if len(step.Moving) == 0 {
				row += " (no image moves)"
			} else {
				row += " (apps: " + strings.Join(step.Moving, ", ") + ")"
			}
			rows = append(rows, row)
			// A pin the operator set is why an app the chart moved stays
			// where it is. Saying so is the difference between a plan that
			// respects their values and one that looks like it forgot.
			for _, app := range step.Held {
				heldRows = append(heldRows, fmt.Sprintf("%s: held at %s by your values", app, res.pins[app]))
			}
		}
		r.List("steps", rows)
		if len(heldRows) > 0 {
			r.List("held", heldRows)
		}
	}
	// A release a box cannot take is said out loud even when there is nothing
	// to do: "s5n 0.6.0 is out and needs a newer box" is the whole reason a
	// user reads the output of a current box.
	for _, skip := range res.Skips {
		r.Warn(fmt.Sprintf("%s %s is out and needs a newer box: install box %s or newer", res.Name, skip.Version, skip.MinBox))
	}
	for _, bad := range res.Skipped {
		r.Warn(fmt.Sprintf("%s %s failed to update on this box before; box is not taking it again until a newer release is out", res.Name, bad))
	}
	if check {
		r.Muted("--check: nothing written but the check time")
	}
}

// take is one step: stop and snapshot what moves, apply, gate, and then either
// record the step or record why it stopped. A failure before the apply puts
// back the app replicas this run stopped; a failure at a gate leaves the
// release, and the apps, where the step put them.
func (u *Updater) take(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, base release.Options) error {
	r := u.report()
	handle := r.Start(say.Op{Label: fmt.Sprintf("updating %s to %s", res.Name, step.To)})

	// The revision this step starts from. It is read before the first write,
	// not after the snapshot, so a failure in the stopping or the snapshot
	// still knows what the box is on -- and knows nothing has moved yet.
	before, err := rel.State(ctx)
	if err != nil {
		handle.Failed(err)
		return err
	}
	if !before.Exists {
		err := fmt.Errorf("update %s to %s: the release is no longer deployed", res.Name, step.To)
		handle.Failed(err)
		return err
	}

	if len(step.Moving) > 0 {
		// Every failure down to the apply is this run's own doing -- it
		// stopped apps nothing has asked to move yet -- so it starts them
		// again and returns the step's error. None of it is a failed
		// release: the release was never tried.
		if err := u.scale(ctx, cluster, step.Moving, 0); err != nil {
			u.scaleUp(ctx, cluster, step.Moving)
			err = fmt.Errorf("update %s to %s: stopping %s: %w", res.Name, step.To, strings.Join(step.Moving, ", "), err)
			handle.Failed(err)
			return err
		}
		if err := u.waitGone(ctx, cluster, step.Moving); err != nil {
			u.scaleUp(ctx, cluster, step.Moving)
			err = fmt.Errorf("update %s to %s: waiting for %s to stop: %w", res.Name, step.To, strings.Join(step.Moving, ", "), err)
			handle.Failed(err)
			return err
		}
		if err := snaps.Take(ctx, step.Step, step.Moving); err != nil {
			u.scaleUp(ctx, cluster, step.Moving)
			err = fmt.Errorf("update %s to %s: snapshotting %s: %w", res.Name, step.To, strings.Join(step.Moving, ", "), err)
			handle.Failed(err)
			return err
		}
	}

	applyOpts := base
	// The step's own release, with the operator's files untouched. Chart is
	// left nil so this apply pulls by version exactly as a bare apply would.
	applyOpts.Chart = nil
	applyOpts.ChartVersion = step.To
	applied, err := u.Apply(ctx, applyOpts)
	gate := ""
	if err != nil {
		gate = gateApply
		if applied.Applied {
			// Helm moved the release; what failed after that is the wiring
			// receipt apply waits for.
			gate = gateWiring
		}
	}
	if err == nil {
		if herr := u.waitHealthy(ctx, rel, cluster); herr != nil {
			gate, err = gateHealth, herr
		}
	}
	if err != nil {
		handle.Failed(err)
		return u.fail(ctx, rel, cluster, res, step, before.Revision, gate, err)
	}

	if len(step.Moving) > 0 {
		if perr := snaps.Prune(ctx, step.Step, step.Moving); perr != nil {
			// A snapshot that cannot be pruned is not a failed step: the
			// release and the box are healthy, and keeping one extra copy
			// costs disk, not correctness.
			r.Warn(fmt.Sprintf("keeping the snapshot for %s: %v", step.To, perr))
		}
	}
	handle.Done(fmt.Sprintf("%s is on %s", res.Name, step.To))
	if err := u.recordOK(res, step, before.Revision); err != nil {
		return fmt.Errorf("%s is on %s, but update's record could not be written: %w", res.Name, step.To, err)
	}
	return nil
}

// fail ends a step that did not pass. It says where the release is now, in
// Helm's own terms, and records the release as failed so the next run and the
// timer pass it over until something newer exists.
//
// It starts the stopped apps again only when Helm never reported the upgrade
// done: an apply that refused left the old release running, with the apps this
// run stopped still down. After a wiring or health gate the new release is
// what is running, so the apps stay as it left them, and the step's snapshot
// stays too -- that copy is the operator's way back.
func (u *Updater) fail(ctx context.Context, rel Release, cluster Cluster, res *Result, step StepPlan, revision int, gate string, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cleanupWait())
	defer cancel()

	u.report().Warn(fmt.Sprintf("update %s to %s did not pass the %s gate: %v", res.Name, step.To, gate, cause))

	// Nothing else puts these replicas back when Helm never moved, and a
	// second failure here belongs in the error rather than in a shrug.
	var startErr error
	if gate == gateApply && len(step.Moving) > 0 {
		if err := u.scale(cctx, cluster, step.Moving, 1); err != nil {
			startErr = err
		}
	}

	// What the release is on is Helm's answer, read now, not box's
	// assumption about what the step managed to do.
	state, stateErr := rel.State(cctx)
	msg := fmt.Sprintf("update %s to %s: ", res.Name, step.To)
	if gate == gateApply {
		msg += fmt.Sprintf("the apply failed (%v)", cause)
	} else {
		msg += fmt.Sprintf("the %s gate failed (%v)", gate, cause)
	}
	switch {
	case stateErr != nil:
		msg += fmt.Sprintf("; the release's state could not be read (%v)", stateErr)
	case !state.Exists:
		msg += fmt.Sprintf("; the release's state could not be read (no %s release is deployed)", res.Name)
	default:
		msg += fmt.Sprintf("; %s is on %s (revision %d, %s)", res.Name, state.ChartVersion, state.Revision, state.Status)
	}
	if gate != gateApply {
		msg += " and was not rolled back"
	}
	if startErr != nil {
		msg += fmt.Sprintf("; starting %s again failed: %v", strings.Join(step.Moving, ", "), startErr)
	}
	if recordErr := u.recordFailed(res, step, revision, gate, msg); recordErr != nil {
		return fmt.Errorf("%s; the record could not be written: %w", msg, recordErr)
	}
	return errors.New(msg)
}

// scale sets every moving app's replicas.
func (u *Updater) scale(ctx context.Context, cluster Cluster, apps []string, replicas int32) error {
	for _, app := range apps {
		if err := cluster.Scale(ctx, app, replicas); err != nil {
			return fmt.Errorf("%s: %w", app, err)
		}
	}
	return nil
}

// scaleUp starts apps again after a failure that never reached Helm. It is
// best effort on purpose: the step's own error is the one box reports, and a
// second failure here would only hide it.
func (u *Updater) scaleUp(ctx context.Context, cluster Cluster, apps []string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cleanupWait())
	defer cancel()
	_ = u.scale(cctx, cluster, apps, 1)
}

// waitGone waits for the stopping apps' pods to go, so the snapshot is taken
// with no writer.
func (u *Updater) waitGone(ctx context.Context, cluster Cluster, apps []string) error {
	waited, err := wait.Until(ctx, u.settleWait(), u.poll(), func(ctx context.Context) (bool, error) {
		for _, app := range apps {
			gone, err := cluster.Gone(ctx, app)
			if err != nil {
				return false, err
			}
			if !gone {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !waited.Done {
		return fmt.Errorf("pods still running after %s", u.settleWait())
	}
	return nil
}

// waitHealthy is the health gate: Helm's status says deployed, and every
// StatefulSet of the release is ready on its current generation.
func (u *Updater) waitHealthy(ctx context.Context, rel Release, cluster Cluster) error {
	waited, err := wait.Until(ctx, u.healthWait(), u.poll(), func(ctx context.Context) (bool, error) {
		state, err := rel.State(ctx)
		if err != nil {
			return false, err
		}
		if !state.Exists || !state.Deployed() {
			return false, nil
		}
		return cluster.Healthy(ctx)
	})
	if err != nil {
		return err
	}
	if !waited.Done {
		return fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait())
	}
	return nil
}

// published reports whether the listing names a version.
func published(releases []release.Published, version string) bool {
	for _, r := range releases {
		if versionsEqual(r.Version, version) {
			return true
		}
	}
	return false
}

// recordOK records a passing step: the box is on step.To, and every release it
// failed on at or below that version is behind it now.
func (u *Updater) recordOK(res *Result, step StepPlan, revision int) error {
	res.state.LastStep = &StepRecord{
		From:     step.From,
		To:       step.To,
		At:       u.now(),
		Outcome:  OutcomeOK,
		Revision: revision,
		Moved:    step.Moving,
	}
	res.state.Failed = dropAtOrBelow(res.state.Failed, step.To)
	return u.State.Save(res.state)
}

// recordFailed records a failed step and the release it could not take, so the
// next run (and the timer) does not try it again until something newer exists.
func (u *Updater) recordFailed(res *Result, step StepPlan, revision int, gate, detail string) error {
	res.state.LastStep = &StepRecord{
		From:     step.From,
		To:       step.To,
		At:       u.now(),
		Outcome:  OutcomeFailed,
		Error:    detail,
		Revision: revision,
		Moved:    step.Moving,
		Gate:     gate,
	}
	if !containsVersion(res.state.Failed, step.To) {
		res.state.Failed = append(res.state.Failed, step.To)
	}
	return u.State.Save(res.state)
}

// dropAtOrBelow removes every failed version at or below keep: a passing step
// has carried the box past them.
func dropAtOrBelow(failed []string, keep string) []string {
	var out []string
	for _, v := range failed {
		cmp, err := compareVersions(v, keep)
		if err != nil || cmp > 0 {
			out = append(out, v)
		}
	}
	return out
}

func containsVersion(list []string, version string) bool {
	for _, v := range list {
		if versionsEqual(v, version) {
			return true
		}
	}
	return false
}

// versionsEqual compares two version strings the way semver orders them; a
// string that is not a version is only equal to itself.
func versionsEqual(a, b string) bool {
	av, aerr := semver.NewVersion(a)
	bv, berr := semver.NewVersion(b)
	if aerr != nil || berr != nil {
		return a == b
	}
	return av.Equal(bv)
}

// compareVersions is -1 when a is older than b, 0 when they are the same, 1
// when a is newer.
func compareVersions(a, b string) (int, error) {
	av, err := semver.NewVersion(a)
	if err != nil {
		return 0, fmt.Errorf("version %q is not a version: %v", a, err)
	}
	bv, err := semver.NewVersion(b)
	if err != nil {
		return 0, fmt.Errorf("version %q is not a version: %v", b, err)
	}
	return av.Compare(bv), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// aStepMoves reports whether any step copies an app's config, which is what
// decides whether update needs a snapshotter at all.
func aStepMoves(steps []StepPlan) bool {
	for _, step := range steps {
		if len(step.Moving) > 0 {
			return true
		}
	}
	return false
}

// movingText names every app the path moves, once, in step order. It is used
// in the one sentence box says when it cannot snapshot.
func movingText(steps []StepPlan) string {
	var apps []string
	seen := make(map[string]bool)
	for _, step := range steps {
		for _, app := range step.Moving {
			if seen[app] {
				continue
			}
			seen[app] = true
			apps = append(apps, app)
		}
	}
	return strings.Join(apps, ", ")
}
