// Package release is `box apply`: it turns one seedbox's two files into the
// release that runs it.
//
// The flow is linear, and every refusal happens before the first cluster write:
// load the grant and the values file, merge them, check the result against the
// chart release's values.schema.json, read what is deployed, show the
// difference, ask, then install or upgrade and read the wiring receipt back.
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
}

// Result is what happened, for the caller to report or a test to assert.
type Result struct {
	FirstInstall bool
	Changes      []seedbox.Change
	Applied      bool
	Revision     int
	Receipt      Receipt
}

// Applier is `box apply`'s whole decision.
type Applier struct {
	Charts ChartSource
	// Open returns the namespace-scoped release handle. The namespace comes
	// from the grant, so it can only be opened after the files are read.
	Open    func(namespace string) (ReleaseClient, error)
	Confirm Confirmer
	Log     io.Writer

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
	namespace, err := namespaceOf(sb)
	if err != nil {
		return res, err
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

	client, err := a.Open(namespace)
	if err != nil {
		return res, err
	}
	deployed, exists, err := client.Deployed(ctx)
	if err != nil {
		return res, err
	}
	res.FirstInstall = !exists
	if exists {
		res.Changes = seedbox.Diff(deployed, values)
	}

	if exists && len(res.Changes) == 0 {
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

	ok, err := a.Confirm.Ask(fmt.Sprintf("apply %s in %s?", ReleaseName, namespace))
	if err != nil {
		return res, err
	}
	if !ok {
		a.logf("oh heck, nvm\n")
		return res, ErrCancelled
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

// namespaceOf is the grant's `namespace` key, which is box's own record and
// never a chart value. It names the release namespace, so it is read before
// anything is sent to Helm.
func namespaceOf(sb *seedbox.Seedbox) (string, error) {
	ns, _ := sb.Grant.Map()["namespace"].(string)
	if strings.TrimSpace(ns) == "" {
		return "", fmt.Errorf("%s: namespace is missing or is not a name; the grant is what names the release namespace", sb.Grant.Path)
	}
	return ns, nil
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
