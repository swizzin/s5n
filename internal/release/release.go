// Package release is `box apply`: it turns one seedbox's two files into the
// release that runs it.
//
// The flow is linear, and every refusal happens before the first cluster write:
// load the grant and the values file, merge them, check the result against the
// chart release's values.schema.json, read the namespace, the storage and what
// is deployed, show the difference, ask, then create the namespace if it is
// missing, write the storage, install or upgrade, and read the wiring receipt
// back.
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
	"io"
	"sort"
	"strings"
	"time"

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

// Chart is the chart release box is about to install. Dir is where the Helm
// loader reads it from; Values and Schema are the two files box checks a
// seedbox against, so one chart is read once and used for both.
type Chart struct {
	Dir    string
	Values []byte
	Schema []byte
}

// ChartSource loads a chart from a directory. Pulling a published chart from
// the registry is a later step; this phase reads a local checkout.
type ChartSource interface {
	Load(ctx context.Context, dir string) (Chart, error)
}

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
	ChartDir   string
	GrantPath  string
	ValuesPath string
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
}

// Result is what happened, for the caller to report or a test to assert.
type Result struct {
	FirstInstall bool
	Changes      []seedbox.Change
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
	Log         io.Writer

	// ReceiptWait bounds how long box waits for the wiring hook's receipt
	// after the release is ready. Zero means ten minutes.
	ReceiptWait time.Duration
	// Poll is how often the receipt is re-read while waiting. Zero means two
	// seconds.
	Poll time.Duration
}

func (a *Applier) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	fmt.Fprintf(a.Log, format, args...)
}

// Apply reads the files, checks them, shows what would change and applies it.
func (a *Applier) Apply(ctx context.Context, opts Options) (Result, error) {
	var res Result
	if opts.ChartDir == "" {
		return res, errors.New("no chart: pass --chart <dir>; box cannot pull a published chart yet")
	}

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
	chart, err := a.Charts.Load(ctx, opts.ChartDir)
	if err != nil {
		return res, err
	}
	if chart.Dir == "" {
		chart.Dir = opts.ChartDir
	}
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return res, err
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
		a.logf("namespace %s does not enforce Pod Security %s (%s): root's labels win, box leaves it alone\n",
			namespace, podSecurityRestricted, enforce)
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
		a.logf("%s in %s: nothing to change\n", ReleaseName, namespace)
		return res, nil
	}

	if exists {
		a.logf("%s in %s: %d %s\n", ReleaseName, namespace, len(res.Changes), plural(len(res.Changes), "change", "changes"))
		a.logf("%s\n", seedbox.FormatChanges(res.Changes))
	} else {
		a.logf("%s in %s: first install\n", ReleaseName, namespace)
		if host := hostOf(values); host != "" {
			a.logf("  host: %s\n", host)
		}
		if apps := enabledApps(values); len(apps) > 0 {
			a.logf("  apps: %s\n", strings.Join(apps, ", "))
		}
	}
	a.printPlan(nsp, plan)

	ok, err := a.Confirm.Ask(fmt.Sprintf("apply %s in %s?", ReleaseName, namespace))
	if err != nil {
		return res, err
	}
	if !ok {
		a.logf("oh heck, nvm\n")
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

	// The namespace first: it is what the claims and the release live in. It is
	// created only when it was missing at plan time, so an existing namespace
	// -- root's or an admin's, with whatever labels they gave it -- is never
	// applied over.
	if !nsp.Exists {
		if err := store.CreateNamespace(ctx); err != nil {
			return res, err
		}
		res.NamespaceCreated = true
		a.logf("created namespace %s (Pod Security %s)\n", namespace, podSecurityRestricted)
	}

	for _, obj := range plan {
		if err := store.Apply(ctx, obj); err != nil {
			return res, err
		}
		res.StorageCreated++
	}

	started := time.Now()
	rev, err := client.Upgrade(ctx, chart, values)
	if err != nil {
		return res, err
	}
	res.Applied = true
	res.Revision = rev
	a.logf("applied: %s revision %d in %s\n", ReleaseName, rev, namespace)

	receipt, err := a.waitReceipt(ctx, client, started)
	if err != nil {
		return res, err
	}
	res.Receipt = receipt
	a.printReceipt(receipt)
	if receipt.Status != "ok" {
		if receipt.Error != "" {
			return res, fmt.Errorf("wiring failed: %s", receipt.Error)
		}
		return res, errors.New("wiring failed: the receipt does not say ok")
	}
	return res, nil
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

	for {
		rec, err := client.Receipt(ctx)
		switch {
		case err == nil && !rec.StartedAt.Before(fresh):
			return rec, nil
		case err != nil && !errors.Is(err, ErrNoReceipt):
			return Receipt{}, err
		}
		if time.Now().After(deadline) {
			return Receipt{}, errors.New("the wiring receipt never arrived: the release is installed but its wiring hook has not reported")
		}
		select {
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// printPlan is what box will create before the release, in the order it will
// create it: the namespace, when it is missing, then the claims. On a first
// install these lines are the only trace of the storage, because it is not
// chart objects and Helm never sees it.
func (a *Applier) printPlan(nsp NamespacePlan, objs []StorageObject) {
	if nsp.Exists && len(objs) == 0 {
		return
	}
	a.logf("plan:\n")
	if !nsp.Exists {
		a.logf("  + Namespace %s (Pod Security %s)\n", nsp.Name, podSecurityRestricted)
	}
	for _, o := range objs {
		a.logf("  + %s %s (%s)\n", o.Kind, o.Name, o.Detail)
	}
}

func (a *Applier) printReceipt(rec Receipt) {
	a.logf("wiring %s\n", rec.Status)
	for _, s := range rec.Steps {
		if s.Detail != "" {
			a.logf("  %s: %s (%s)\n", s.Name, s.Result, s.Detail)
			continue
		}
		a.logf("  %s: %s\n", s.Name, s.Result)
	}
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
