// Package release is `box apply`: it turns one seedbox's two files into the
// release that runs it. Every refusal happens before the first write; a volume
// this run wrote is removed if the run never reaches the release. The Helm SDK
// is the client (PLAN.md), so `helm` can read and roll back what box installs.
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
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/wait"
)

// ReleaseName is the one Helm release box owns; `s5n` is also what
// hack/e2e-kind.sh installs under, so the receipt is the same object.
const ReleaseName = "s5n"

// The app whose person-facing password the chart generates, and its Secret key
// (charts/s5n/templates/secret.yaml); box reads it once and never writes it.
const (
	jellyfinApp             = "jellyfin"
	jellyfinUserPasswordKey = "jellyfin-user-password"
)

// ErrCancelled says the operator answered no; nothing was written.
var ErrCancelled = errors.New("cancelled")

// ReleaseClient is box's handle on one release in one namespace. It is opened
// only after the files pass every check, so a refusal cannot write.
type ReleaseClient interface {
	// Deployed returns the installed values, false when none is deployed. A
	// release in another state (failed, pending) is an error, never overwritten.
	Deployed(ctx context.Context) (values map[string]any, exists bool, err error)

	// State returns the deployed chart release: values alone cannot tell a
	// digest bump, which ships the values a box already has.
	State(ctx context.Context) (ReleaseState, error)

	// Upgrade installs or upgrades, waits, and returns the new revision.
	Upgrade(ctx context.Context, chart Chart, values map[string]any) (revision int, err error)

	// Receipt reads the wiring receipt; ErrNoReceipt means not yet, not a fault.
	Receipt(ctx context.Context) (Receipt, error)
}

// ErrNoReceipt says the wiring ConfigMap is not there yet; the hook can lag
// the release by a moment.
var ErrNoReceipt = errors.New("no wiring receipt yet")

// Receipt is the part of `<release>-wiring` status.json box prints; the full
// document belongs to internal/wiring.
type Receipt struct {
	Status    string
	StartedAt time.Time
	Duration  time.Duration
	Error     string
	Steps     []Step
}

// Step is one row of the receipt.
type Step struct {
	Name   string
	Result string
	Detail string
}

// Confirmer asks the one question; def is a bare enter's answer, and these
// flows ask with no because every one changes a serving box.
type Confirmer interface {
	Ask(question string, def bool) (bool, error)
}

// HostPlan is what apply tells its per-host TLS step: the box's domain, the
// hosts with no listener yet, and any forced issuer.
type HostPlan struct {
	Domain string
	Forced string
	// Issuers is init's per-host decision, so the check compares a host with
	// what this run will write. Forced alone misses a DNS-chosen issuer.
	Issuers map[string]string
	Hosts   []string
	// All is every name this run decided; `box init` sets it so a host that
	// answers can still be re-issued. Hosts stays the names nothing answers on.
	All []string
	// Served is every name this run serves, always: what a removal of names a
	// previous run wrote is compared against.
	Served []string
	// Rule is the login's half: a host is not served without its access_control
	// rule, rendered from init's inputs. Ensure only.
	Rule HostRule
}

// HostRule is one rule write: who logs in, the values, and every hosted app --
// the whole list, because the login configuration is rewritten whole.
type HostRule struct {
	Login  string
	Hosted []string
	// OwnLogin are apps that sign users in themselves and bypass the login; the
	// whole list, so a rewrite never puts the login back in front of them.
	OwnLogin []string
	Values   map[string]any
	// Namespace is half of every OIDC client id; OIDC are the apps that sign in
	// through the login. Both halves need init's inputs.
	Namespace string
	OIDC      []string
	// Image is the login Deployment's pinned image.
	Image string
}

// Hosts is root's per-host TLS step (docs/box.md, "No run leaves an app host
// without its listener"). Missing is the read made with the diff; Ensure is
// the write after the confirm and before the release moves, so a stop leaves
// the release and old routes untouched.
type Hosts interface {
	Missing(ctx context.Context, plan HostPlan) ([]string, error)
	Ensure(ctx context.Context, plan HostPlan) error
}

// StaleHosts is the optional half for a step that owns the whole box: hosts
// whose Certificate is not the one this run would issue. `box init` asks.
type StaleHosts interface {
	Stale(ctx context.Context, plan HostPlan) ([]string, error)
}

// HostPruner is the optional half that removes what a previous run wrote and this one
// does not serve: a renamed or switched-off hosted app would otherwise keep its listener
// and Certificate for good. Only names box's own managers wrote are ever removed.
type HostPruner interface {
	Prune(ctx context.Context, plan HostPlan) error
}

// union is a's names then b's, deduplicated in first-named order.
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

// Login is root's login, reconciled on its own: the theme and pictures are one
// page for the host, so a theme-only run must re-render it. Pending is the read
// made with the diff; Reconcile is the write after the confirm, and writes
// nothing when the cluster already matches. A user's run has none.
type Login interface {
	Pending(ctx context.Context, plan HostPlan) (bool, error)
	Reconcile(ctx context.Context, plan HostPlan) error
}

// LoginAccess is the optional question asked when an enabled app signs in
// through the login. The chart mounts the client Secret whether or not it
// exists, so a missing one is a pod that never runs; only the login's writer
// can register it. Both methods are reads, before the confirm.
type LoginAccess interface {
	MayWrite(ctx context.Context) (bool, error)
	ClientRegistered(ctx context.Context, namespace, app string) (bool, error)
}

// AnchorPlan is what apply tells its host-CA step.
type AnchorPlan struct {
	Domain string
	// Issuer is the ClusterIssuer this run writes the host Certificate with. On
	// a first init there is nothing to read yet; empty means follow the existing
	// Certificate.
	Issuer    string
	Namespace string
	// OIDC apps' client Secrets live here too; this is the first point a first
	// install's namespace exists.
	OIDC []string
}

// Anchor is root's host-CA step: the ConfigMap carrying the self-signed root
// an OIDC app trusts (docs/box.md, "Certificates"). It follows the host's
// Certificate, not DNS: moving early takes the login away from apps in between.
// Plan is the read; Ensure writes before the release moves. Only `box init`
// holds one.
type Anchor interface {
	Plan(ctx context.Context, plan AnchorPlan) (string, error)
	Ensure(ctx context.Context, plan AnchorPlan) error
}

// Snapshotter copies an app's config before this apply moves its image; without
// a copy `box rollback` has nothing to put back. Pending refuses before the
// question; Take runs immediately before the release; Prune's failure is a
// warning, not a failed apply.
type Snapshotter interface {
	Pending(ctx context.Context, plan SnapshotPlan) (SnapshotWork, error)
	Take(ctx context.Context, plan SnapshotPlan, work SnapshotWork) (SnapshotDirs, error)
	Prune(ctx context.Context, plan SnapshotPlan, work SnapshotWork) error
}

// SnapshotPlan is what apply knows when it asks its snapshot step to read.
type SnapshotPlan struct {
	// Namespace is the release's, from the grant.
	Namespace string
	// Chart's variants are the new images; its catalog names state to copy.
	Chart Chart
	// Values are the final values: files merged, box's keys, then holds.
	Values map[string]any
	// ConfigRoot holds one directory per instance's /config.
	ConfigRoot string
}

// SnapshotMove is one app whose image this apply moves.
type SnapshotMove struct {
	App     string
	Ran     snapshot.ImageRef
	MovedTo snapshot.ImageRef
	// Copied says the catalog names files and the bytes can be named, so the
	// app is stopped and copied. Otherwise it moves uncopied, and is never
	// rolled back: box has no state to put back.
	Copied bool
	// Uncopied is why a copyable move is uncopied: its bytes cannot be named.
	// When instances differ, Ran is the first's and the row does not print it.
	Uncopied UncopiedReason
	// Detail is the shared tag, or the pairs of differing instances.
	Detail string
}

// UncopiedReason is why a copyable move gets no copy.
type UncopiedReason int

const (
	// UncopiedNone is a move that is copied, or one with nothing to copy.
	UncopiedNone UncopiedReason = iota
	// UncopiedNoDigest: a tag-only reference with no pod digest to name it.
	UncopiedNoDigest
	// UncopiedDifferentBytes: the tag covered more than one digest.
	UncopiedDifferentBytes
	// UncopiedDifferentImages: instances ask for different images.
	UncopiedDifferentImages
)

// Row is the plan's line for one move. Only a copied app is said to be
// stopped; an uncopied one says why, so the operator decides.
func (m SnapshotMove) Row() string {
	switch m.Uncopied {
	case UncopiedNoDigest:
		return fmt.Sprintf("%s: moves from %s to %s without a snapshot (no pod reports the bytes it runs under tag %s)",
			m.App, m.Ran.Label(), m.MovedTo.Label(), m.Detail)
	case UncopiedDifferentBytes:
		return fmt.Sprintf("%s: moves from %s to %s without a snapshot (its instances run different bytes under tag %s)",
			m.App, m.Ran.Label(), m.MovedTo.Label(), m.Detail)
	case UncopiedDifferentImages:
		return fmt.Sprintf("%s: moves to %s without a snapshot (its instances run different images: %s)",
			m.App, m.MovedTo.Label(), m.Detail)
	}
	if m.Copied {
		return fmt.Sprintf("%s: stopped and snapshotted before it moves from %s to %s",
			m.App, m.Ran.Label(), m.MovedTo.Label())
	}
	return fmt.Sprintf("%s: moves from %s to %s without a snapshot (nothing to copy)",
		m.App, m.Ran.Label(), m.MovedTo.Label())
}

// SnapshotWork is every app this apply moves; all share one copy.
type SnapshotWork struct {
	// From and To name the copy; both are the installed version for an apply.
	From string
	To   string
	// Moves is empty for a run that moves no image.
	Moves []SnapshotMove
}

// Copied is the apps this apply stops and copies, in plan order.
func (w SnapshotWork) Copied() []string {
	var out []string
	for _, m := range w.Moves {
		if m.Copied {
			out = append(out, m.App)
		}
	}
	return out
}

// SnapshotDirs is where Take put each copied app's config.
type SnapshotDirs map[string]string

// Options are one `box apply`.
type Options struct {
	// Chart is a release the caller already loaded: init must hand over the
	// same release, not pull a second one.
	Chart *Chart
	// ChartRepo and ChartVersion name the release when Chart is nil; empty
	// version is the newest this box can install (version.go).
	ChartRepo    string
	ChartVersion string
	GrantPath    string
	ValuesPath   string
	// PathsInNode skips the host stat for the kind e2e, whose paths are inside
	// the node; on a real host the check catches a missing directory.
	PathsInNode bool
	// AfterConfirm asks for answers that outlive no file -- the Plex claim token,
	// which expires in minutes -- and merges them by dotted path. Never written.
	AfterConfirm func(ctx context.Context) (map[string]any, error)
	// TransientKeys names AfterConfirm's paths, dropped from the diff so a
	// stored secret is not printed; a transient answer is never a setting.
	TransientKeys []string
	// BoxValues overlays cluster-derived values after both files and before the
	// diff, recomputed every run so a re-run stays empty.
	BoxValues func(ctx context.Context) (map[string]any, error)
	// Holds overlays the values keeping a rolled-back app on its image, checked
	// against the schema; the seedbox is passed so no file is loaded twice. nil
	// keeps no holds.
	Holds func(ctx context.Context, sb *seedbox.Seedbox, chart Chart, values map[string]any) (map[string]any, error)
	// Snapshots copies apps before their images move; only `box apply` sets it.
	Snapshots Snapshotter
	// NetworkProof proves the cluster's CNI enforces NetworkPolicy before any app
	// exists. Nil means this cluster needs no proof, because box installed it or
	// one run already proved it.
	NetworkProof NetworkProof
	// HostIssuers is init's per-host issuer, so a host just moved reaches the
	// step even with tls.issuer empty. Nil decides from tls.issuer or DNS.
	HostIssuers map[string]string
	// HostsAll hands the step every host, not just missing ones: `box init` sets
	// it so a serving host's Certificate can be re-issued or moved.
	HostsAll bool
	// Hosts is root's work outside the release: per-host TLS and, if it also
	// implements Login, the login. Both run after the confirm and before the
	// release: Helm's wait would hang on an app dialing a host not served yet.
	Hosts Hosts
	// Anchor is root's host-CA step, set only by `box init`, which writes the
	// Certificate it follows. A user's apply must not read cert-manager's
	// namespace, let alone write this ConfigMap.
	Anchor Anchor
	// AnchorIssuer is what the host-CA step follows; empty reads the cluster's
	// Certificate.
	AnchorIssuer string
	// SkipHooks installs without the chart's hooks; `box update` does, because
	// the wiring Job waits for every app and a never-starting image cannot.
	// The caller applies again with hooks once the right images stay.
	SkipHooks bool
	// RunHooks makes the release install even when nothing changed, so `box
	// update` can run the wiring on an already-deployed version.
	RunHooks bool
}

// ChartRef is what the chart source is asked for.
func (o Options) ChartRef() ChartRef {
	return ChartRef{Repo: o.ChartRepo, Version: o.ChartVersion}
}

// Result is what happened, for the caller to report or a test to assert.
type Result struct {
	FirstInstall bool
	// Host is the domain this release serves, when its values name one.
	Host    string
	Changes []seedbox.Change
	// Hosts are the app hosts given a listener and Certificate.
	Hosts []string
	// Login says the login page was re-rendered. It is not a Helm revision.
	Login bool
	// Storage is what box found it had to create, as Plan returned it.
	Storage []StorageObject
	// AnchorWrote says this run wrote or removed the host CA ConfigMap.
	AnchorWrote bool
	// PlexToken says this run wrote Plex's token (plextoken.go).
	PlexToken bool
	// NamespaceCreated says apply created the seedbox's namespace (root only).
	NamespaceCreated bool
	// StorageCreated is how many of those it wrote.
	StorageCreated int
	// JellyfinPassword and JellyfinLogin are set only on the apply that created
	// the key: the chart generates and keeps it, so a later run would print a
	// credential the person already has.
	JellyfinPassword string
	JellyfinLogin    string
	Applied          bool
	Revision         int
	Receipt          Receipt
}

// NetworkProof proves the cluster's CNI enforces NetworkPolicy. It runs after the
// confirm and after the seedbox's namespace exists, because that namespace is
// where its pods go, and before any app: a cluster that ignores policy must not
// have one installed behind one. The chart and its rendered values are the
// release's own, so the proof runs the image the release does.
type NetworkProof func(ctx context.Context, namespace string, chart Chart, values map[string]any) error

// Applier is `box apply`'s whole decision.
type Applier struct {
	Charts ChartSource
	// Open returns the release handle, after the grant names the namespace.
	Open func(namespace string) (ReleaseClient, error)
	// OpenSecret reads and, for a claimed Plex, writes one Secret key
	// (plextoken.go). nil seeds no token.
	OpenSecret func(namespace string) (SecretKeys, error)
	// OpenStorage is separate because PVs are cluster-scoped and Helm must never
	// own the claims.
	OpenStorage func(namespace string) (StorageClient, error)
	Confirm     Confirmer
	// Say is every line apply writes; nil writes nothing.
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

	// Files first: refusals here happen before a cluster client exists.
	sb, err := seedbox.Load(opts.GrantPath, opts.ValuesPath)
	if err != nil {
		return res, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return res, err
	}
	// A missing host path is refused before the chart is read: box does not
	// create directories, and a release over an empty volume is worse.
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
	// A bare apply has not shown which release it installs; init prints its own.
	if opts.Chart == nil && chart.Version != "" {
		r.Chart(chart.Name, chart.Version, chart.Ref)
	}
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		return res, err
	}
	// Cluster-derived values go last, like claim names: no file may set or
	// shadow them, and recomputing them keeps a re-run empty.
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
	// Holds keep a rolled-back app on its image; a hold the schema would not
	// render is refused here, not in a failed upgrade later.
	if opts.Holds != nil {
		held, err := opts.Holds(ctx, sb, chart, values)
		if err != nil {
			return res, err
		}
		if len(held) > 0 {
			overlayTree(values, held)
			if err := seedbox.Validate(chart.Schema, values); err != nil {
				return res, err
			}
		}
	}

	// Read the namespace before the release: a claim needs it, and on a fresh
	// cluster it is what is missing. Both this and the access question are reads.
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
			return res, fmt.Errorf("the namespace %s does not exist, and this kubeconfig may not create namespaces: root creates a seedbox's namespace with `box adduser`; ask root to add this box, then apply again", namespace)
		}
	} else if nsp.Enforce != podSecurityRestricted {
		enforce := "no " + podSecurityEnforce + " label"
		if nsp.Enforce != "" {
			enforce = podSecurityEnforce + "=" + nsp.Enforce
		}
		r.Warn(fmt.Sprintf("namespace %s does not enforce Pod Security %s (%s): root's labels win",
			namespace, podSecurityRestricted, enforce))
	}

	// Ask about the login client before anything is written, with the namespace.
	login, _ := opts.Hosts.(Login)
	// The chart mounts the client Secret whether or not it exists, so a missing
	// one is a pod that never runs. root's apply registers it; a user's run
	// stops and names the command that can (docs/box.md, "Apps that speak OIDC
	// sign in through the login").
	if oidcGatewayIP(values) != "" {
		if err := refuseUnregisteredClient(ctx, login, chart, values, namespace); err != nil {
			return res, err
		}
	}

	client, err := a.Open(namespace)
	if err != nil {
		return res, err
	}
	// A missing namespace is a missing release; do not ask Helm.
	var deployed map[string]any
	var installed ReleaseState
	exists := false
	if nsp.Exists {
		deployed, exists, err = client.Deployed(ctx)
		if err != nil {
			return res, err
		}
		if exists {
			installed, err = client.State(ctx)
			if err != nil {
				return res, err
			}
		}
	}
	// A chart move is work even with unchanged values: a digest bump ships as a
	// new release, and installing it is what moves the pod.
	chartMoved := exists && installed.ChartVersion != chart.Version
	// Hooks are work too: update moved the release with them off, and without
	// this the apply would answer "nothing to change" and wiring never runs.
	hooksWait := opts.RunHooks && !opts.SkipHooks
	res.FirstInstall = !exists
	res.Host = hostOf(values)
	// The Plex token is planned with the diff so it counts as work; it is
	// written after the confirm (plextoken.go).
	plexToken := a.planPlexToken(ctx, r, values, vols, namespace, exists)
	if plexToken.needed() {
		// Install only to run the wiring again, now that it can read a token.
		hooksWait = true
	}
	// Write the running peer port before the diff: a box from when the chart
	// defaulted it has none in either file, and it must not read as a change.
	if err := keepPeerPort(values, deployed, exists, chart.Schema, r); err != nil {
		return res, err
	}
	if exists {
		res.Changes = seedbox.Diff(deployed, values)
	}
	// Drop transient answers before the empty-diff check: the stored release
	// holds the previous secret, and a re-run must still be a no-op.
	res.Changes = withoutTransient(res.Changes, opts.TransientKeys)

	// Storage is planned separately and written before the release: pods need
	// the claims, and Helm must never own them: `helm uninstall` would delete the data.
	plan, err := store.Plan(ctx, namespace, vols)
	if err != nil {
		return res, err
	}
	res.Storage = plan

	// Read per-host TLS with the diff: a host with no listener is a route
	// nothing answers on (docs/box.md, "No run leaves an app host without its
	// listener"). The read is the cluster's, never a record.
	var hostPlan HostPlan
	if opts.Hosts != nil || login != nil {
		hostPlan, err = a.hostPlan(ctx, opts, chart, sb, values)
		if err != nil {
			return res, err
		}
		res.Hosts = hostPlan.Hosts
	}
	// A re-rendered theme is work; a login already right is not.
	if login != nil {
		res.Login, err = login.Pending(ctx, hostPlan)
		if err != nil {
			return res, err
		}
	}

	// Read the host CA with the diff: a missing anchor is a real write.
	var anchorLine string
	anchorPlan := AnchorPlan{Domain: hostOf(values), Issuer: opts.AnchorIssuer, Namespace: namespace}
	if opts.Anchor != nil && anchorPlan.Domain != "" {
		// The catalog's clients, not the login rule's: their Secrets cannot wait
		// for a host, and the rule inputs may never be built.
		anchorPlan.OIDC, err = oidcClients(chart, values)
		if err != nil {
			return res, err
		}
		anchorLine, err = opts.Anchor.Plan(ctx, anchorPlan)
		if err != nil {
			return res, err
		}
	}

	if exists && len(res.Changes) == 0 && len(plan) == 0 && nsp.Exists && len(hostPlan.Hosts) == 0 && !res.Login && anchorLine == "" && !chartMoved && !hooksWait {
		r.Rows([]say.Row{{Mark: say.MarkNote, Name: ReleaseName, Facts: []string{"nothing to change", namespace}}})
		return res, nil
	}

	// Read the image moves before the question: a move migrates forward, so its
	// config is copied first. An unchanged release reads no pods.
	snapPlan := SnapshotPlan{Namespace: namespace, Chart: chart, Values: values, ConfigRoot: configRoot(vols)}
	var snapWork SnapshotWork
	var snapDirs SnapshotDirs
	if opts.Snapshots != nil && exists && len(res.Changes) > 0 {
		snapWork, err = opts.Snapshots.Pending(ctx, snapPlan)
		if err != nil {
			return res, err
		}
	}

	if exists {
		switch {
		case len(res.Changes) > 0:
			r.Line(fmt.Sprintf("%s in %s · %d %s", ReleaseName, namespace, len(res.Changes), plural(len(res.Changes), "change", "changes")))
			r.Diff(changeItems(res.Changes))
		case chartMoved:
			// Only the release moved; its pins are the whole change.
			r.Line(fmt.Sprintf("%s in %s · %s → %s", ReleaseName, namespace, installed.ChartVersion, chart.Version))
		case hooksWait:
			// Only the hook is left to run, on the deployed version.
			r.Line(fmt.Sprintf("%s in %s · %s, running its hooks", ReleaseName, namespace, chart.Version))
		default:
			// Only work outside the release remains.
			r.Line(fmt.Sprintf("%s in %s · the release is unchanged", ReleaseName, namespace))
		}
	} else {
		r.Line(fmt.Sprintf("%s in %s · first install", ReleaseName, namespace))
		var details []string
		if host := hostOf(values); host != "" {
			details = append(details, "host: "+host)
		}
		if apps := EnabledApps(values); len(apps) > 0 {
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
	// Say which moved apps are copied and which keep running through the change.
	if len(snapWork.Moves) > 0 {
		lines := make([]string, 0, len(snapWork.Moves))
		for _, move := range snapWork.Moves {
			lines = append(lines, move.Row())
		}
		r.List("snapshots", lines)
	}

	ok, err := a.Confirm.Ask(fmt.Sprintf("apply %s in %s?", ReleaseName, namespace), false)
	if err != nil {
		return res, err
	}
	if !ok {
		r.Line("oh heck, nvm")
		return res, ErrCancelled
	}

	// Ask transient answers after the review: a claim token expires in minutes.
	if opts.AfterConfirm != nil {
		extra, err := opts.AfterConfirm(ctx)
		if err != nil {
			return res, err
		}
		if err := mergeTransient(values, extra); err != nil {
			return res, err
		}
	}

	// Per-host TLS goes after the confirm and before the release; a stop leaves
	// the release and old routes as they were.
	if len(hostPlan.Hosts) > 0 || len(hostPlan.All) > 0 {
		if err := opts.Hosts.Ensure(ctx, hostPlan); err != nil {
			return res, err
		}
	}
	// Write the anchor at the first point its namespace exists and before the
	// release: a pod that starts without it crash-loops through Helm's wait. The
	// same step writes OIDC client Secrets, so the login follows it.
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
		// The login goes after hosts and the anchor, on its own: a theme checks
		// no DNS and writes no listener, and compares before applying.
		if err := reconcileLogin(); err != nil {
			return res, err
		}
	}

	// Only steps outside the release had work, so make no Helm revision: a
	// certificate or login page must not churn what users roll back to.
	if exists && len(res.Changes) == 0 && len(plan) == 0 && nsp.Exists && !chartMoved && !hooksWait {
		return res, nil
	}

	// Everything is checked, but a write can fail: remember what this run
	// creates so a failure before the release can remove it.
	var created []StorageObject

	// Create the namespace only when it was missing: an existing one keeps
	// whatever labels its owner gave it.
	if !nsp.Exists {
		if err := store.CreateNamespace(ctx); err != nil {
			return res, err
		}
		res.NamespaceCreated = true
		r.Detail(fmt.Sprintf("created namespace %s (Pod Security %s)", namespace, podSecurityRestricted))
		// The anchor and OIDC Secrets live here, so write them now.
		if err := writeAnchor(); err != nil {
			return res, err
		}
		if err := reconcileLogin(); err != nil {
			return res, err
		}
	}

	// The cluster's policy enforcement is proved here: the namespace exists, no app
	// does, and a refusal costs the namespace alone.
	if opts.NetworkProof != nil {
		if err := opts.NetworkProof(ctx, namespace, chart, values); err != nil {
			return res, err
		}
	}

	if len(plan) > 0 {
		step := r.Start(say.Op{Name: "storage"})
		r.Detail(fmt.Sprintf("writing the storage for %s", namespace))
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
		step.Done(fmt.Sprintf("%d object%s", len(plan), plural(len(plan), "", "s")))
	}

	// Stop the apps immediately before the release: everything else has already
	// succeeded, so nothing but the upgrade can fail while they are down.
	if len(snapWork.Copied()) > 0 {
		snapDirs, err = opts.Snapshots.Take(ctx, snapPlan, snapWork)
		if err != nil {
			return res, err
		}
	}

	// The Plex token goes last, before the release, so the wiring Job mounts it.
	// A failed write is one line: the wiring reports the same pending row.
	if plexToken.needed() {
		res.PlexToken = a.writePlexToken(ctx, r, plexToken)
	}

	// The chart generates jellyfin-user-password once and keeps it. Read before
	// the release: absent means this apply creates it. A failed read shows
	// nothing, because printing a stored password later is the failure to avoid.
	var jellyfinPasswordNew bool
	if a.OpenSecret != nil && appEnabled(values, jellyfinApp) {
		if store, err := a.OpenSecret(namespace); err == nil {
			if pw, err := store.Get(ctx, jellyfinUserPasswordKey); err == nil && pw == "" {
				jellyfinPasswordNew = true
			}
		}
	}

	started := time.Now()
	// The release is one row; revision and namespace are the log's (rule 5).
	upgrade := r.Start(say.Op{Name: ReleaseName + " " + chart.Version})
	r.Detail(fmt.Sprintf("installing %s in %s", ReleaseName, namespace))
	rev, err := client.Upgrade(ctx, chart, values)
	if err != nil {
		upgrade.Failed(err)
		return res, a.undo(ctx, store, created, withSnapshotDirs(err, snapDirs))
	}
	res.Applied = true
	res.Revision = rev
	r.Detail(fmt.Sprintf("applied %s revision %d in %s", ReleaseName, rev, namespace))
	upgrade.Done(strings.Join(releaseFacts(values, res.FirstInstall, chartMoved, installed.ChartVersion), " · "))

	// Then the other direction: a name a previous run served and this one does not keeps
	// answering (and its Certificate renews) until it is removed. Only now: until Helm moved
	// the routes, the old name is where the running app is reached, and a failed release
	// keeps it. A name left behind is a listener with no route, so a failure here is the
	// next apply's to retry, not this one's to fail.
	if pruner, ok := opts.Hosts.(HostPruner); ok {
		if err := pruner.Prune(ctx, hostPlan); err != nil {
			r.Warn(fmt.Sprintf("an app host this box no longer serves is still on the Gateway; the next apply removes it: %v", err))
		}
	}

	// The key is new; a still-absent one is an older chart, so print nothing.
	if jellyfinPasswordNew {
		if store, err := a.OpenSecret(namespace); err == nil {
			if pw, err := store.Get(ctx, jellyfinUserPasswordKey); err == nil && pw != "" {
				res.JellyfinPassword = pw
				res.JellyfinLogin, _ = values["login"].(string)
			}
		}
	}

	// No hooks means no receipt: the caller applies again with them later.
	if opts.SkipHooks {
		return res, nil
	}

	// A wiring failure is when the operator reaches for the copy: name it here
	// as a failed upgrade does.
	receipt, err := a.waitReceipt(ctx, client, started)
	if err != nil {
		return res, withSnapshotDirs(err, snapDirs)
	}
	res.Receipt = receipt
	ReportReceipt(r, receipt, rehearsalOf(values))
	if receipt.Status != "ok" {
		// The receipt's rows are already shown.
		if receipt.Error != "" {
			return res, withSnapshotDirs(say.Reported{Err: fmt.Errorf("wiring failed: %s", receipt.Error)}, snapDirs)
		}
		return res, withSnapshotDirs(say.Reported{Err: errors.New("wiring failed: the receipt does not say ok")}, snapDirs)
	}
	// Keep the newest two copies per app; a failed prune costs disk, never the
	// apply.
	if len(snapDirs) > 0 {
		if perr := opts.Snapshots.Prune(ctx, snapPlan, snapWork); perr != nil {
			r.Warn(fmt.Sprintf("keeping the snapshot of %s: %v", strings.Join(sortedKeys(snapDirs), ", "), perr))
		}
		// End by saying where the newest copy is and how to put it back.
		for _, note := range snapshotNotes(snapDirs) {
			r.Line(note)
		}
	}
	return res, nil
}

// withSnapshotDirs adds the copies' locations to a failed upgrade's error: the
// stopped apps' state was copied, and one command puts it back.
func withSnapshotDirs(err error, dirs SnapshotDirs) error {
	if len(dirs) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s", err, strings.Join(snapshotNotes(dirs), "; "))
}

// snapshotNotes says where each copy is and how to put it back.
func snapshotNotes(dirs SnapshotDirs) []string {
	notes := make([]string, 0, len(dirs))
	for _, app := range sortedKeys(dirs) {
		notes = append(notes, fmt.Sprintf("the snapshot of %s is in %s; `box rollback %s` puts it back", app, dirs[app], app))
	}
	return notes
}

// configRoot is the grant's config path, where copies and holds live.
func configRoot(vols []seedbox.Volume) string {
	for _, v := range vols {
		if v.Kind == "config" {
			return v.Path
		}
	}
	return ""
}

// sortedKeys orders app names so sentences do not depend on a map walk.
func sortedKeys(dirs SnapshotDirs) []string {
	out := make([]string, 0, len(dirs))
	for app := range dirs {
		out = append(out, app)
	}
	sort.Strings(out)
	return out
}

// loadChart returns the caller's chart or one from the registry; a chart this
// box is too old for is refused here (minbox.go), before any cluster touch.
func (a *Applier) loadChart(ctx context.Context, opts Options) (Chart, error) {
	if opts.Chart != nil {
		return *opts.Chart, nil
	}
	if a.Charts == nil {
		return Chart{}, errors.New("no chart source: box reads its chart from the registry")
	}
	return a.Charts.Load(ctx, opts.ChartRef())
}

// hostPlan is the work outside the release: per-host TLS and the login's rule
// inputs. Which apps get a host is the catalog's (`exposure: host`); a chart
// with no catalog or no domain names none.
func (a *Applier) hostPlan(ctx context.Context, opts Options, chart Chart, sb *seedbox.Seedbox, values map[string]any) (HostPlan, error) {
	plan := HostPlan{Domain: hostOf(values), Forced: forcedIssuer(values), Issuers: opts.HostIssuers}
	apps, err := hostedHosts(chart, values, plan.Domain)
	if err != nil {
		return plan, err
	}
	// The box's own name is a host this run serves: without its listener the
	// Gateway has no 443, and every app dialing /auth waits.
	hosts := apps
	if plan.Domain != "" {
		hosts = append([]string{plan.Domain}, apps...)
	}
	plan.Served = hosts
	// Build the login's inputs even with no hosts: the reconcile still runs.
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
		// Include stale certificates as work, so an idempotent run still says
		// nothing to change while a moved issuer is written.
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

// loginRule is the login's half of both steps; init passes internal/login the
// same four, so the commands write one file.
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
		Hosted:    hosts,
		OwnLogin:  cat.SelfAuthenticating(),
		Values:    values,
		Image:     chart.Pins.Authelia,
		Namespace: namespace,
		OIDC:      cat.OIDCApps(EnabledApps(values)),
	}, nil
}

// oidcClients is the catalog's OIDC apps in order: only this half, so clients
// are not tied to a host name.
func oidcClients(chart Chart, values map[string]any) ([]string, error) {
	if len(chart.Catalog) == 0 {
		return nil, nil
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return nil, err
	}
	return cat.OIDCApps(EnabledApps(values)), nil
}

// refuseUnregisteredClient refuses a run that cannot write the login when an
// enabled app has no client. It is a read before the confirm, so a user's plan
// is never one it cannot carry out.
func refuseUnregisteredClient(ctx context.Context, login Login, chart Chart, values map[string]any, namespace string) error {
	access, ok := login.(LoginAccess)
	if !ok {
		return nil
	}
	apps, err := oidcClients(chart, values)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return nil
	}
	may, err := access.MayWrite(ctx)
	if err != nil {
		return err
	}
	if may {
		return nil
	}
	var missing []string
	for _, app := range apps {
		there, err := access.ClientRegistered(ctx, namespace, app)
		if err != nil {
			return err
		}
		if !there {
			missing = append(missing, app)
		}
	}
	if len(missing) == 0 {
		// Every client is registered: a re-enabled app keeps its Secret.
		return nil
	}
	return fmt.Errorf("%s signs in through the login and has no client yet, and only root can register one: run `box init` as root for this seedbox, then apply again", strings.Join(missing, ", "))
}

// oidcGatewayIP is the grant's record, injected only when the chart declares
// the key. Empty means no OIDC at all, so no client Secret and no refusal.
func oidcGatewayIP(values map[string]any) string {
	auth, _ := values["auth"].(map[string]any)
	oidc, _ := auth["oidc"].(map[string]any)
	ip, _ := oidc["gatewayIP"].(string)
	return strings.TrimSpace(ip)
}

// hostedHosts is <instance>.<domain> for every enabled instance of an `exposure: host`
// app: the name is the instance's, because a second instance of one app is reached under
// a name of its own.
func hostedHosts(chart Chart, values map[string]any, domain string) ([]string, error) {
	if len(chart.Catalog) == 0 || domain == "" {
		return nil, nil
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return nil, err
	}
	var hosted []seedbox.Instance
	for _, inst := range seedbox.Instances(values) {
		app, ok := cat.App(inst.App)
		if !ok || app.Exposure != catalog.ExposureHost {
			continue
		}
		hosted = append(hosted, inst)
	}
	// The chart refuses the same three; a plain apply must not write a certificate for a
	// name the login's cookie cannot cover, or two for one name.
	if err := seedbox.CheckHosts(hosted, domain); err != nil {
		return nil, err
	}
	var hosts []string
	for _, inst := range hosted {
		name, err := inst.HostName(domain)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, name)
	}
	return hosts, nil
}

// forcedIssuer is tls.issuer; "" means each name's DNS answer decides.
func forcedIssuer(values map[string]any) string {
	tls, _ := values["tls"].(map[string]any)
	forced, _ := tls["issuer"].(string)
	return strings.TrimSpace(forced)
}

// waitReceipt waits for this run's receipt: one from an earlier revision is not
// evidence, so its start must follow the upgrade. It shows the Job's newest log
// line, read in the background (joblog.go), so a slow migration does not look
// broken.
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

	tail := startJobLog(ctx, client, poll)
	defer func() {
		tail.stop()
		tail.logTo(r)
	}()

	var source func() string
	if tail != nil {
		source = tail.newest
	}
	task := r.Start(say.Op{Label: wiringName, Budget: budget, Source: source})

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
		// No receipt yet, or one from an earlier revision.
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
	// End silently; the row below says what the Job was doing at the deadline.
	task.Done("")
	row := say.Row{
		Mark:  say.MarkFailed,
		Name:  wiringName,
		Facts: []string{"no receipt after " + say.Duration(budget)},
		Why:   tail.newest(),
	}
	r.Rows([]say.Row{row, {
		Mark:    say.MarkNote,
		Facts:   []string{"your apps are running", "only the wiring stopped"},
		Command: "box apply",
	}})
	// The rows are the screen's; the error is the log's.
	err = errors.New("the wiring receipt never arrived: the release is installed but its wiring hook has not reported")
	r.Detail(err.Error())
	return Receipt{}, say.Reported{Err: err}
}

// undoTimeout bounds undo: a few deletes take well under a second.
const undoTimeout = time.Minute

// undo removes the volumes this run created when a failure stops it before the
// release; what was already there, and the claims and namespace, stay.
//
// It detaches from cancellation -- a Ctrl-C is when the volume must come off --
// and carries its own deadline so a silent API server cannot hold the run open.
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

// printPlan shows the namespace and claims this run creates; Helm never sees
// them.
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

// rehearsalOf reads the grant's own flag from the effective values.
func rehearsalOf(values map[string]any) bool {
	on, _ := values["rehearsal"].(bool)
	return on
}

// changeItems turns a diff into the marked plan; the text is FormatChanges'.
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
			// Drop FormatChanges' glyph; the plan's mark is what is coloured.
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

// EnabledApps is every app the values switch on, sorted; a disabled app has no
// database to migrate and never moves.
func EnabledApps(values map[string]any) []string {
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

// overlayTree merges mappings so other keys survive; a list or scalar replaces,
// because a NetworkPolicy peer list is one decision.
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

// releaseFacts is the release row besides its time.
func releaseFacts(values map[string]any, first, moved bool, from string) []string {
	var facts []string
	switch {
	case first:
		facts = append(facts, "first install")
	case moved && from != "":
		facts = append(facts, "from "+from)
	}
	if apps := EnabledApps(values); len(apps) > 0 {
		facts = append(facts, fmt.Sprintf("%d apps", len(apps)))
	}
	if port, ok := portAt(values); ok && port > 0 {
		facts = append(facts, fmt.Sprintf("torrent port %d", port))
	}
	return facts
}
