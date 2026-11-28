// Package update is `box update`: it moves a seedbox to a newer chart release.
// Helm's deployed release is the truth, and upgrade-from floors decide the steps,
// each with its own apply, wiring gate and health gate. The values file is never
// rewritten; a moving image is snapshotted with its app stopped, so a rollback
// over a forward-only database is real.
//
// box puts one thing back: a moved app that keeps exiting is restored from this
// step's copy and held on its old image. An app with nothing to copy stops
// instead, and an app that is running but not listening is waited for.
package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/wait"
)

// ChartSource reads one release by version and every published release from a
// floor up.
type ChartSource interface {
	release.ChartSource
	// Releases lists published releases at or above floor, oldest first; an
	// empty list means the box is current, not an error.
	Releases(ctx context.Context, ref release.ChartRef, floor string) ([]release.Published, error)
}

// Release is the deployed release as Helm reports it; update never moves it
// back, so the interface holds the read alone.
type Release interface {
	State(ctx context.Context) (release.ReleaseState, error)
}

// Cluster is the release's StatefulSets and pods: scale down, wait for pods to
// go, later ask what is ready and what keeps exiting.
type Cluster interface {
	// Scale sets every StatefulSet of one app, instances included. An app with
	// none is not an error: there is nothing to stop.
	Scale(ctx context.Context, app string, replicas int32) error
	// Gone reports whether every pod of that app is gone.
	Gone(ctx context.Context, app string) (bool, error)
	// Health reads whether every StatefulSet is ready and which moved apps keep
	// exiting; an app that kept its image cannot be this step's fault.
	Health(ctx context.Context, moving []string) (Health, error)
	// Running reads each app's asked-for reference and reported digest: what
	// runs, which an apply compares with what it will render.
	Running(ctx context.Context) (map[string]Running, error)
}

// Snapshotter copies the apps a step moves, so box or the operator can put one
// back; ConfigSnapshots implements it.
type Snapshotter interface {
	// Check reports whether a copy can be taken, without stopping or writing.
	// A refusal ends the update with nothing stopped.
	Check(ctx context.Context, step Step, apps []string) error
	// Take copies the named apps' stopped config and says where. It must fail
	// rather than leave a partial snapshot; an image it cannot name is refused
	// before anything is copied.
	Take(ctx context.Context, step Step, apps []string, images map[string]ImageChange) (Taken, error)
	// Prune keeps the newest two snapshots per app.
	Prune(ctx context.Context, step Step, apps []string) error
	// Restore puts the named apps back to this step's copy, only for apps whose
	// new image kept exiting.
	Restore(ctx context.Context, step Step, apps []string) error
}

// Taken is one step's copy: its name, time and each app's directory.
type Taken struct {
	// Step is the step directory's name, which a hold records.
	Step string
	// At is when the copy was taken, in UTC.
	At time.Time
	// Dirs is each app's copy, named when a rollback fails.
	Dirs map[string]string
}

// ApplyFunc runs one step's apply, the same plan, diff, write and wiring gate
// as an operator's `box apply`.
type ApplyFunc func(ctx context.Context, opts release.Options) (release.Result, error)

// Options are one `box update`.
type Options struct {
	// Apply is the apply every step runs. Its ChartVersion is the operator's
	// target, which each step overwrites with the release it installs.
	Apply release.Options
	// Check is `box update --check`: plan and report, write nothing but
	// lastCheck.
	Check bool
}

// The gates a step passes, as the record names them.
const (
	gateApply    = "apply"
	gateWiring   = "wiring"
	gateHealth   = "health"
	gateRollback = "rollback"
)

// GateResult is how the health gate ended: ready, an app that keeps exiting
// (the image's fault, named as such), or a wait that ran out (running but not
// ready, left where it is). A read error is separate.
type GateResult struct {
	// Exiting is the moved apps whose containers keep exiting; the gate stops
	// on the first reading rather than wait for the rest.
	Exiting []Exiting
	// TimedOut is the wait running out with no moved app exiting.
	TimedOut bool
}

// StepPlan is one step and the apps its image move touches.
type StepPlan struct {
	Step
	// Moving is the apps whose image differs between the releases; the subset
	// with state to copy is what a step stops and snapshots.
	Moving []string
	// Images is each moving app's image on either side; a missing variant has a
	// zero reference, which makes the copy refuse.
	Images map[string]ImageChange
	// Held is the apps a values pin keeps in place; the plan names them so the
	// pin does not read as a forgotten app.
	Held []string
	// HeldBack is the apps a rollback hold keeps on their image until a release
	// pins another; the plan names the image it waits on.
	HeldBack []string
	// Digest is the target release's manifest digest, which its signature covers.
	Digest string
}

// Copied is the moving apps whose catalog names files to copy; one without them
// is moved uncopied and never rolled back.
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
	// Current says there is nothing to do.
	Current bool
	// Steps is the ordered path to the target.
	Steps []StepPlan
	// Skips are releases box passed over because their min-box is above it.
	Skips []release.VersionSkip
	// Skipped are versions a previous run failed on and selection passes over.
	Skipped []string
	// Taken is the steps this run completed, in order.
	Taken []Step

	// state is the record this run read and keeps current.
	state State
	// values are this run's effective values, which name the instances copied.
	values map[string]any
	// pins is each held app's pin label, for the report, which has no values.
	pins map[string]string
	// holds are the rollback holds the plan honoured, by app.
	holds map[string]snapshot.Hold
	// configRoot is where holds, pins and snapshots live.
	configRoot string
	// pinWarnings are the pins the registry did not answer, said before the
	// steps; pinNotes are the pins the chart has moved past.
	pinWarnings []string
	pinNotes    []string
	// pinDigests is what every step's apply renders for a tag pin, and
	// pinAnswers are this run's resolutions, whose records are written once
	// every step has succeeded.
	pinDigests map[string]string
	pinAnswers []pin.Answer
}

// Updater is `box update`'s whole decision.
type Updater struct {
	// Charts is the chart repository: published releases only.
	Charts ChartSource
	// Open returns the release handle for the grant's namespace.
	Open func(namespace string) (Release, error)
	// OpenCluster returns the release's StatefulSets; `--check` never opens it.
	OpenCluster func(namespace string) (Cluster, error)
	// OpenSnapshots returns the copy for the apps a step snapshots. It is opened
	// only when a step copies; such a box without one refuses before stopping
	// anything.
	OpenSnapshots func(namespace string, values map[string]any) (Snapshotter, error)
	// Apply runs one step.
	Apply ApplyFunc
	// Confirm asks the one question before the first write; each step's apply
	// runs pre-confirmed, or a multi-step path would ask after it had moved.
	Confirm release.Confirmer
	// State is update.json.
	State StateStore
	// Notify is told what each step did; nil posts nothing.
	Notify func(context.Context, Notice)
	// Holds is the record of apps held at an older image; nil keeps none.
	Holds HoldStore
	// Pins resolves a tag pin against the registry; nil refuses a tag pin, as a
	// box that cannot ask has nothing to render it with.
	Pins pin.Resolver
	// PinStore is where a resolved tag pin is recorded; nil keeps none, and a tag
	// pin then refuses before the plan.
	PinStore PinStore
	// BoxVersion is compared with every release's min-box; "dev" skips it.
	BoxVersion string
	// Say is every line update writes.
	Say say.Reporter

	// HealthWait bounds the gate after a step; zero is thirty-five minutes,
	// outlasting the chart's thirty-minute startup budget.
	HealthWait time.Duration
	// SettleWait bounds the wait for scaled-down pods to go. Zero means five
	// minutes.
	SettleWait time.Duration
	// CleanupWait bounds a failed step's cleanup after its context is gone;
	// zero is five minutes.
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

// DefaultHealthWait is the gate's budget: every app gets thirty minutes to open
// its port (the chart's startupProbe, 360 failures of 5s), so the gate must
// outlast it. A step's Helm wait uses the same budget (cmd/box sets it), so a
// long migration is not failed before this gate can see it.
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
	// A box that cannot copy gives the operator no way back, so say so first.
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
	ok, err := u.Confirm.Ask(fmt.Sprintf("update %s from %s to %s?", res.Name, res.Installed, res.Target), false)
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
	// The digests are what the tags resolved to before the steps; recording them
	// now means a step that moved nothing cannot claim a check it did not make,
	// and a failed run keeps the record it started with.
	if err := u.settlePins(res); err != nil {
		return res, fmt.Errorf("%s is on %s, but the pins could not be written: %w", res.Name, res.Target, err)
	}
	return res, nil
}

// settlePins writes this run's resolutions after the steps succeeded.
func (u *Updater) settlePins(res Result) error {
	added := pin.Records(res.pinAnswers, res.Target, u.now())
	if len(added) == 0 {
		return nil
	}
	records, err := u.PinStore.Load(res.configRoot)
	if err != nil {
		return err
	}
	for _, record := range added {
		records = snapshot.WithPin(records, record)
	}
	return u.PinStore.Save(res.configRoot, records)
}

// plan is every read: files, deployed release, published releases, target and
// path. It writes only the check time.
func (u *Updater) plan(ctx context.Context, opts Options) (Result, error) {
	res := Result{}
	if u.Charts == nil {
		return res, errors.New("no chart source: box reads its chart from the registry")
	}
	if u.State == nil {
		return res, errors.New("no state store: update records what it last checked and failed")
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
	// The grant's config root is where copies and holds live.
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
	// List from the installed release: an update never moves backwards, and
	// older charts may not even be readable today.
	releases, err := u.Charts.Releases(ctx, release.ChartRef{Repo: repo}, installed.ChartVersion)
	if err != nil {
		return res, err
	}
	// Load the installed chart even when current: its name and variants are
	// the old side of every comparison.
	fromChart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: installed.ChartVersion})
	if err != nil {
		return res, fmt.Errorf("the box is on chart %s, which cannot be read: %w", installed.ChartVersion, err)
	}
	res.Name = fromChart.Name
	if res.Name == "" {
		res.Name = release.ReleaseName
	}
	// Nothing at or above installed: current. A named target is still read.
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

	// A named target can be a prerelease the listing leaves out; its floors are
	// the chart's own annotations.
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
		// A hand-installed future release makes selection land older; that is
		// current, but a named backward target is refused.
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
	// The target's effective values are what apply installs and what this run
	// is checked against.
	toChart, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: target})
	if err != nil {
		return res, err
	}
	values, err := sb.Effective(seedbox.Chart{Values: toChart.Values, Schema: toChart.Schema})
	if err != nil {
		return res, err
	}
	res.values = values
	var records []snapshot.Pin
	if u.PinStore != nil {
		if records, err = u.PinStore.Load(root); err != nil {
			return res, err
		}
	}
	tagPins, err := pin.TaggedPins(toChart.Variants, values, release.EnabledApps(values), records)
	if err != nil {
		return res, err
	}
	if len(tagPins) > 0 && u.PinStore == nil {
		return res, fmt.Errorf("%s is pinned to a tag and this box keeps no pin file: `box pin` records the digest it renders", strings.Join(pinApps(tagPins), ", "))
	}
	answers, err := pin.Resolve(ctx, u.Pins, tagPins)
	if err != nil {
		return res, err
	}
	res.pinAnswers = answers
	res.pinDigests = pin.Digests(answers)
	res.pinWarnings = pin.Warnings(answers)
	res.pinNotes = chartMovedPastPins(answers, toChart, values)
	res.Steps, res.pins, res.holds, err = u.stepsPlan(ctx, repo, fromChart, steps, values, holds, answersByApp(answers))
	if err != nil {
		return res, err
	}

	res.state.LastCheck = u.now()
	if err := u.State.Save(res.state); err != nil {
		return res, err
	}
	return res, nil
}

// stepsPlan compares each step's images with the release before it, and
// carries held pins and the catalog's copy globs for the report.
func (u *Updater) stepsPlan(ctx context.Context, repo string, from release.Chart, steps []Step, values map[string]any, holds []snapshot.Hold, answers map[string]pin.Answer) ([]StepPlan, map[string]string, map[string]snapshot.Hold, error) {
	var plans []StepPlan
	pins := make(map[string]string)
	heldByRollback := make(map[string]snapshot.Hold)
	for _, s := range steps {
		to, err := u.Charts.Load(ctx, release.ChartRef{Repo: repo, Version: s.To})
		if err != nil {
			return nil, nil, nil, err
		}
		moving, held, heldBack, images, err := movingApps(from, to, values, holds, answers)
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
		plans = append(plans, StepPlan{Step: s, Moving: moving, Images: images, Held: held, HeldBack: heldBack, Digest: to.Digest})
		from = to
	}
	return plans, pins, heldByRollback, nil
}

// target picks the named version, or the newest this box can use. Releases a
// previous run failed on are passed over unless named.
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
		// A failure the box already moved past is history, not a skip.
		if cmp, err := compareVersions(bad, chosen); err == nil && cmp > 0 {
			skipped = append(skipped, bad)
		}
	}
	return chosen, skipped, skips, nil
}

// reportPlan says what this run is, then only the rows a person acts on: a pin,
// a skipped release, and what --check did not write.
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
		// Say why an app the chart moved stays: a plan that looks forgetful is
		// worse than one that names the pin.
		var held []say.Row
		pinned := make(map[string]bool, len(res.pinAnswers))
		for _, answer := range res.pinAnswers {
			pinned[answer.App] = true
		}
		for _, step := range res.Steps {
			// A pin's rebuilt tag is a move the heading cannot show: both sides
			// read as the same version. A chart bump's row would be noise.
			for _, app := range step.Moving {
				change := step.Images[app]
				move := release.SnapshotMove{App: app, Ran: change.Ran, MovedTo: change.MovedTo, Copied: change.Snapshot}
				if pinned[app] && move.Rebuilt() {
					held = append(held, say.Row{Mark: say.MarkNote, Facts: []string{move.Row()}})
				}
			}
			// An app with nothing to copy is never rolled back; say so before
			// the step, because it changes what a failure costs.
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
			// Name the image a rollback hold waits on, so it does not read as a
			// forgotten app.
			for _, app := range step.HeldBack {
				h := res.holds[app]
				held = append(held, say.Row{Mark: say.MarkNeeds, Name: app,
					Facts: []string{"held at " + h.At.Label() + " after a rollback"},
					Why:   "it moves again when a release pins something other than " + h.Avoiding.Label()})
			}
		}
		r.Rows(held)
	}
	// Say a release this box cannot take even when current: that is why a user
	// reads the output.
	var rows []say.Row
	for _, skip := range res.Skips {
		rows = append(rows, say.Row{Mark: say.MarkNeeds, Facts: []string{
			fmt.Sprintf("%s %s is out and needs box %s or newer: install it, or `box update` stays on %s", res.Name, skip.Version, skip.MinBox, res.Target)}})
	}
	for _, bad := range res.Skipped {
		rows = append(rows, say.Row{Mark: say.MarkNeeds, Facts: []string{
			fmt.Sprintf("%s %s failed to update on this box before; box takes it again when a newer release is out", res.Name, bad)}})
	}
	for _, warning := range res.pinWarnings {
		rows = append(rows, say.Row{Mark: say.MarkNeeds, Facts: []string{warning}})
	}
	if check {
		rows = append(rows, say.Row{Mark: say.MarkNote, Facts: []string{"--check: nothing was written but the check time"}})
	}
	r.Rows(rows)
	for _, note := range res.pinNotes {
		r.Muted(note)
	}
}

// pinApps names the tag pins for a refusal.
func pinApps(pins []pin.Tagged) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, p.App)
	}
	return out
}

// answersByApp is this run's resolutions by app, for the step that compares
// images.
func answersByApp(answers []pin.Answer) map[string]pin.Answer {
	out := make(map[string]pin.Answer, len(answers))
	for _, a := range answers {
		out[a.App] = a
	}
	return out
}

// chartMovedPastPins says, for each pin the chart has moved past, that its tag
// still gets rebuilds only while its publisher still builds that version. box
// cannot know when someone else stops building it, so it never claims the pin has
// stopped moving.
func chartMovedPastPins(answers []pin.Answer, chart release.Chart, values map[string]any) []string {
	if len(answers) == 0 || len(chart.Variants) == 0 {
		return nil
	}
	images, err := admission.Images(chart.Variants)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range answers {
		image, ok := images[a.App][variantOf(values, a.App)]
		if !ok || image.Tag == "" {
			continue
		}
		version := pin.ShapeOf(a.Tag).Version(a.Tag)
		if version == "" || strings.HasPrefix(image.Tag, version) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: the chart is on %s, and %s gets rebuilds only while its publisher still builds that version",
			a.App, image.Tag, a.Tag))
	}
	return out
}

// planHeading is the update's first line: what moves, in how many steps.
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

// movingText names the apps the path moves, once each, in step order.
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

// take is one step: stop and snapshot, apply, gate, then record what happened.
// A failure before the apply restarts the apps; a gate failure leaves them.
func (u *Updater) take(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, base release.Options) error {
	r := u.report()
	name := step.From + " → " + step.To
	handle := r.Start(say.Op{Name: name, Label: fmt.Sprintf("updating %s to %s", res.Name, step.To)})

	// Read the revision before the first write: a snapshot failure still knows
	// what the box is on and that nothing moved.
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
		// The copy gets its own screen row: it stops the apps.
		snap := r.Start(say.Op{Name: "snapshots", Label: "stopping " + andList(copied) + " to copy their config"})
		// Ask before stopping: a box that cannot copy refuses with nothing
		// down.
		if err := snaps.Check(ctx, step.Step, copied); err != nil {
			err = fmt.Errorf("update %s to %s: %s cannot be snapshotted: %w", res.Name, step.To, strings.Join(copied, ", "), err)
			handle.Failed(err)
			return err
		}
		// Down to the apply, nothing has moved the release: start the apps
		// again and return the step's error.
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
	// Chart is nil so this pulls by version as a bare apply would.
	applyOpts.Chart = nil
	applyOpts.ChartVersion = step.To
	// Move the release without hooks: the wiring Job waits for every app, which
	// a new image that never starts cannot do, so a hook here would spend its
	// budget and hide the crashloop from the gate. The release that stays is
	// applied once more with hooks below.
	applyOpts.SkipHooks = true
	// The plan already resolved every tag pin; the apply renders that answer.
	applyOpts.PinDigests = res.pinDigests
	gate, outcome, err := u.applyAndGate(ctx, rel, cluster, step, applyOpts)
	if err != nil {
		handle.Failed(err)
		return u.fail(ctx, rel, cluster, res, step, before.Revision, gate, err, "")
	}
	// The release has moved; the gate's rows belong under this one.
	handle.Done("")

	var rolledBack []string
	var rec release.Receipt
	if len(outcome.Exiting) > 0 {
		// putBack's apply is the hooks-on one, with the app held on its old
		// image, so wiring runs against the images that stay.
		rolledBack, rec, err = u.putBack(ctx, rel, cluster, snaps, res, step, applyOpts, taken, outcome.Exiting, before.Revision)
		if err != nil {
			return err
		}
	} else if rec, err = u.wire(ctx, rel, cluster, step, applyOpts); err != nil {
		// No app was put back, so this is the hooks-on apply.
		return u.fail(ctx, rel, cluster, res, step, before.Revision, gateWiring, err, "")
	}
	release.ReportReceipt(r, rec, false)

	if len(copied) > 0 {
		if perr := snaps.Prune(ctx, step.Step, copied); perr != nil {
			// An unpruned snapshot is not a failed step: it costs disk, not
			// correctness.
			r.Rows([]say.Row{{Mark: say.MarkNote, Facts: []string{"keeping the snapshot for " + step.To + ": " + perr.Error()}}})
		}
	}
	// A put-back app stays on its old image; say so last, with the command that
	// shows it.
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
	// Notify the put-back apps first, in the order they happened, then the step.
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

// wire applies the step's release once more with hooks on: the step's one run of
// the wiring Job. It is called only when nothing was put back; a rollback's
// apply does the same with the hold in place, so wiring runs once and never
// against an app just declared broken (the Job waits for every app).
func (u *Updater) wire(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options) (release.Receipt, error) {
	wireOpts := applyOpts
	wireOpts.SkipHooks = false
	wireOpts.RunHooks = true
	applied, err := u.Apply(ctx, wireOpts)
	if err != nil {
		return applied.Receipt, fmt.Errorf("applying %s with its hooks: %w", step.To, err)
	}
	// Read the release again: a hook that wrote what an app reads at start is
	// only useful if the app is still ready.
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

// putBack rolls back the moved apps that keep exiting, or ends the step. Only a
// copied app can be put back: one with nothing to copy was never stopped, so the
// gate's failure stands.
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

// fail ends a failed step with Helm's own answer for where the release is, and
// records the release as failed so later runs pass it over. Apps are restarted
// only when Helm never moved: after a gate the new release is what runs. tail is
// what box can only know after the failure, and goes last.
func (u *Updater) fail(ctx context.Context, rel Release, cluster Cluster, res *Result, step StepPlan, revision int, gate string, cause error, tail string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cleanupWait())
	defer cancel()

	// The returned error is the one line; what the operator acts on is in it.

	// Nothing else puts these replicas back when Helm never moved.
	var startErr error
	copied := step.Copied()
	if gate == gateApply && len(copied) > 0 {
		if err := scaleApps(cctx, cluster, copied, 1); err != nil {
			startErr = err
		}
	}

	// Read Helm's answer now, not box's assumption about the step.
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
	// The notice's release is Helm's answer, read above, never a guess.
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

// scaleApps sets every named app's replicas, instances included.
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

// startApps restarts apps after a failure that never reached the release. It is
// best effort and carries its own deadline: detaching from a cancelled context
// drops the parent's, and a silent API server must not hold the run open.
func startApps(ctx context.Context, cluster Cluster, apps []string, budget time.Duration) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	_ = scaleApps(cctx, cluster, apps, 1)
}

// waitGone waits for the apps' pods to go, so a copy has no writer and a
// restore writes into a directory nothing reads.
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

// applyAndGate runs one step's apply and gate; a failed apply while a moved app
// restarts gets the second look in afterFailedApply.
func (u *Updater) applyAndGate(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options) (string, GateResult, error) {
	applied, err := u.Apply(ctx, applyOpts)
	if err != nil {
		gate := gateApply
		if applied.Applied {
			// Helm moved it; the wiring receipt apply waits for is what failed.
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

// afterFailedApply is the second look at a failed apply. Helm's upgrade wait
// gives up on the first CrashLoopBackOff, before the health gate ever runs, so
// box reads the moved apps: if none restarted, the apply's own failure stands.
// Otherwise an app reaching three exits is the crashloop to step back from, all
// apps running again means the image was slow and the apply is run once more,
// and a watch that runs out is an ordinary health-gate failure.
func (u *Updater) afterFailedApply(ctx context.Context, rel Release, cluster Cluster, step StepPlan, applyOpts release.Options, gate string, cause error) (string, GateResult, error) {
	if len(step.Moving) == 0 {
		return gate, GateResult{}, cause
	}
	health, err := cluster.Health(ctx, step.Moving)
	if err != nil {
		// The apply's failure is the answer; a failed read does not worsen it.
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
	// Every app runs again: Helm read a pod too early. Apply once more; a second
	// failure is ordinary.
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

// anyRestarted: one exit is enough; the gate tells a slow start from a
// crashloop.
func anyRestarted(health Health, moving []string) bool {
	for _, app := range moving {
		if health.Restarts[app] >= 1 {
			return true
		}
	}
	return false
}

// rollbackFailure is a step-back that did not finish: the cause, the copy to
// point at, and whether the app is still stopped because the restore failed.
type rollbackFailure struct {
	cause   error
	app     string
	dir     string
	stopped bool
}

func (f *rollbackFailure) Error() string { return f.cause.Error() }

func (f *rollbackFailure) Unwrap() error { return f.cause }

// stepBack puts the apps that keep exiting back on their old image and holds
// them there. Stop before restoring, hold before re-applying, so nothing writes
// over a copy and the chart renders the old image. The rest of the release
// stays.
func (u *Updater) stepBack(ctx context.Context, rel Release, cluster Cluster, snaps Snapshotter, res *Result, step StepPlan, stepOpts release.Options, taken Taken, exiting []Exiting) ([]string, release.Receipt, error) {
	r := u.report()
	apps := make([]string, 0, len(exiting))
	for _, e := range exiting {
		apps = append(apps, e.App)
	}
	// Announce before stopping: the ✗ row is the new image's failure.
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
	// The same release, so Helm records a revision and runs hooks; the hold
	// makes it render the old image. RunHooks makes it install at all: the
	// version is already deployed, so an ordinary apply would change nothing.
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
// once. The apps are read before Helm's status, because a never-starting image
// makes Helm's wait give up first and the crashloop is still the step's to name;
// otherwise it waits for deployed and ready, since a closed port may be a
// migration. update's step-back and a hand rollback share it.
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

// waitRestarting watches a failed apply's moved apps until one reaches the
// crashloop count, all are ready, or the budget runs out. Helm's status is not
// read: it says failed until box applies again, which is what this decides.
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

// exitingText names every exiting app, not just the first the gate saw.
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

// recordOK records a passing step and drops failures at or below it.
func (u *Updater) recordOK(res *Result, step StepPlan, revision int, rolledBack []string) error {
	res.state.LastStep = &StepRecord{
		From:        step.From,
		To:          step.To,
		At:          u.now(),
		Outcome:     OutcomeOK,
		Revision:    revision,
		ChartDigest: step.Digest,
		Moved:       step.Moving,
		RolledBack:  rolledBack,
	}
	res.state.Failed = dropAtOrBelow(res.state.Failed, step.To)
	return u.State.Save(res.state)
}

// recordFailed records the release so later runs skip it until something newer
// exists.
func (u *Updater) recordFailed(res *Result, step StepPlan, revision int, gate, detail string) error {
	res.state.LastStep = &StepRecord{
		From:        step.From,
		To:          step.To,
		At:          u.now(),
		Outcome:     OutcomeFailed,
		Error:       detail,
		Revision:    revision,
		ChartDigest: step.Digest,
		Moved:       step.Moving,
		Gate:        gate,
	}
	if !containsVersion(res.state.Failed, step.To) {
		res.state.Failed = append(res.state.Failed, step.To)
	}
	return u.State.Save(res.state)
}

// dropAtOrBelow removes failures a passing step has carried the box past.
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

// versionsEqual compares semver-wise; a non-version equals only itself.
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

// orDuration is a configured deadline or its default; zero means no opinion.
func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// movingNames names the moved apps, marking one with nothing to copy: it is
// never rolled back, and the plan should not leave that for a failure.
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

// aStepCopies reports whether update needs a snapshotter at all.
func aStepCopies(steps []StepPlan) bool {
	for _, step := range steps {
		if len(step.Copied()) > 0 {
			return true
		}
	}
	return false
}

// copyingText names every copied app once, in step order.
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
