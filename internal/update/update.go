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
// box puts one thing back on its own. A gate that fails stops the update and
// says where the release is, except for the failure the new image is the cause
// of: a moved app whose container keeps exiting is scaled to zero, restored from
// this step's copy, and held on the image it ran, and the step re-applies the
// same release so the chart renders that image. The rest of the release stays
// where the step put it. The hold lives beside the snapshots and lapses when a
// release pins another image, so the app moves again with the release that
// fixes it. An app the catalog gives nothing to copy is never put back: box has
// no state to restore, so it stops instead.
//
// An app that is running but not yet listening is waited for, because an Arr
// opens its port only after its migration has finished. A failure before the
// release ever moved only starts the apps this run stopped.
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
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/wait"
)

// ChartSource is where update reads the repository: one release by version
// (apply's ChartSource), and every published release from a floor up, with the
// annotations the update plan needs. release.OCIChart implements both.
type ChartSource interface {
	release.ChartSource
	// Releases lists the published releases at or above floor, oldest first.
	// floor is the installed release: an update only ever steps forward, so
	// box never reads a version below it. An empty listing is a repository
	// with nothing ahead of the box, which is current, not an error.
	Releases(ctx context.Context, ref release.ChartRef, floor string) ([]release.Published, error)
}

// Release is the deployed release as update reads it: what Helm says is
// running. release.Helm implements it. This package never moves the release
// back -- a failed gate stops the update where it is and says where the box
// was left -- so the interface holds the read alone.
type Release interface {
	State(ctx context.Context) (release.ReleaseState, error)
}

// Cluster is the release's StatefulSets and pods, as an update writes and
// reads them. It is one interface rather than three clients because the three
// questions are one conversation: scale an app down, wait for its pods to go,
// and later ask whether everything is ready and nothing it moved is
// crashlooping.
type Cluster interface {
	// Scale sets the replicas of every StatefulSet of one app -- both Arr
	// instances included -- in the release's namespace. An app with no
	// StatefulSet there (the values just enabled it) is not an error: there
	// is nothing of it to stop.
	Scale(ctx context.Context, app string, replicas int32) error
	// Gone reports whether every pod of that app is gone.
	Gone(ctx context.Context, app string) (bool, error)
	// Health reads the release once: whether every StatefulSet of it is
	// ready and has rolled out its current generation, and which of the apps
	// this step moved keep exiting. Only the moved apps are asked about:
	// an app that kept its image cannot be this step's fault.
	Health(ctx context.Context, moving []string) (Health, error)
	// Running reads the image each app runs: the reference its StatefulSets
	// ask for and the digest its pods report. It is the other half of "what
	// moves": Helm's stored values are what was asked for, and an apply
	// compares what runs with what it is about to render.
	Running(ctx context.Context) (map[string]Running, error)
}

// Snapshotter copies the apps a step moves out of the way, so box can put one
// of them back when its new image crashloops, and the operator can always go
// back by hand with the copy. internal/update's ConfigSnapshots implements it
// over internal/snapshot; this package ships the interface and a test fake, and
// cmd/box wires the implementation in.
type Snapshotter interface {
	// Check reports whether a copy of these apps can be taken, without
	// stopping an app or writing a file. A refusal ends the update with
	// nothing stopped: a box whose config directory is not where the values
	// say has no way to put the app's database back, and that is not a step
	// to start.
	Check(ctx context.Context, step Step, apps []string) error
	// Take copies the named apps' config for this step, with the apps
	// stopped, and says where it put them. It must fail rather than leave a
	// partial snapshot. The images are what the manifest records for a later
	// rollback; a step that cannot name both is refused before anything is
	// copied.
	Take(ctx context.Context, step Step, apps []string, images map[string]ImageChange) (Taken, error)
	// Prune keeps the newest two snapshots per app.
	Prune(ctx context.Context, step Step, apps []string) error
	// Restore puts the named apps back to the copy this step took, before the
	// release moved. It is the one thing box puts back on its own, and only
	// for apps whose new image kept exiting.
	Restore(ctx context.Context, step Step, apps []string) error
}

// Taken is one step's copy: where it is and what it named, which is what the
// rollback holds record and what box says when an app is put back.
type Taken struct {
	// Step is the step directory's name. A hold records it, so a later
	// rollback finds this step's copies of every app it moved.
	Step string
	// At is when the copy was taken, in UTC. box tells the operator how old
	// the state a rollback restored is.
	At time.Time
	// Dirs is each app's copy inside the step directory, for box's sentence
	// when a rollback fails: the operator needs to know where to look.
	Dirs map[string]string
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
// wiring receipt says the release did not work, the release never became
// healthy, or the step-back box tried after a moved app kept exiting did not
// finish.
const (
	gateApply    = "apply"
	gateWiring   = "wiring"
	gateHealth   = "health"
	gateRollback = "rollback"
)

// GateResult is how one step's health gate ended. It is a value rather than an
// error because the three answers are not one failure: a release that is ready
// passed, a moved app that keeps exiting is the image's fault and is named as
// such, and a wait that ran out is an app that is running but not ready, which
// box leaves where it is. A read that fails is an error on top of it.
type GateResult struct {
	// Exiting is the moved apps whose containers keep exiting, sorted by
	// app. The gate stops on the first reading that finds one, without
	// waiting for the rest of the release: the answer would only be the same
	// later, with more restarts behind it.
	Exiting []Exiting
	// TimedOut is the wait running out before the release was deployed and
	// healthy, with no moved app exiting: the apps are running, just not
	// ready.
	TimedOut bool
}

// StepPlan is one step and the apps its image move touches.
type StepPlan struct {
	Step
	// Moving is the enabled apps whose image (repository and digest) differs
	// between the two releases. They are the ones the release moves; the
	// subset the catalog says has state to copy is what a step stops and
	// snapshots.
	Moving []string
	// Images is each moving app's image on either side of the step, which the
	// snapshot records and a rollback holds. A moving app whose selected
	// variant is missing on one side has a zero reference there, and the copy
	// refuses rather than record an image box never saw.
	Images map[string]ImageChange
	// Held is the enabled apps the operator's values pin, so they run the
	// same image on both sides of the step and do not move. A held app is
	// not stopped, not checked and not copied; the plan names it so the pin
	// does not read as a release that forgot the app.
	Held []string
	// HeldBack is the enabled apps a rollback hold keeps on the image they
	// ran: the release still pins the image the rollback avoided, so the app
	// does not move until a release pins something else. Like a values pin,
	// it is neither stopped nor copied; the plan names it and the image it
	// waits on.
	HeldBack []string
}

// Copied is the moving apps this step stops and snapshots: those whose catalog
// entry names files an image upgrade can rewrite. A moving app without them is
// applied without a copy and is never rolled back, because box would have no
// state to put back.
func (p StepPlan) Copied() []string {
	var out []string
	for _, app := range p.Moving {
		if p.Images[app].Snapshot {
			out = append(out, app)
		}
	}
	return out
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
	// values are the effective values this run planned with, which the
	// snapshotter needs for the instance names it copies.
	values map[string]any
	// pins is the pin each held app is held at, for the plan's held rows: the
	// values' own label, carried from the plan to the report, which has no
	// values of its own.
	pins map[string]string
	// holds are the rollback holds the plan honoured, by app: the report
	// names each held app's old and avoided image.
	holds map[string]snapshot.Hold
	// configRoot is the grant's config path, where the holds and their
	// snapshots live. A step that rolls an app back writes its hold there.
	configRoot string
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
	// OpenSnapshots returns the copy of the copying apps' config for the
	// grant's namespace, with the effective values this run planned with (the
	// instance names come from them). It is opened only when a step copies an
	// image's state: a release change that moves nothing has nothing to copy,
	// and a box without a snapshotter still takes those. A step that does copy
	// on such a box refuses before it stops anything.
	OpenSnapshots func(namespace string, values map[string]any) (Snapshotter, error)
	// Apply runs one step.
	Apply ApplyFunc
	// Confirm asks the one question, before the first write. Every step's
	// own apply runs pre-confirmed: this is the update the operator said yes
	// to, and a step that stopped halfway through a multi-step path to ask
	// again would be asking after the first step had already moved the box.
	Confirm release.Confirmer
	// State is update.json.
	State StateStore
	// Notify is told what each step did, and told when one stops. Nil posts
	// nothing, which is a box with no notice address.
	Notify func(context.Context, Notice)
	// Holds is the record of apps a rollback holds at an older image. The
	// plan reads it and a step that rolls an app back writes it; nil is a box
	// that keeps no holds, which is what a test that never rolls back wants.
	Holds HoldStore
	// BoxVersion is this binary's version, which every release's min-box is
	// compared with. "dev" skips the check.
	BoxVersion string
	// Say is every line update writes.
	Say say.Reporter

	// HealthWait bounds the wait for the release and its StatefulSets to be
	// ready after a step. Zero means thirty-five minutes: every app gets
	// thirty minutes to open its port before the kubelet restarts it, so the
	// gate has to outlast that budget.
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

// DefaultHealthWait is the health gate's budget: every app gets thirty minutes
// to open its port (the chart's startupProbe, 360 failures of 5s), so the gate
// has to outlast that budget or it would stop an update over a migration the
// kubelet would have let finish. The margin covers the poll and the last probe.
//
// A step's Helm wait uses the same budget (cmd/box sets it): Helm gives up on a
// pod in CrashLoopBackOff at once, and a migration longer than Helm's own
// fifteen-minute default must not fail the apply before this gate can see it.
const DefaultHealthWait = 35 * time.Minute

func (u *Updater) healthWait() time.Duration { return orDuration(u.HealthWait, DefaultHealthWait) }

func (u *Updater) settleWait() time.Duration { return orDuration(u.SettleWait, 5*time.Minute) }

func (u *Updater) cleanupWait() time.Duration { return orDuration(u.CleanupWait, 5*time.Minute) }

func (u *Updater) poll() time.Duration { return orDuration(u.Poll, 2*time.Second) }

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
	// A box that cannot copy what a step moves gives the operator no way to
	// put a migrated database back, so it says so before asking anything.
	if aStepCopies(res.Steps) && u.OpenSnapshots == nil {
		return res, fmt.Errorf("%s needs to snapshot %s to go to %s and this box cannot: update with a box that has snapshot support",
			res.Name, copyingText(res.Steps), res.Target)
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
	if aStepCopies(res.Steps) {
		snaps, err = u.OpenSnapshots(res.Namespace, res.values)
		if err != nil {
			return res, err
		}
		if snaps == nil {
			return res, fmt.Errorf("%s needs to snapshot %s to go to %s and this box cannot: update with a box that has snapshot support",
				res.Name, copyingText(res.Steps), res.Target)
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
	// The config root is where an app's config lives and therefore where a
	// step copies it and the holds beside those copies live. It is the
	// grant's path, so it is read here, before any registry round trip.
	root, err := ConfigRoot(sb)
	if err != nil {
		return res, err
	}
	res.configRoot = root
	holds, err := loadHolds(u.Holds, root)
	if err != nil {
		return res, err
	}
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
	// The listing starts at the installed release: an update never moves a
	// release backwards, and the repository's older charts are history -- some
	// of it a chart today's box cannot read at all.
	releases, err := u.Charts.Releases(ctx, release.ChartRef{Repo: repo}, installed.ChartVersion)
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
	// Nothing at or above the installed release is published: the box is on the
	// newest one, or ahead of every published one. That is current. A target the
	// operator named is still read below, because naming one is a request.
	if len(releases) == 0 && strings.TrimSpace(opts.Apply.ChartVersion) == "" {
		res.Target = installed.ChartVersion
		res.Current = true
		res.state.LastCheck = u.now()
		if err := u.State.Save(res.state); err != nil {
			return res, err
		}
		return res, nil
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
	res.values = values
	res.Steps, res.pins, res.holds, err = u.stepsPlan(ctx, repo, fromChart, steps, values, holds)
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
// no values of its own, and the target release's catalog, which says what a
// step can copy: an app the catalog gives no globs is moved without a snapshot.
func (u *Updater) stepsPlan(ctx context.Context, repo string, from release.Chart, steps []Step, values map[string]any, holds []snapshot.Hold) ([]StepPlan, map[string]string, map[string]snapshot.Hold, error) {
	var plans []StepPlan
	pins := make(map[string]string)
	heldByRollback := make(map[string]snapshot.Hold)
	for _, s := range steps {
		to, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: s.To})
		if err != nil {
			return nil, nil, nil, err
		}
		moving, held, heldBack, images, err := movingApps(from, to, values, holds)
		if err != nil {
			return nil, nil, nil, err
		}
		globs, err := ChartGlobs(to)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, app := range moving {
			change := images[app]
			change.Snapshot = len(globs[app]) > 0
			images[app] = change
		}
		for _, app := range held {
			pins[app] = pinLabel(values, app)
		}
		for _, app := range heldBack {
			if h, ok := holdFor(holds, app); ok {
				heldByRollback[app] = h
			}
		}
		plans = append(plans, StepPlan{Step: s, Moving: moving, Images: images, Held: held, HeldBack: heldBack})
		from = to
	}
	return plans, pins, heldByRollback, nil
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

// reportPlan is the one line that says what this run is, and then only the rows
// a person acts on: a pin the operator set, a release box is passing over, and
// what --check did not write. The step list is a heading's worth of information
// while there is one step, and one quiet row per step while there are several.
func (u *Updater) reportPlan(res Result, check bool) {
	r := u.report()
	if res.Current {
		r.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{fmt.Sprintf("%s %s is current", res.Name, res.Installed)}}})
		r.Detail(fmt.Sprintf("%s is in %s and nothing newer is published for this box", res.Installed, res.Namespace))
	} else {
		r.Line(planHeading(res))
		if len(res.Steps) > 1 {
			rows := make([]say.Row, 0, len(res.Steps))
			for _, step := range res.Steps {
				facts := []string{"no image moves"}
				if len(step.Moving) > 0 {
					facts = []string{movingNames(step) + " move"}
				}
				rows = append(rows, say.Row{Mark: say.MarkNote, Name: step.From + " → " + step.To, Facts: facts})
			}
			r.Rows(rows)
		}
		r.Line("")
		// A pin the operator set is why an app the chart moved stays where it
		// is. Saying so is the difference between a plan that respects their
		// values and one that looks like it forgot.
		var held []say.Row
		for _, step := range res.Steps {
			// An app the catalog gives nothing to copy is never rolled back:
			// box would have no state to put back. The plan says so before the
			// step moves, because it is the one thing that changes what a
			// failure costs.
			for _, app := range step.Moving {
				if step.Images[app].Snapshot {
					continue
				}
				held = append(held, say.Row{Mark: say.MarkNeeds, Name: app,
					Facts: []string{"is moved without a copy of its config"},
					Why:   "if its new image fails, box has no copy to put back"})
			}
			for _, app := range step.Held {
				held = append(held, say.Row{Mark: say.MarkNeeds, Name: app, Facts: []string{"held at " + res.pins[app] + " by your values"}})
			}
			// A rollback hold keeps an app on the image it ran until a
			// release pins another: the row names the image it waits on, so
			// it does not read as a release that forgot the app.
			for _, app := range step.HeldBack {
				h := res.holds[app]
				held = append(held, say.Row{Mark: say.MarkNeeds, Name: app,
					Facts: []string{"held at " + h.At.Label() + " after a rollback"},
					Why:   "it moves again when a release pins something other than " + h.Avoiding.Label()})
			}
		}
		r.Rows(held)
	}
	// A release a box cannot take is said out loud even when there is nothing
	// to do: "s5n 0.6.0 is out and needs a newer box" is the whole reason a
	// user reads the output of a current box.
	var rows []say.Row
	for _, skip := range res.Skips {
		rows = append(rows, say.Row{Mark: say.MarkNeeds, Facts: []string{
			fmt.Sprintf("%s %s is out and needs box %s or newer: install it, or `box update` stays on %s", res.Name, skip.Version, skip.MinBox, res.Target)}})
	}
	for _, bad := range res.Skipped {
		rows = append(rows, say.Row{Mark: say.MarkNeeds, Facts: []string{
			fmt.Sprintf("%s %s failed to update on this box before; box takes it again when a newer release is out", res.Name, bad)}})
	}
	if check {
		rows = append(rows, say.Row{Mark: say.MarkNote, Facts: []string{"--check: nothing was written but the check time"}})
	}
	r.Rows(rows)
}

// planHeading is the update's first line: what moves, how many steps it takes,
// and which apps the image move touches -- the whole plan in one fact per
// column, because a person reads this and then decides.
func planHeading(res Result) string {
	facts := []string{plural(len(res.Steps), "step", "steps")}
	apps := movingText(res.Steps)
	if apps == "" {
		facts = append(facts, "no image moves")
	} else {
		verb := "move"
		if strings.Count(apps, ",") == 0 && !strings.Contains(apps, " and ") {
			verb = "moves"
		}
		facts = append(facts, apps+" "+verb)
	}
	return fmt.Sprintf("%s %s → %s · %s", res.Name, res.Installed, res.Target, strings.Join(facts, " · "))
}

// movingText names the apps the whole path moves, once each, in step order, as
// a person says them: "prowlarr and sonarr".
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
	return andList(apps)
}

// andList joins names the way a sentence does: "a", "a and b", "a, b and c".
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// take is one step: stop and snapshot what moves, apply, gate, and then either
// record the step or record why it stopped. A failure before the apply puts
// back the app replicas this run stopped; a failure at a gate leaves the
// release, and the apps, where the step put them.
func (u *Updater) take(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, base release.Options) error {
	r := u.report()
	name := step.From + " → " + step.To
	handle := r.Start(say.Op{Name: name, Label: fmt.Sprintf("updating %s to %s", res.Name, step.To)})

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

	copied := step.Copied()
	var taken Taken
	if len(copied) > 0 {
		// The copy is one step of its own on screen: it stops the apps, which
		// is long enough to be worth watching.
		snap := r.Start(say.Op{Name: "snapshots", Label: "stopping " + andList(copied) + " to copy their config"})
		// Whether the copy can be taken is asked before anything stops, so a
		// box that cannot copy refuses with nothing down and nothing
		// recorded: the box refused, not the release.
		if err := snaps.Check(ctx, step.Step, copied); err != nil {
			err = fmt.Errorf("update %s to %s: %s cannot be snapshotted: %w", res.Name, step.To, strings.Join(copied, ", "), err)
			handle.Failed(err)
			return err
		}
		// Every failure down to the apply is this run's own doing -- it
		// stopped apps nothing has asked to move yet -- so it starts them
		// again and returns the step's error. None of it is a failed
		// release: the release was never tried.
		if err := scaleApps(ctx, cluster, copied, 0); err != nil {
			u.startAgain(ctx, cluster, copied)
			err = fmt.Errorf("update %s to %s: stopping %s: %w", res.Name, step.To, strings.Join(copied, ", "), err)
			handle.Failed(err)
			return err
		}
		if err := waitGone(ctx, cluster, copied, u.settleWait(), u.poll()); err != nil {
			u.startAgain(ctx, cluster, copied)
			err = fmt.Errorf("update %s to %s: waiting for %s to stop: %w", res.Name, step.To, strings.Join(copied, ", "), err)
			handle.Failed(err)
			return err
		}
		taken, err = snaps.Take(ctx, step.Step, copied, step.Images)
		if err != nil {
			u.startAgain(ctx, cluster, copied)
			err = fmt.Errorf("update %s to %s: snapshotting %s: %w", res.Name, step.To, strings.Join(copied, ", "), err)
			handle.Failed(err)
			return err
		}
		snap.Done(strings.Join(copied, ", "))
	}

	applyOpts := base
	// The step's own release, with the operator's files untouched. Chart is
	// left nil so this apply pulls by version exactly as a bare apply would.
	applyOpts.Chart = nil
	applyOpts.ChartVersion = step.To
	// This apply moves the release without running the chart's hooks. The
	// wiring Job waits for every app to answer, and a moved app whose new
	// image never starts cannot: a hook run here would spend its whole budget
	// and fail the upgrade, and the step's own gate -- the one that can tell a
	// crashloop from a slow migration -- would never look. The state that
	// stays is applied once more, with the hooks on, below.
	applyOpts.SkipHooks = true
	gate, outcome, err := u.applyAndGate(ctx, rel, cluster, step, applyOpts)
	if err != nil {
		handle.Failed(err)
		return u.fail(ctx, rel, cluster, res, step, before.Revision, gate, err, "")
	}
	// The release has moved: the step's own row goes down here, above what the
	// gate and the wiring did about it, because they are that step's story.
	handle.Done("")

	var rolledBack []string
	var rec release.Receipt
	if len(outcome.Exiting) > 0 {
		// putBack's own apply is the hooks-on one: it installs the release
		// once more with the app held at the image it ran, so the wiring runs
		// against the images that stay.
		rolledBack, rec, err = u.putBack(ctx, rel, cluster, snaps, res, step, applyOpts, taken, outcome.Exiting, before.Revision)
		if err != nil {
			return err
		}
	} else if rec, err = u.wire(ctx, rel, cluster, step, applyOpts); err != nil {
		// No app was put back, so the hooks-on apply is this one: the same
		// release, installed again so the wiring runs against the images that
		// stay.
		return u.fail(ctx, rel, cluster, res, step, before.Revision, gateWiring, err, "")
	}
	reportReceipt(r, rec)

	if len(copied) > 0 {
		if perr := snaps.Prune(ctx, step.Step, copied); perr != nil {
			// A snapshot that cannot be pruned is not a failed step: the
			// release and the box are healthy, and keeping one extra copy
			// costs disk, not correctness.
			r.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{"keeping the snapshot for " + step.To + ": " + perr.Error()}}})
		}
	}
	// An app box put back stays on its old image until a release pins another,
	// and that is the next thing the operator needs to know. It is the last
	// thing said about the step, with the one command that shows it.
	for _, app := range rolledBack {
		change := step.Images[app]
		r.Rows([]say.Row{{
			Mark:    say.MarkNeeds,
			Facts:   []string{fmt.Sprintf("%s is held on %s until a release ships a fixed image", app, change.Ran.Label())},
			Command: "box status",
		}})
	}
	if err := u.recordOK(res, step, before.Revision, rolledBack); err != nil {
		return fmt.Errorf("%s is on %s, but update's record could not be written: %w", res.Name, step.To, err)
	}
	// The step is recorded, so it is real: the apps box put back first, because
	// that is the order they happened in, and then the step itself.
	for _, app := range rolledBack {
		change := step.Images[app]
		u.notify(ctx, Notice{
			Event: EventRolledBack, Name: res.Name, From: step.From, To: step.To,
			Apps: []AppChange{{App: app, From: change.Ran.Label(), To: change.MovedTo.Label()}},
		})
	}
	u.notify(ctx, Notice{
		Event: EventUpdated, Name: res.Name, From: step.From, To: step.To,
		Apps: movedApps(step, rolledBack),
	})
	return nil
}

// wire applies the step's release once more with the chart's hooks on: the one
// run of the wiring Job a step makes.
//
// It is called only when no moved app was put back. A step that rolled one back
// makes the same apply inside stepBack, with the hold in place, so the wiring
// runs against the images that stay -- once, not twice, and never against an app
// box has just declared broken (a wiring Job waits for every app to answer, so
// it cannot pass while one of them is down).
//
// Its failure is the chart's own: the apps are the ones the gate just read as
// ready, so a wiring Job that cannot finish is the release's, and the step stops
// where it is.
func (u *Updater) wire(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options) (release.Receipt, error) {
	wireOpts := applyOpts
	wireOpts.SkipHooks = false
	wireOpts.RunHooks = true
	applied, err := u.Apply(ctx, wireOpts)
	if err != nil {
		return applied.Receipt, fmt.Errorf("applying %s with its hooks: %w", step.To, err)
	}
	// The hooks have run: the release is read once more, because a Job that
	// writes what an app reads at start is only worth anything if the app is
	// still ready after it.
	outcome, err := waitHealthy(ctx, rel, cluster, step.Moving, u.healthWait(), u.poll())
	if err != nil {
		return applied.Receipt, err
	}
	if len(outcome.Exiting) > 0 {
		return applied.Receipt, errors.New(exitingText(outcome.Exiting))
	}
	if outcome.TimedOut {
		return applied.Receipt, fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait())
	}
	return applied.Receipt, nil
}

// reportReceipt folds a wiring receipt into the one row a person reads: a count
// when every check passed, and only the rows somebody has to do something about
// otherwise. The receipt itself -- every check, its result and its detail --
// goes to the run log.
func reportReceipt(r say.Reporter, rec release.Receipt) {
	if rec.Status == "" && len(rec.Steps) == 0 {
		return
	}
	for _, step := range rec.Steps {
		detail := step.Detail
		if detail != "" {
			detail = " (" + detail + ")"
		}
		r.Detail(fmt.Sprintf("wiring %s: %s%s", step.Name, step.Result, detail))
	}
	if rec.Status == "ok" {
		r.Rows([]say.Row{{Mark: say.MarkDone, Name: "wiring", Facts: []string{plural(len(rec.Steps), "check", "checks")}}})
		return
	}
	r.Detail("wiring " + rec.Status + ": " + rec.Error)
	var rows []say.Row
	for _, step := range rec.Steps {
		switch step.Result {
		case "pending":
			rows = append(rows, say.Row{Mark: say.MarkNeeds, Name: step.Name, Facts: []string{step.Detail}})
		case "failed":
			rows = append(rows, say.Row{Mark: say.MarkFailed, Name: step.Name, Facts: []string{step.Detail}})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, say.Row{Mark: say.MarkFailed, Name: "wiring", Facts: []string{rec.Error}})
	}
	r.Rows(rows)
}

// putBack is a step's answer to moved apps that keep exiting: it rolls them
// back and returns them, or ends the step through fail. Only an app whose
// config box copied can be put back. One the catalog gives nothing to copy
// was never stopped or copied, and rolling it back would be a scale-zero with
// nothing to restore, so the failure is the gate's, unchanged.
func (u *Updater) putBack(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, applyOpts release.Options, taken Taken, exiting []Exiting, revision int) ([]string, release.Receipt, error) {
	for _, app := range exiting {
		if !step.Images[app.App].Snapshot {
			err := errors.New(exitingText(exiting))
			return nil, release.Receipt{}, u.fail(ctx, rel, cluster, res, step, revision, gateHealth, err, "")
		}
	}
	rolledBack, rec, err := u.stepBack(ctx, rel, cluster, snaps, res, step, applyOpts, taken, exiting)
	if err == nil {
		return rolledBack, rec, nil
	}
	tail := ""
	var failed *rollbackFailure
	if errors.As(err, &failed) {
		tail = fmt.Sprintf("; %s's snapshot is at %s", failed.app, failed.dir)
		if failed.stopped {
			tail = fmt.Sprintf("; %s is stopped", failed.app) + tail
		}
	}
	return nil, rec, u.fail(ctx, rel, cluster, res, step, revision, gateRollback, err, tail)
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
//
// tail is the part of the message box can only know after the failure, such as
// where a failed step-back left an app's snapshot; it goes last, after the
// release's state, because it is what the operator acts on.
func (u *Updater) fail(ctx context.Context, rel Release, cluster Cluster, res *Result, step StepPlan, revision int, gate string, cause error, tail string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cleanupWait())
	defer cancel()

	// Nothing is said here: the error this returns is the one ✗ line, and a
	// second sentence about the same failure is the noise the rulebook is
	// against. What the operator acts on -- which release is deployed, where a
	// snapshot is -- is in that error.

	// Nothing else puts these replicas back when Helm never moved, and a
	// second failure here belongs in the error rather than in a shrug.
	var startErr error
	copied := step.Copied()
	if gate == gateApply && len(copied) > 0 {
		if err := scaleApps(cctx, cluster, copied, 1); err != nil {
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
	if startErr != nil {
		msg += fmt.Sprintf("; starting %s again failed: %v", strings.Join(copied, ", "), startErr)
	}
	msg += tail
	if recordErr := u.recordFailed(res, step, revision, gate, msg); recordErr != nil {
		return fmt.Errorf("%s; the record could not be written: %w", msg, recordErr)
	}
	// Which release the box is on is Helm's answer, read above: a notice that
	// guessed would be the one sentence someone acts on.
	on := ""
	if stateErr == nil && state.Exists {
		on = state.ChartVersion
	}
	u.notify(ctx, Notice{
		Event: EventStopped, Name: res.Name, From: step.From, To: step.To,
		Gate: gate, Error: cause.Error(), On: on,
	})
	return errors.New(msg)
}

// scaleApps sets every named app's replicas. It is one app's StatefulSets and
// its instances together, which is what both a step and a hand rollback stop
// and start.
func scaleApps(ctx context.Context, cluster Cluster, apps []string, replicas int32) error {
	for _, app := range apps {
		if err := cluster.Scale(ctx, app, replicas); err != nil {
			return fmt.Errorf("%s: %w", app, err)
		}
	}
	return nil
}

// startAgain starts a step's apps again with the step's cleanup budget.
func (u *Updater) startAgain(ctx context.Context, cluster Cluster, apps []string) {
	startApps(ctx, cluster, apps, u.cleanupWait())
}

// startApps starts apps again after a failure that never reached the release. It
// is best effort on purpose: the failure box reports is the one that matters,
// and a second one here would only hide it. It carries its own deadline because
// detaching from a cancelled context drops the parent's as well, and an API
// server that stopped answering must not hold a failed run open.
func startApps(ctx context.Context, cluster Cluster, apps []string, budget time.Duration) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	_ = scaleApps(cctx, cluster, apps, 1)
}

// waitGone waits for the stopping apps' pods to go, so the snapshot is taken
// with no writer, and a restore writes into a directory nothing is reading.
func waitGone(ctx context.Context, cluster Cluster, apps []string, budget, poll time.Duration) error {
	waited, err := wait.Until(ctx, budget, poll, func(ctx context.Context) (bool, error) {
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
		return fmt.Errorf("pods still running after %s", budget)
	}
	return nil
}

// applyAndGate runs one step's apply and its gate. An apply that returned an
// error is not taken at face value while a moved app is restarting (see
// afterFailedApply); everything else is today's flow -- the apply's own failure,
// its wiring receipt, or the health gate.
func (u *Updater) applyAndGate(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options) (string, GateResult, error) {
	applied, err := u.Apply(ctx, applyOpts)
	if err != nil {
		gate := gateApply
		if applied.Applied {
			// Helm moved the release; what failed after that is the wiring
			// receipt apply waits for.
			gate = gateWiring
		}
		return u.afterFailedApply(ctx, rel, cluster, step, applyOpts, gate, err)
	}
	outcome, err := waitHealthy(ctx, rel, cluster, step.Moving, u.healthWait(), u.poll())
	if err != nil {
		return gateHealth, GateResult{}, err
	}
	if outcome.TimedOut {
		return gateHealth, GateResult{}, fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait())
	}
	return "", outcome, nil
}

// afterFailedApply is the second look at a step whose apply failed. Helm's own
// upgrade wait gives up the moment a pod reports CrashLoopBackOff, which is
// after the first exit, so a failed apply is where an image that never starts
// shows up -- not the health gate, which only runs once Helm says deployed. So
// box reads the moved apps once: if none has restarted, the apply failed for its
// own reason and is reported as before. If one has, the step goes into the
// health gate's watch instead:
//
//   - an app reaching three exits is the crashloop the step-back handles;
//   - every moved app running again means the image was merely slow, so the
//     apply is run once more (Helm records a deployed revision and runs its
//     hooks) and the normal gate decides;
//   - the watch running out is an ordinary health-gate failure.
func (u *Updater) afterFailedApply(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options, gate string, cause error) (string, GateResult, error) {
	if len(step.Moving) == 0 {
		return gate, GateResult{}, cause
	}
	health, err := cluster.Health(ctx, step.Moving)
	if err != nil {
		// The apply's own failure is the answer box has; a read that failed
		// after it does not make the step worse.
		return gate, GateResult{}, cause
	}
	if !anyRestarted(health, step.Moving) {
		return gate, GateResult{}, cause
	}
	outcome, err := u.waitRestarting(ctx, cluster, step.Moving)
	if err != nil {
		return gateHealth, GateResult{}, fmt.Errorf("%v (and reading the health after it: %v)", cause, err)
	}
	if len(outcome.Exiting) > 0 {
		return gateHealth, outcome, nil
	}
	if outcome.TimedOut {
		return gateHealth, GateResult{}, fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait())
	}
	// Every moved app is running again: the apply failed around a pod Helm
	// read too early. One more apply records a deployed revision and runs the
	// hooks; a second failure is an ordinary apply failure.
	applied, err := u.Apply(ctx, applyOpts)
	if err != nil {
		gate := gateApply
		if applied.Applied {
			gate = gateWiring
		}
		return gate, GateResult{}, err
	}
	outcome, err = waitHealthy(ctx, rel, cluster, step.Moving, u.healthWait(), u.poll())
	if err != nil {
		return gateHealth, GateResult{}, err
	}
	if outcome.TimedOut {
		return gateHealth, GateResult{}, fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait())
	}
	return "", outcome, nil
}

// anyRestarted reports whether any moved app's container exited at least once
// since the step. One exit is enough: the health gate is what tells a slow start
// from a crashloop, and it costs only a wait.
func anyRestarted(health Health, moving []string) bool {
	for _, app := range moving {
		if health.Restarts[app] >= 1 {
			return true
		}
	}
	return false
}

// rollbackFailure is a step-back that did not finish: what failed, and which
// app's copy box can point the operator at. stopped says the app is still scaled
// to zero because the restore is what failed.
type rollbackFailure struct {
	cause   error
	app     string
	dir     string
	stopped bool
}

func (f *rollbackFailure) Error() string { return f.cause.Error() }

func (f *rollbackFailure) Unwrap() error { return f.cause }

// stepBack puts the apps whose new image keeps exiting back on the image they
// ran, and holds them there.
//
// The order is the one thing that makes it safe: the apps are stopped before
// anything is copied over, the copy is verified and restored before the release
// is re-rendered, and the hold is written before the re-apply, so the chart that
// renders the rolled-back release names the old image. Nothing else in the
// release moves: the rest of the step stays where it is.
func (u *Updater) stepBack(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, stepOpts release.Options, taken Taken, exiting []Exiting) ([]string, release.Receipt, error) {
	r := u.report()
	apps := make([]string, 0, len(exiting))
	for _, e := range exiting {
		apps = append(apps, e.App)
	}
	// Announced before anything stops, so the operator knows why the app that
	// just came up is going down again: the ✗ row is what went wrong with the
	// new image, and the ✓ row below is what box did about it.
	rows := make([]say.Row, 0, len(exiting))
	for _, e := range exiting {
		rows = append(rows, say.Row{Mark: say.MarkFailed, Name: e.App,
			Facts: []string{fmt.Sprintf("crashed %d times on the new image", e.Restarts)}})
	}
	r.Rows(rows)
	if err := scaleApps(ctx, cluster, apps, 0); err != nil {
		return nil, release.Receipt{}, fmt.Errorf("stopping %s: %w", strings.Join(apps, ", "), err)
	}
	if err := waitGone(ctx, cluster, apps, u.settleWait(), u.poll()); err != nil {
		return nil, release.Receipt{}, fmt.Errorf("waiting for %s to stop: %w", strings.Join(apps, ", "), err)
	}
	if err := snaps.Restore(ctx, step.Step, apps); err != nil {
		return nil, release.Receipt{}, &rollbackFailure{cause: err, app: apps[0], dir: taken.Dirs[apps[0]], stopped: true}
	}
	if u.Holds == nil {
		return nil, release.Receipt{}, &rollbackFailure{
			cause: errors.New("this box keeps no holds, so the app cannot stay on its old image"),
			app:   apps[0], dir: taken.Dirs[apps[0]],
		}
	}
	holds, err := loadHolds(u.Holds, res.configRoot)
	if err != nil {
		return nil, release.Receipt{}, &rollbackFailure{cause: err, app: apps[0], dir: taken.Dirs[apps[0]]}
	}
	for _, app := range apps {
		change := step.Images[app]
		holds = snapshot.WithHold(holds, snapshot.Hold{
			App:      app,
			At:       change.Ran,
			Avoiding: change.MovedTo,
			Snapshot: taken.Step,
			Since:    u.now(),
			By:       snapshot.ByUpdate,
		})
		if err := u.Holds.Save(res.configRoot, holds); err != nil {
			return nil, release.Receipt{}, &rollbackFailure{cause: fmt.Errorf("writing the hold for %s: %w", app, err), app: app, dir: taken.Dirs[app]}
		}
	}
	// The same release, so Helm records a deployed revision and runs its
	// hooks; the hold is what makes it render the image the app ran. This is
	// the step's one hooks-on apply: the apply that moved the release ran
	// without them, because the wiring Job cannot pass while an app box is
	// about to put back is down. RunHooks is what makes it install at all --
	// the release is already deployed at this version, so an ordinary apply
	// would read as nothing to change and no hook would run.
	wireOpts := stepOpts
	wireOpts.SkipHooks = false
	wireOpts.RunHooks = true
	applied, err := u.Apply(ctx, wireOpts)
	if err != nil {
		return nil, applied.Receipt, &rollbackFailure{cause: fmt.Errorf("re-applying %s: %w", step.To, err), app: apps[0], dir: taken.Dirs[apps[0]]}
	}
	outcome, err := waitHealthy(ctx, rel, cluster, step.Moving, u.healthWait(), u.poll())
	if err != nil {
		return nil, applied.Receipt, &rollbackFailure{cause: err, app: apps[0], dir: taken.Dirs[apps[0]]}
	}
	if len(outcome.Exiting) > 0 {
		return nil, applied.Receipt, &rollbackFailure{cause: errors.New(exitingText(outcome.Exiting)), app: apps[0], dir: taken.Dirs[apps[0]]}
	}
	if outcome.TimedOut {
		return nil, applied.Receipt, &rollbackFailure{
			cause: fmt.Errorf("the release is not deployed and healthy after %s", u.healthWait()),
			app:   apps[0], dir: taken.Dirs[apps[0]],
		}
	}
	back := make([]say.Row, 0, len(apps))
	for _, app := range apps {
		change := step.Images[app]
		back = append(back, say.Row{Mark: say.MarkDone, Name: app,
			Facts: []string{fmt.Sprintf("rolled back to %s", change.Ran.Label()), "its " + taken.At.In(time.Local).Format("15:04") + " snapshot"}})
	}
	r.Rows(back)
	return apps, applied.Receipt, nil
}

// waitHealthy is the health gate. A moved app that keeps exiting ends it at
// once: the image is what has to change, and waiting for the rest of the
// release would only reach the same answer later with more restarts behind it.
// The apps are read before Helm's status, because an image that never starts
// makes Helm's own wait give up and the release read as failed, and the
// crashloop is still the step's to name. Otherwise it waits for Helm to say
// deployed and for every StatefulSet to be ready, because an app that has not
// opened its port yet may be migrating rather than failing.
//
// It is one function rather than a method because update's step-back and a hand
// rollback both end in it, asking the same question: is the app ready, or does
// it keep exiting?
func waitHealthy(ctx context.Context, rel Release, cluster Cluster, moving []string, budget, poll time.Duration) (GateResult, error) {
	var last Health
	waited, err := wait.Until(ctx, budget, poll, func(ctx context.Context) (bool, error) {
		health, err := cluster.Health(ctx, moving)
		if err != nil {
			return false, err
		}
		last = health
		if len(health.Exiting) > 0 {
			return true, nil
		}
		state, err := rel.State(ctx)
		if err != nil {
			return false, err
		}
		if !state.Exists || !state.Deployed() {
			return false, nil
		}
		return health.Ready, nil
	})
	if err != nil {
		return GateResult{}, err
	}
	if len(last.Exiting) > 0 {
		return GateResult{Exiting: last.Exiting}, nil
	}
	if !waited.Done {
		return GateResult{TimedOut: true}, nil
	}
	return GateResult{}, nil
}

// waitRestarting watches a release whose apply failed while a moved app was
// restarting: it ends when an app reaches the crashloop count, when every moved
// app's StatefulSet is ready again, or when the budget runs out. Helm's own
// status is not read: it says failed until box applies again, and that is exactly
// what this watch decides.
func (u *Updater) waitRestarting(ctx context.Context, cluster Cluster, moving []string) (GateResult, error) {
	var last Health
	waited, err := wait.Until(ctx, u.healthWait(), u.poll(), func(ctx context.Context) (bool, error) {
		health, err := cluster.Health(ctx, moving)
		if err != nil {
			return false, err
		}
		last = health
		return health.Ready || len(health.Exiting) > 0, nil
	})
	if err != nil {
		return GateResult{}, err
	}
	if len(last.Exiting) > 0 {
		return GateResult{Exiting: last.Exiting}, nil
	}
	if !waited.Done {
		return GateResult{TimedOut: true}, nil
	}
	return GateResult{}, nil
}

// exitingText is the cause box records when the apps a step moved keep exiting:
// one clause per app, so the record and the sentence name every one of them and
// not just the first the gate saw.
func exitingText(apps []Exiting) string {
	clauses := make([]string, 0, len(apps))
	for _, app := range apps {
		clauses = append(clauses, fmt.Sprintf("%s exited %d times on its new image", app.App, app.Restarts))
	}
	return strings.Join(clauses, "; ")
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
func (u *Updater) recordOK(res *Result, step StepPlan, revision int, rolledBack []string) error {
	res.state.LastStep = &StepRecord{
		From:       step.From,
		To:         step.To,
		At:         u.now(),
		Outcome:    OutcomeOK,
		Revision:   revision,
		Moved:      step.Moving,
		RolledBack: rolledBack,
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

// orDuration is a configured deadline or its default. Zero is "the caller has
// no opinion", which is why a test that wants a short wait sets one and the
// operator's box never does.
func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// movingNames names the apps a step moves, with "(no snapshot)" after one whose
// catalog entry has nothing to copy: the copy is what a rollback restores, so an
// app without one is never rolled back, and the plan should not leave the
// operator to find that out from a failed step.
func movingNames(step StepPlan) string {
	names := make([]string, 0, len(step.Moving))
	for _, app := range step.Moving {
		if step.Images[app].Snapshot {
			names = append(names, app)
			continue
		}
		names = append(names, app+" (no snapshot)")
	}
	return strings.Join(names, ", ")
}

// aStepCopies reports whether any step copies an app's config, which is what
// decides whether update needs a snapshotter at all.
func aStepCopies(steps []StepPlan) bool {
	for _, step := range steps {
		if len(step.Copied()) > 0 {
			return true
		}
	}
	return false
}

// copyingText names every app the path copies, once, in step order. It is used
// in the one sentence box says when it cannot snapshot.
func copyingText(steps []StepPlan) string {
	var apps []string
	seen := make(map[string]bool)
	for _, step := range steps {
		for _, app := range step.Copied() {
			if seen[app] {
				continue
			}
			seen[app] = true
			apps = append(apps, app)
		}
	}
	return strings.Join(apps, ", ")
}
