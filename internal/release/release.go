// Package release is `box apply`: it turns one seedbox's two files into the
// release that runs it.
//
// The flow is linear, and every refusal happens before the first cluster write:
// load the grant and the values file, merge them, check the result against the
// chart release's values.schema.json, read the namespace, the storage and what
// is deployed, show the difference, ask, then create the namespace if it is
// missing, write the storage, install or upgrade, and read the wiring receipt
// back. A volume box wrote in a run that did not finish is removed again before
// the error is returned, so a failed first install leaves no cluster-scoped PV.
// Nothing here does the cluster's work by hand: the Helm SDK is the client
// (PLAN.md), so a release box installs is a release `helm` can read, upgrade
// and roll back.
//
// Like internal/bootstrap, the decision is made over small interfaces —
// ChartSource, ReleaseClient and Confirmer — so every branch, including the
// promise that a refusal writes nothing, is tested from fixtures without a
// cluster. The Helm-backed implementations live in this package beside the
// logic (helm.go), and only cmd/box imports them.
package release

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// ReleaseName is the Helm release box owns on root's own seedbox. One release
// is one namespace is one seedbox, so the name is box's decision, not a chart
// value; `s5n` is also what hack/e2e-kind.sh installs under, so the wiring
// receipt (`s5n-wiring`) is the same object whichever path installed it.
const ReleaseName = "s5n"

// ErrCancelled says the operator answered no, or could not be asked. Nothing
// was written; the caller prints the voice and stops.
var ErrCancelled = errors.New("cancelled")

// Chart is the chart release box is about to install, and ChartSource loads
// one; both live in chart.go, because a chart pulled from the registry and the
// fixture copy of the same files have to be the same value to apply.
// ReleaseClient is box's handle on one release in one namespace. It is the
// only thing that touches a cluster, and it is opened only after the two files
// have passed every check, so a refusal cannot write by construction.
type ReleaseClient interface {
	// Deployed returns the values the deployed release was installed with,
	// and false when no release of that name is deployed. A release that
	// exists in another state (failed, pending) is an error: box will not
	// silently install over it.
	Deployed(ctx context.Context) (values map[string]any, exists bool, err error)

	// Upgrade installs the release when it does not exist and upgrades it
	// when it does, then waits for it, including its wiring hook. It returns
	// the new revision.
	Upgrade(ctx context.Context, chart Chart, values map[string]any) (revision int, err error)

	// Receipt reads the wiring receipt the release publishes. ErrNoReceipt
	// means the hook has not published yet, which is not a fault.
	Receipt(ctx context.Context) (Receipt, error)
}

// ErrNoReceipt says the wiring ConfigMap (or its status.json) is not there
// yet. The wiring Job is a hook, so a receipt can lag the release by a moment.
var ErrNoReceipt = errors.New("no wiring receipt yet")

// Receipt is the subset of `<release>-wiring`'s status.json box prints: the
// overall state and one row per wiring step. The full document belongs to
// internal/wiring; box reads only what it shows.
type Receipt struct {
	Status    string
	StartedAt time.Time
	Error     string
	Steps     []Step
}

// Step is one row of the receipt.
type Step struct {
	Name   string
	Result string
	Detail string
}

// Confirmer asks the one question. It is an interface so the prompt's "no"
// path can be tested, and because the voice is the only thing that varies
// between an interactive box and --yes.
type Confirmer interface {
	Ask(question string) (bool, error)
}

// Options are one `box apply`.
type Options struct {
	// Chart is a chart release the caller has already loaded. `box init` sets
	// it: init reads the chart to ask its questions, and the apply it hands
	// over to must install that same release rather than pull a second time
	// and risk a different one.
	Chart *Chart
	// ChartRepo and ChartVersion name the published release to load when
	// Chart is nil. An empty version means the newest release this box can
	// install (version.go); an empty repo means release.DefaultChartRepo.
	// ChartRef is the one mapping from these fields to what the chart source
	// is asked for.
	ChartRepo    string
	ChartVersion string
	GrantPath    string
	ValuesPath   string
	// PathsInNode says the grant's storage paths live inside a test node rather
	// than on this host, so box cannot stat them. It exists for the kind e2e,
	// which writes the directories in the node's filesystem; on a real host the
	// check is what catches a grant pointing at a directory that is not there.
	PathsInNode bool
	// AfterConfirm asks for the answers that outlive no file, after the
	// operator said yes and before the first write. `box init` asks for the Plex
	// claim token here: it expires about four minutes after plex.tv/claim
	// issues it, so it can only reach the apply that installs Plex and must
	// never be written to either file. What it returns is merged into this
	// apply's values by dotted values path.
	AfterConfirm func(ctx context.Context) (map[string]any, error)
	// TransientKeys names the values paths AfterConfirm answers. The release
	// Helm stored last time still holds the previous answer, so the diff would
	// otherwise print a secret the second time box runs; these keys are dropped
	// from it. A transient answer is therefore never reviewable and never
	// changeable through apply, which is the point: it is not a setting.
	TransientKeys []string
	// BoxValues returns values box derives from the cluster rather than from
	// either file, overlaid after both and before the diff. It is how
	// networkPolicy.gatewayFrom follows the Gateway implementation actually in
	// use: a value that is box's, not the operator's, and that has to be
	// recomputed on every apply so a re-run is still a no-op.
	BoxValues func(ctx context.Context) (map[string]any, error)
}

// ChartRef is the reference the chart source is asked for. loadChart uses it,
// and cmd/box's flag tests pin the options it builds, so a field dropped
// between a flag and the source breaks a test rather than an e2e.
func (o Options) ChartRef() ChartRef {
	return ChartRef{Repo: o.ChartRepo, Version: o.ChartVersion}
}

// Result is what happened, for the caller to report or a test to assert.
type Result struct {
	FirstInstall bool
	// Host is the domain this release serves, when its values name one.
	Host    string
	Changes []seedbox.Change
	// Storage is what box found it had to create, as Plan returned it.
	Storage []StorageObject
	// NamespaceCreated says apply created the seedbox's namespace, which only
	// happens on a cluster where it is missing and this identity may create
	// one (root's).
	NamespaceCreated bool
	// StorageCreated is how many of those it wrote.
	StorageCreated int
	Applied        bool
	Revision       int
	Receipt        Receipt
}

// Applier is `box apply`'s whole decision.
type Applier struct {
	Charts ChartSource
	// Open returns the namespace-scoped release handle. The namespace comes
	// from the grant, so it can only be opened after the files are read.
	Open func(namespace string) (ReleaseClient, error)
	// OpenStorage returns the storage handle for that namespace. It is separate
	// from Open because the PVs are cluster-scoped and the claims are not part
	// of the release: Helm must never own them.
	OpenStorage func(namespace string) (StorageClient, error)
	Confirm     Confirmer
	// Say is every line apply writes. A nil reporter writes nothing, which is
	// what a caller with no output wants; cmd/box always gives the plain or the
	// live one.
	Say say.Reporter

	// ReceiptWait bounds how long box waits for the wiring hook's receipt
	// after the release is ready. Zero means ten minutes.
	ReceiptWait time.Duration
	// Poll is how often the receipt is re-read while waiting. Zero means two
	// seconds.
	Poll time.Duration
}

// report is the one place apply writes progress.
func (a *Applier) report() say.Reporter { return say.Or(a.Say, nil) }

// Apply reads the files, checks them, shows what would change and applies it.
func (a *Applier) Apply(ctx context.Context, opts Options) (Result, error) {
	var res Result
	r := a.report()
	r.Phase("apply")

	// The two files. A grant key in values, a key the chart does not know and
	// a missing namespace are all refused here, before box has a cluster
	// client at all.
	sb, err := seedbox.Load(opts.GrantPath, opts.ValuesPath)
	if err != nil {
		return res, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return res, err
	}
	// The grant's host paths. A path that is not there is refused here, before
	// the chart is even read: box does not create the directories (`box init`
	// does), and a release over an empty volume is worse than a refusal.
	vols, err := sb.Storage()
	if err != nil {
		return res, err
	}
	if !opts.PathsInNode {
		if err := checkHostPaths(vols); err != nil {
			return res, err
		}
	}
	chart, err := a.loadChart(ctx, opts)
	if err != nil {
		return res, err
	}
	// init prints the chart before its questions; a bare `box apply` has not
	// shown anyone which release it is about to install, and "newest this box
	// can use" is not obvious from the tag it ends up with.
	if opts.Chart == nil && chart.Version != "" {
		r.Chart(chart.Name, chart.Version, chart.Ref)
	}
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return res, err
	}
	// Values box derives from the cluster are applied last, exactly like the
	// claim names: neither file may set them, and nothing in either file can
	// shadow them. They are recomputed on every apply, so the stored release
	// and the computed document stay comparable and a re-run remains empty.
	if opts.BoxValues != nil {
		box, err := opts.BoxValues(ctx)
		if err != nil {
			return res, err
		}
		overlayTree(values, box)
		if err := seedbox.Validate(chart.Schema, values); err != nil {
			return res, err
		}
	}

	// The namespace is read before the release is: a claim cannot be written
	// into a namespace that is not there, and on a fresh cluster the namespace
	// is exactly what is missing. Both the read and the access question are
	// reads, so nothing is written before the confirm.
	store, err := a.OpenStorage(namespace)
	if err != nil {
		return res, err
	}
	nsp, err := store.PlanNamespace(ctx)
	if err != nil {
		return res, err
	}
	if !nsp.Exists {
		if !nsp.CanCreate {
			return res, fmt.Errorf("the namespace %s does not exist, and this kubeconfig may not create namespaces: root creates a seedbox's namespace with `box adduser`; ask root to add the box, then apply again", namespace)
		}
	} else if nsp.Enforce != podSecurityRestricted {
		enforce := "no " + podSecurityEnforce + " label"
		if nsp.Enforce != "" {
			enforce = podSecurityEnforce + "=" + nsp.Enforce
		}
		r.Warn(fmt.Sprintf("namespace %s does not enforce Pod Security %s (%s): root's labels win, box leaves it alone",
			namespace, podSecurityRestricted, enforce))
	}

	client, err := a.Open(namespace)
	if err != nil {
		return res, err
	}
	// A release lives in the namespace, so a missing namespace is a missing
	// release. Asking Helm about it would only report the archive back.
	var deployed map[string]any
	exists := false
	if nsp.Exists {
		deployed, exists, err = client.Deployed(ctx)
		if err != nil {
			return res, err
		}
	}
	res.FirstInstall = !exists
	res.Host = hostOf(values)
	if exists {
		res.Changes = seedbox.Diff(deployed, values)
	}
	// A transient answer is not a setting: the release Helm stored last time
	// holds the previous one, and showing it would print a secret. Dropping it
	// before the empty-diff check is also what makes a re-run a no-op.
	res.Changes = withoutTransient(res.Changes, opts.TransientKeys)

	// The storage is not a release object, so it is planned separately and
	// written before the release: pods cannot schedule without the claims, and
	// Helm must never own them (a `helm uninstall` must not delete the data).
	plan, err := store.Plan(ctx, namespace, vols)
	if err != nil {
		return res, err
	}
	res.Storage = plan

	if exists && len(res.Changes) == 0 && len(plan) == 0 && nsp.Exists {
		r.Line(fmt.Sprintf("%s in %s: nothing to change", ReleaseName, namespace))
		return res, nil
	}

	if exists {
		r.Line(fmt.Sprintf("%s in %s: %d %s", ReleaseName, namespace, len(res.Changes), plural(len(res.Changes), "change", "changes")))
		r.Diff(changeItems(res.Changes))
	} else {
		r.Line(fmt.Sprintf("%s in %s: first install", ReleaseName, namespace))
		var details []string
		if host := hostOf(values); host != "" {
			details = append(details, "host: "+host)
		}
		if apps := enabledApps(values); len(apps) > 0 {
			details = append(details, "apps: "+strings.Join(apps, ", "))
		}
		if len(details) > 0 {
			r.List("", details)
		}
	}
	a.printPlan(r, nsp, plan)

	ok, err := a.Confirm.Ask(fmt.Sprintf("apply %s in %s?", ReleaseName, namespace))
	if err != nil {
		return res, err
	}
	if !ok {
		r.Line("oh heck, nvm")
		return res, ErrCancelled
	}

	// Answers that outlive no file are asked last, and only now: the diff above
	// is the review, and a claim token that expires in minutes would be expired
	// by the time a slow reader finished it.
	if opts.AfterConfirm != nil {
		extra, err := opts.AfterConfirm(ctx)
		if err != nil {
			return res, err
		}
		if err := mergeTransient(values, extra); err != nil {
			return res, err
		}
	}

	// Everything box can check has been checked: the namespace is there or may
	// be created, and every volume and claim is either what box would write or
	// a refusal. A write can still fail, and a volume is cluster-scoped, so
	// what this run creates is remembered and removed again if the run stops
	// before the release exists.
	var created []StorageObject

	// The namespace first: it is what the claims and the release live in. It is
	// created only when it was missing at plan time, so an existing namespace
	// -- root's or an admin's, with whatever labels they gave it -- is never
	// applied over.
	if !nsp.Exists {
		if err := store.CreateNamespace(ctx); err != nil {
			return res, err
		}
		res.NamespaceCreated = true
		r.Success(fmt.Sprintf("created namespace %s (Pod Security %s)", namespace, podSecurityRestricted))
	}

	if len(plan) > 0 {
		step := r.Start(say.Op{Label: fmt.Sprintf("writing the storage for %s", namespace)})
		for _, obj := range plan {
			if err := store.Apply(ctx, obj); err != nil {
				step.Failed(err)
				return res, a.undo(ctx, store, created, err)
			}
			res.StorageCreated++
			if obj.Kind == KindPersistentVolume {
				created = append(created, obj)
			}
		}
		step.DoneAs("", fmt.Sprintf("wrote %d storage object%s", len(plan), plural(len(plan), "", "s")))
	}

	started := time.Now()
	upgrade := r.Start(say.Op{Label: fmt.Sprintf("installing %s in %s", ReleaseName, namespace)})
	rev, err := client.Upgrade(ctx, chart, values)
	if err != nil {
		upgrade.Failed(err)
		return res, a.undo(ctx, store, created, err)
	}
	res.Applied = true
	res.Revision = rev
	upgrade.Done(fmt.Sprintf("applied: %s revision %d in %s", ReleaseName, rev, namespace))

	receipt, err := a.waitReceipt(ctx, client, started)
	if err != nil {
		return res, err
	}
	res.Receipt = receipt
	a.printReceipt(r, receipt)
	if receipt.Status != "ok" {
		if receipt.Error != "" {
			return res, fmt.Errorf("wiring failed: %s", receipt.Error)
		}
		return res, errors.New("wiring failed: the receipt does not say ok")
	}
	return res, nil
}

// loadChart returns the chart this apply installs: the one the caller already
// read, or one loaded from the registry the options name.
//
// This is the first thing Apply does after the files are read, and it is where
// a chart box is too old for is refused (minbox.go), so no refusal here can
// have touched a cluster.
func (a *Applier) loadChart(ctx context.Context, opts Options) (Chart, error) {
	if opts.Chart != nil {
		return *opts.Chart, nil
	}
	if a.Charts == nil {
		return Chart{}, errors.New("no chart source: box reads its chart from the registry")
	}
	return a.Charts.Load(ctx, opts.ChartRef())
}

// waitReceipt waits for a receipt from this run. A receipt left by an earlier
// revision is not evidence about this one, so its start time must be after the
// upgrade began (with a little slack for clock granularity).
func (a *Applier) waitReceipt(ctx context.Context, client ReleaseClient, since time.Time) (Receipt, error) {
	wait := a.ReceiptWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	poll := a.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	fresh := since.Add(-2 * time.Second)
	r := a.report()
	task := r.Start(say.Op{Label: "waiting for the wiring receipt", Budget: wait})

	for {
		rec, err := client.Receipt(ctx)
		switch {
		case err == nil && !rec.StartedAt.Before(fresh):
			task.Done("")
			return rec, nil
		case err != nil && !errors.Is(err, ErrNoReceipt):
			task.Failed(err)
			return Receipt{}, err
		}
		if time.Now().After(deadline) {
			task.Failed(nil)
			return Receipt{}, errors.New("the wiring receipt never arrived: the release is installed but its wiring hook has not reported")
		}
		select {
		case <-ctx.Done():
			task.Failed(ctx.Err())
			return Receipt{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// undo removes the volumes this run created, after a failure that stopped it
// before the release existed. A PV is cluster-scoped and reclaim Retain, so one
// left behind is a volume nobody's release owns; it would make the next apply
// against a different path refuse over it. Only what this run wrote is removed:
// a volume that was already there is never touched, and neither are the claims,
// which are namespaced, harmless, and what the next apply binds the recreated
// volume to. The namespace stays too: it is empty and it is what the box wants.
func (a *Applier) undo(ctx context.Context, store StorageClient, created []StorageObject, cause error) error {
	if len(created) == 0 {
		return cause
	}
	errs := []error{cause}
	for _, obj := range created {
		if err := store.Delete(ctx, obj); err != nil {
			errs = append(errs, fmt.Errorf("%s %s was created by this run and is still there; delete it with kubectl: %w",
				obj.Kind, obj.Name, err))
			continue
		}
		a.report().Line(fmt.Sprintf("removed %s %s: this run created it and did not finish", obj.Kind, obj.Name))
	}
	return errors.Join(errs...)
}

// printPlan is what box will create before the release, in the order it will
// create it: the namespace, when it is missing, then the claims. On a first
// install these lines are the only trace of the storage, because it is not
// chart objects and Helm never sees it.
func (a *Applier) printPlan(r say.Reporter, nsp NamespacePlan, objs []StorageObject) {
	if nsp.Exists && len(objs) == 0 {
		return
	}
	var items []say.Item
	if !nsp.Exists {
		items = append(items, say.Item{Mark: say.Install, Text: fmt.Sprintf("Namespace %s (Pod Security %s)", nsp.Name, podSecurityRestricted)})
	}
	for _, o := range objs {
		items = append(items, say.Item{Mark: say.Install, Text: fmt.Sprintf("%s %s (%s)", o.Kind, o.Name, o.Detail)})
	}
	r.Plan("plan", items)
}

func (a *Applier) printReceipt(r say.Reporter, rec Receipt) {
	rows := make([]say.ReceiptRow, 0, len(rec.Steps))
	for _, s := range rec.Steps {
		rows = append(rows, say.ReceiptRow{Name: s.Name, Result: s.Result, Detail: s.Detail})
	}
	r.Receipt(rec.Status, rows)
}

// changeItems turns a values diff into the marked plan the reporter draws. The
// text is FormatChanges' own, so the plain transcript is unchanged.
func changeItems(changes []seedbox.Change) []say.Item {
	items := make([]say.Item, 0, len(changes))
	for _, c := range changes {
		mark := say.Change
		switch c.Kind {
		case seedbox.Added:
			mark = say.Install
		case seedbox.Removed:
			mark = say.Remove
		}
		for _, line := range strings.Split(seedbox.FormatChanges([]seedbox.Change{c}), "\n") {
			if line == "" {
				continue
			}
			// FormatChanges already carries the mark; the plan's own mark is
			// what a live renderer colours, so the text drops the glyph.
			if i := strings.IndexByte(line, ' '); i >= 0 {
				line = line[i+1:]
			}
			items = append(items, say.Item{Mark: mark, Text: line})
		}
	}
	return items
}

func hostOf(values map[string]any) string {
	gw, _ := values["gateway"].(map[string]any)
	host, _ := gw["host"].(string)
	return host
}

func enabledApps(values map[string]any) []string {
	apps, _ := values["apps"].(map[string]any)
	out := make([]string, 0, len(apps))
	for name, v := range apps {
		entry, _ := v.(map[string]any)
		if on, _ := entry["enabled"].(bool); on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// overlayTree applies box's cluster-derived values on top of the effective
// document. Mappings merge, so networkPolicy's other keys survive; a list or a
// scalar replaces, because a NetworkPolicy peer list is one decision.
func overlayTree(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				overlayTree(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
