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

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/wait"
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

// HostPlan is what apply tells its per-host TLS step. Domain is the box's own
// host name, the answer every app host has to resolve to; Hosts are the names
// this run would serve at a host of their own and that have no listener yet;
// Forced is tls.issuer when the files force one, so the certificate an apply
// writes names the same issuer init would have chosen.
type HostPlan struct {
	Domain string
	Forced string
	// Issuers is the issuer this run decided for each name it serves, by
	// host: init's own answer (setup.Result.Hosts), so the certificate check
	// compares a host with what this run will write. Forced alone is not
	// enough: it is empty on the run whose issuer comes from DNS, which is
	// exactly the run whose decision can differ from the Certificate that is
	// already there.
	Issuers map[string]string
	Hosts   []string
	// All is every name this run decided, for the runs that own the whole
	// box: `box init` sets it, and the step writes each name idempotently,
	// because re-issuing a self-signed host under the box root is work an
	// already-answering host still has. Hosts stays the plan's list -- the
	// names nothing answers on -- so a run with nothing new still says so.
	All []string
	// Rule is the login's half of the same step: a host is not served without
	// its access_control rule, and the rule is rendered from the same inputs
	// init renders it from. It is filled in for Ensure only; Missing is a read
	// of the cluster and needs none of it.
	Rule HostRule
}

// HostRule is one rule write: who logs in, the document the access_control
// rules are derived from, and every app the chart serves at a host of its own --
// the whole list, because the login's configuration is rewritten whole and a
// name left out would lose the rule it already had.
type HostRule struct {
	Login  string
	Hosted []string
	Values map[string]any
	// Namespace is the seedbox's namespace -- half of every OIDC client id --
	// and OIDC are the apps the catalog says sign in through the login. The
	// login's configuration is one file: a rule write renders the clients too,
	// so both halves of the step need the same inputs init had.
	Namespace string
	OIDC      []string
	// Image is the login Deployment's pinned image: the rule write applies the
	// login's objects from the template init uses, and the image is part of it.
	Image string
}

// Hosts is root's per-host TLS step: the listener and Certificate an app
// served at a name of its own needs (docs/box.md, "No run leaves an app host
// without its listener").
//
// Missing is the read apply makes with its diff, so a host that is missing is
// work this run shows and does even when the release itself does not change.
// Ensure is the write, after the confirm and before the release moves: the DNS
// check, the access question, then one listener and Certificate per name. A
// stop in either leaves the release and the old routes untouched, which is why
// the step is an interface with two halves rather than one call at the end.
type Hosts interface {
	Missing(ctx context.Context, plan HostPlan) ([]string, error)
	Ensure(ctx context.Context, plan HostPlan) error
}

// StaleHosts is the optional half a step that owns the whole box implements:
// the hosts that answer already but whose Certificate is not the one a run
// would issue -- an issuer that changed under it, or a self-signed certificate
// off the box root. `box init` asks; a step that does not implement this is
// asked only what is missing.
type StaleHosts interface {
	Stale(ctx context.Context, plan HostPlan) ([]string, error)
}

// union is a's names followed by b's, without repeats, in the order they were
// first named: the plan's host list is a set, and printing one twice would read
// as two writes.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Login is root's login, reconciled on its own by every apply that has one.
//
// The login's configuration carries the `theme:` the values file names -- one
// page for everyone on the host, in the box's palette -- and the pictures the
// page is drawn with beside it, so a run whose only change is a theme has to
// re-render it or the login keeps the old face. That is not per host and must
// not become one: none of it checks DNS, asks for a Certificate or writes a
// listener, which is why it is a second interface rather than a third method on
// Hosts.
//
// Pending is the read apply makes with its diff, so a login that differs is
// work this run shows and does even when the release and every host are as they
// were. Reconcile is the write, after the confirm and before the release moves,
// and writes nothing when the cluster already holds the rendering.
//
// A user's run has none: the login is root's, and a `theme:` in a user's values
// file is the palette that user's own box draws with, not the page everyone
// signs in on.
type Login interface {
	Pending(ctx context.Context, plan HostPlan) (bool, error)
	Reconcile(ctx context.Context, plan HostPlan) error
}

// AnchorPlan is what apply tells its host-CA step: the box's own host name, the
// issuer this run writes that host's Certificate with (empty when this run
// writes none), and the namespace the anchor is written into.
type AnchorPlan struct {
	Domain string
	// Issuer is the ClusterIssuer this run writes the box's host Certificate
	// with. On a first init it is the only thing that can be: init decides the
	// issuer and hands over to apply, and its per-host step writes the
	// Certificate afterwards, so there is nothing in the cluster to read yet.
	// Empty means this run writes no Certificate, and the anchor follows the
	// one the cluster already has.
	Issuer    string
	Namespace string
	// OIDC are the apps that sign in through the login, from the chart's
	// catalog. Their client Secrets live in this namespace too, and the step
	// that runs here is the first point in an install where it exists: a first
	// install's namespace is created by apply, after init has already written
	// the login.
	OIDC []string
}

// Anchor is root's host-CA step: the ConfigMap in the seedbox's namespace that
// carries the box's self-signed root, so an app that signs in through the
// login's OIDC provider can verify the certificate the box serves
// (docs/box.md, "Certificates"). A Let's Encrypt box needs nothing carried --
// its root is in every image -- and the step removes what an earlier
// self-signed run left.
//
// It follows the Certificate the box's host is issued under, not the name's
// DNS answer: a record that appears does not move the certificate, and an
// anchor that moved before it would take the login away from every app that
// restarted in between.
//
// Plan is the read apply makes with its diff. Ensure is the write, after the
// confirm and before the release moves -- an app that has to trust the login
// needs the anchor when it starts, not after it has crash-looped through
// Helm's wait. Only `box init` holds one: it is the run that writes the box's
// own Certificate, so it is the run that knows what the anchor should be.
type Anchor interface {
	Plan(ctx context.Context, plan AnchorPlan) (string, error)
	Ensure(ctx context.Context, plan AnchorPlan) error
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
	// HostIssuers is the issuer each host was decided with when the caller
	// made that decision already (`box init`): host name to tls.issuer
	// value. The plan compares each Certificate that answers with its own
	// host's decision, so a name init just decided to move reaches the step
	// even though tls.issuer is empty. Nil leaves the step to decide from
	// tls.issuer or each name's DNS answer.
	HostIssuers map[string]string
	// HostsAll says the step is handed every host this run decides, not only
	// the missing ones. `box init` sets it: a host whose listener and rule are
	// already there still has a Certificate to check, and init is the run that
	// re-issues a self-signed host under the box root once the name resolves
	// (ca-issuer's followRoot) and moves it to the production issuer. A bare
	// `box apply` leaves it false: a host nothing answers on is its work, and
	// one that answers is not.
	HostsAll bool
	// Hosts is root's work outside the release: the per-host TLS step, and --
	// when the value also implements Login -- the login those hosts and the
	// portal sign in through. Both run after the confirm and before the
	// release moves, on every run that serves a host: `box init` and a bare
	// `box apply` alike. Nothing else writes them, and a name reached only
	// after the release is one Helm's own wait can hang on -- an app that will
	// not start until it can dial the issuer the host does not answer on yet.
	Hosts Hosts
	// Anchor is root's host-CA step: the ConfigMap a self-signed box carries
	// into its namespace, and the removal of it once the host is publicly
	// trusted. Only `box init` sets it: the anchor follows the Certificate the
	// box's host is actually served with, and init is the run that writes that
	// Certificate. A `box apply` does no anchor work at all -- it cannot move a
	// host from one issuer to another, so it has nothing to follow -- and a
	// user's apply must not read cert-manager's namespace, let alone write this
	// ConfigMap.
	Anchor Anchor
	// AnchorIssuer is the ClusterIssuer this run writes the box's own host
	// Certificate with, and so what its host-CA step follows. init sets it to
	// the issuer its per-host step will write; empty leaves the step reading
	// the Certificate in the cluster.
	AnchorIssuer string
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
	// Hosts are the app hosts this run gave a listener and a Certificate,
	// because they had neither and the values serve them.
	Hosts []string
	// Login says the login's configuration or pictures differed from the
	// cluster's, so this run re-rendered them. It is not a revision: a theme
	// the release already holds leaves the release alone.
	Login bool
	// Storage is what box found it had to create, as Plan returned it.
	Storage []StorageObject
	// AnchorWrote says this run wrote or removed the host CA ConfigMap.
	AnchorWrote bool
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

	// Root's per-host TLS, read here with the diff: an app served at a host of
	// its own that has no listener yet is a route on a name nothing answers on,
	// so the step is part of this run's work even when the release itself does
	// not change (docs/box.md, "No run leaves an app host without its
	// listener"). The read is the cluster's, not a record box kept.
	//
	// The login is the same value as that step when the step also implements
	// Login, which root's does: both are root's writes outside the release, and
	// the login is one page for the whole host rather than a name on it. A value
	// that does not -- `box init`'s nil, or a step for a user who cannot write
	// the login at all -- has no login half.
	login, _ := opts.Hosts.(Login)
	var hostPlan HostPlan
	if opts.Hosts != nil || login != nil {
		hostPlan, err = a.hostPlan(ctx, opts, chart, sb, values)
		if err != nil {
			return res, err
		}
		res.Hosts = hostPlan.Hosts
	}
	// The login, read here with the diff for the same reason: the theme the
	// values file names is the page everyone signs in on, so a run whose only
	// work is a re-rendered login is a run with something to do -- and one whose
	// login is already right is not.
	if login != nil {
		res.Login, err = login.Pending(ctx, hostPlan)
		if err != nil {
			return res, err
		}
	}

	// Root's host CA, read with the diff for the same reason: an anchor that is
	// missing is a real write this run must make, and a run that only had that
	// to do must not be reported as "nothing to change".
	var anchorLine string
	anchorPlan := AnchorPlan{Domain: hostOf(values), Issuer: opts.AnchorIssuer, Namespace: namespace}
	if opts.Anchor != nil && anchorPlan.Domain != "" {
		// The clients are the catalog's answer, not the login rule's: the
		// anchor step writes their Secrets on every run that has an anchor,
		// including one whose login-rule inputs are never built (a value that
		// is not a Login). A client Secret cannot wait for a host to exist.
		anchorPlan.OIDC, err = oidcClients(chart, values)
		if err != nil {
			return res, err
		}
		anchorLine, err = opts.Anchor.Plan(ctx, anchorPlan)
		if err != nil {
			return res, err
		}
	}

	if exists && len(res.Changes) == 0 && len(plan) == 0 && nsp.Exists && len(hostPlan.Hosts) == 0 && !res.Login && anchorLine == "" {
		r.Line(fmt.Sprintf("%s in %s: nothing to change", ReleaseName, namespace))
		return res, nil
	}

	if exists {
		if len(res.Changes) > 0 {
			r.Line(fmt.Sprintf("%s in %s: %d %s", ReleaseName, namespace, len(res.Changes), plural(len(res.Changes), "change", "changes")))
			r.Diff(changeItems(res.Changes))
		} else {
			// The only work this run has is outside the release -- a host
			// without its listener, or a login page that is not the one the
			// values name -- which is why the early return above did not take
			// it.
			r.Line(fmt.Sprintf("%s in %s: the release is unchanged", ReleaseName, namespace))
		}
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
	if len(hostPlan.Hosts) > 0 {
		lines := make([]string, 0, len(hostPlan.Hosts))
		for _, host := range hostPlan.Hosts {
			lines = append(lines, "Certificate, listener and login rule for "+host)
		}
		r.List("app hosts", lines)
	}
	if res.Login {
		r.List("login", []string{"the sign-in page's configuration, mark and favicon"})
	}
	if anchorLine != "" {
		r.List("host CA", []string{anchorLine})
	}

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

	// Root's per-host TLS goes before the release moves, and after the confirm
	// so a "no" writes nothing: a route on a host with no listener is an app
	// nobody can reach, and a stop here leaves the release and the old routes
	// exactly as they were. `box update` will call this same step.
	if len(hostPlan.Hosts) > 0 || len(hostPlan.All) > 0 {
		if err := opts.Hosts.Ensure(ctx, hostPlan); err != nil {
			return res, err
		}
	}

	// The anchor is written at the first point where the namespace it lives in
	// is known to exist, and always before the release moves: an app that has
	// to verify the login's certificate reads the ConfigMap as its pod starts,
	// and a pod that starts without it crash-loops through Helm's wait. On a
	// first install that point is just after this run creates the namespace
	// (below); on every later run it is here.
	//
	// The same step writes the OIDC clients' Secrets, which also live in that
	// namespace -- so the login's configuration, which names those clients, is
	// written after it rather than before. A run whose anchor step had work to
	// do therefore reconciles the login even when the login itself read as
	// unchanged: its client list depends on what that step just wrote.
	writeAnchor := func() error {
		if anchorLine == "" {
			return nil
		}
		if err := opts.Anchor.Ensure(ctx, anchorPlan); err != nil {
			return err
		}
		res.AnchorWrote = true
		return nil
	}
	reconcileLogin := func() error {
		if login == nil || (!res.Login && anchorLine == "") {
			return nil
		}
		return login.Reconcile(ctx, hostPlan)
	}
	if nsp.Exists {
		if err := writeAnchor(); err != nil {
			return res, err
		}
		// The login goes after the per-host step and after the anchor, and on
		// its own: a theme is not a host, so nothing here DNS-checks a name,
		// asks for a Certificate or writes a listener -- and because the write
		// compares before it applies, the rule the step above just wrote is not
		// written a second time.
		if err := reconcileLogin(); err != nil {
			return res, err
		}
	}

	// Only the steps that are not the release had work: the release, its
	// namespace and its storage are all as they were, so this run makes no Helm
	// revision. A certificate nobody has to renew, or a login page the release
	// already names, must not churn the release users roll back to.
	if exists && len(res.Changes) == 0 && len(plan) == 0 && nsp.Exists {
		return res, nil
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
		// The anchor and the OIDC clients' Secrets are objects in that
		// namespace, so they can only be written now -- and the login's
		// configuration, which names those clients, after them.
		if err := writeAnchor(); err != nil {
			return res, err
		}
		if err := reconcileLogin(); err != nil {
			return res, err
		}
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

// hostPlan is the work this apply has outside the release: the per-host TLS a
// values file's hosted apps need, and the login's rule inputs, which both steps
// render from.
//
// Every name the values serve at a host of their own, less the ones the cluster
// already has a listener, a Certificate and a login rule for, is the per-host
// step's work. Which apps get a host is the catalog's (`exposure: host`), so
// this reads the chart release's manifest the same way init does. A chart that
// ships no catalog serves no app at a host of its own, and one whose values
// name no domain cannot name one.
func (a *Applier) hostPlan(ctx context.Context, opts Options, chart Chart, sb *seedbox.Seedbox, values map[string]any) (HostPlan, error) {
	plan := HostPlan{Domain: hostOf(values), Forced: forcedIssuer(values), Issuers: opts.HostIssuers}
	apps, err := hostedHosts(chart, values, plan.Domain)
	if err != nil {
		return plan, err
	}
	// The box's own name is a host this run serves, and on a first install it
	// is the one nothing answers on: without its listener and Certificate the
	// Gateway has no 443, and every app that dials https://<host>/auth waits on
	// a name that is not there. Its login rule is the domain's, which init
	// writes with the page, so it is not a hosted app and hostedApps drops it.
	hosts := apps
	if plan.Domain != "" {
		hosts = append([]string{plan.Domain}, apps...)
	}
	// The login's inputs are built whatever the hosts are: a rule write and a
	// login reconcile render one file, and the reconcile runs on a run with no
	// host to serve at all.
	if plan.Domain != "" && len(chart.Catalog) > 0 {
		rule, err := loginRule(sb, chart, values, apps, plan.Domain)
		if err != nil {
			return plan, err
		}
		plan.Rule = rule
	}
	if len(hosts) == 0 || opts.Hosts == nil {
		return plan, nil
	}
	missing, err := opts.Hosts.Missing(ctx, HostPlan{Domain: plan.Domain, Forced: plan.Forced, Issuers: plan.Issuers, Hosts: hosts})
	if err != nil {
		return plan, err
	}
	plan.Hosts = missing
	if opts.HostsAll {
		// Every name this run decided, missing or not: the step is idempotent,
		// and a host that already answers can still have a Certificate to
		// follow the box root or to move to another issuer. One whose
		// Certificate needs that is work, so it is shown with the missing ones
		// and written before the release; the rest are checked and left alone,
		// which is what keeps an idempotent run saying "nothing to change".
		if st, ok := opts.Hosts.(StaleHosts); ok {
			stale, err := st.Stale(ctx, HostPlan{Domain: plan.Domain, Forced: plan.Forced, Issuers: plan.Issuers, Hosts: hosts})
			if err != nil {
				return plan, err
			}
			plan.Hosts = union(plan.Hosts, stale)
		}
		plan.All = hosts
	}
	return plan, nil
}

// loginRule is the login's half of both steps: the grant's account, the
// document the release renders, the release's own login image, and every app
// the chart serves at a name of its own. init passes internal/login the same
// four, which is what makes the two commands write one file.
func loginRule(sb *seedbox.Seedbox, chart Chart, values map[string]any, hosts []string, domain string) (HostRule, error) {
	login, err := sb.Login()
	if err != nil {
		return HostRule{}, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return HostRule{}, err
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return HostRule{}, err
	}
	return HostRule{
		Login:     login,
		Hosted:    hostedApps(hosts, domain),
		Values:    values,
		Image:     chart.Pins.Authelia,
		Namespace: namespace,
		OIDC:      cat.OIDCApps(enabledApps(values)),
	}, nil
}

// oidcClients is the apps the chart's catalog marks as signing in through the
// login, in catalog order. It takes the same inputs the client render does and
// returns the same list, but not the rest of the rule: the anchor step needs
// only this half, and building the whole rule there would tie the clients to a
// host name that has nothing to do with them.
func oidcClients(chart Chart, values map[string]any) ([]string, error) {
	if len(chart.Catalog) == 0 {
		return nil, nil
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return nil, err
	}
	return cat.OIDCApps(enabledApps(values)), nil
}

// hostedHosts is every name the values serve at a host of its own, in catalog
// order: <app>.<domain> for each enabled app whose entry says `exposure: host`.
func hostedHosts(chart Chart, values map[string]any, domain string) ([]string, error) {
	if len(chart.Catalog) == 0 || domain == "" {
		return nil, nil
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return nil, err
	}
	apps, _ := values["apps"].(map[string]any)
	var hosts []string
	for _, name := range cat.Names() {
		app, ok := cat.App(name)
		if !ok || app.Exposure != catalog.ExposureHost {
			continue
		}
		entry, _ := apps[name].(map[string]any)
		if on, _ := entry["enabled"].(bool); !on {
			continue
		}
		hosts = append(hosts, name+"."+domain)
	}
	return hosts, nil
}

// hostedApps is the app names behind the hosts hostedHosts returns, which is
// what the login's rules are keyed by: an app served at <app>.<domain> is one
// app name, and Rules() expects those, not the full names.
func hostedApps(hosts []string, domain string) []string {
	suffix := "." + domain
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		app := strings.TrimSuffix(host, suffix)
		if app == host {
			continue
		}
		out = append(out, app)
	}
	return out
}

// forcedIssuer is tls.issuer as the files left it in the effective document:
// "" means each name's DNS answer decides.
func forcedIssuer(values map[string]any) string {
	tls, _ := values["tls"].(map[string]any)
	forced, _ := tls["issuer"].(string)
	return strings.TrimSpace(forced)
}

// waitReceipt waits for a receipt from this run. A receipt left by an earlier
// revision is not evidence about this one, so its start time must be after the
// upgrade began (with a little slack for clock granularity).
func (a *Applier) waitReceipt(ctx context.Context, client ReleaseClient, since time.Time) (Receipt, error) {
	budget := a.ReceiptWait
	if budget <= 0 {
		budget = 10 * time.Minute
	}
	poll := a.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	fresh := since.Add(-2 * time.Second)
	r := a.report()
	task := r.Start(say.Op{Label: "waiting for the wiring receipt", Budget: budget})

	var receipt Receipt
	waited, err := wait.Until(ctx, budget, poll, func(ctx context.Context) (bool, error) {
		rec, err := client.Receipt(ctx)
		switch {
		case err == nil && !rec.StartedAt.Before(fresh):
			receipt = rec
			return true, nil
		case err != nil && !errors.Is(err, ErrNoReceipt):
			return false, err
		}
		// No receipt yet, or one from an earlier revision: still waiting.
		return false, nil
	})
	switch {
	case err != nil:
		task.Failed(err)
		return Receipt{}, err
	case waited.Done:
		task.Done("")
		return receipt, nil
	}
	task.Failed(nil)
	return Receipt{}, errors.New("the wiring receipt never arrived: the release is installed but its wiring hook has not reported")
}

// undoTimeout bounds the whole of undo: a few deletes on an answering API server
// take well under a second.
const undoTimeout = time.Minute

// undo removes the volumes this run created, after a failure that stopped it
// before the release existed. A PV is cluster-scoped and reclaim Retain, so one
// left behind is a volume nobody's release owns; it would make the next apply
// against a different path refuse over it. Only what this run wrote is removed:
// a volume that was already there is never touched, and neither are the claims,
// which are namespaced, harmless, and what the next apply binds the recreated
// volume to. The namespace stays too: it is empty and it is what the box wants.
//
// Undo is cleanup, so it does not inherit the cancellation that failed the step:
// a Ctrl-C during the storage writes or the upgrade is exactly when a volume
// this run wrote has to come back off, and a Delete sent on the cancelled
// context would fail and strand it. A second Ctrl-C still ends box, because the
// first one unregisters the signal handler. Detaching drops the deadline too, so
// undo carries its own: an API server that stopped answering must not hold the
// failed run open until someone presses Ctrl-C again.
func (a *Applier) undo(ctx context.Context, store StorageClient, created []StorageObject, cause error) error {
	if len(created) == 0 {
		return cause
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
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
