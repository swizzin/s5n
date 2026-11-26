// Package rewrite is the contract between migrate and the per-app rewriters.
// Apply stages each app's config and calls its rewriter twice: Check reads
// everything and writes nothing, and one gap stops the whole migration before
// any Write (docs/box.md, "Shown before it's done"); Write makes exactly what
// Check reported, in the staged copy. The source is never opened for writing.
// Rewriters run before the target's first start, so they read and write the
// source version's formats.
package rewrite

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// Endpoint is where an app answers on the box; a rewritten connection points at
// it.
type Endpoint struct {
	Host    string // the in-cluster Service name
	Port    int    // the chart's port for the app
	URLBase string // the chart's URL base, "" for none
}

// Input is everything a rewriter is given; it holds no clients, because a
// rewriter works on files.
type Input struct {
	// App is box's instance name, the key in the plan.
	App string
	// Dir is the staged copy of the app's config, what it will see as /config:
	// Check reads it, Write edits it in place.
	Dir string
	// Plan is the whole plan: paths, ports and host rules.
	Plan *plan.Plan
	// Targets is every migrated app's endpoint on the box, by instance name.
	Targets map[string]Endpoint
	// Runs is every instance of each app by catalog name ("sonarr" ->
	// [sonarr sonarr-4k]); a proposal asks Sole so it names an app only when the
	// box runs one instance.
	Runs map[string][]string
	// Run runs a tool in the app's pinned image with Dir at /config, for formats
	// only the app's own binary may write (Plex's database). Nil when the
	// caller cannot run images; a rewriter that needs it says so.
	Run Runner
	// Rehearsal is a copy of a live source applied locked: nothing leaves the
	// box, so a rewriter whose app would act on the network writes its state
	// quiet (qBittorrent's torrents start stopped).
	Rehearsal bool
}

// Sole is the one instance this box runs of a catalog app, and whether it runs
// exactly one: with two, an unsettled address is left and called out, not
// guessed at.
func (in Input) Sole(app string) (string, bool) {
	names := in.Runs[app]
	if len(names) != 1 {
		return "", false
	}
	return names[0], true
}

// Resolve settles an address this app stored for another app: the plan's host
// rules and the apps' ports decide first, then an address that already names one
// of this run's endpoints is that app. The fallback keeps the two passes
// consistent -- a Check that rewrote a connection leaves the box's own host and
// port in the staged copy, and no source rule names them, so Write would stop on
// its own work. The plan wins wherever it says something.
func (in Input) Resolve(host string, port int) plan.Resolution {
	if in.Plan != nil {
		if res := in.Plan.Resolve(in.App, host, port); res.Verdict != plan.Unclassified {
			return res
		}
	}
	names := make([]string, 0, len(in.Targets))
	for name := range in.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	h := normalizeHost(host)
	for _, name := range names {
		ep := in.Targets[name]
		if ep.Port == port && normalizeHost(ep.Host) == h {
			return plan.Resolution{Verdict: plan.Rewrite, App: name}
		}
	}
	return plan.Resolution{}
}

// ResolveURL is Resolve with the URL's path, which is the only thing that says
// which app an address meant on a host of paths; one with no path is not guessed.
func (in Input) ResolveURL(u *url.URL) plan.Resolution {
	if u == nil {
		return plan.Resolution{}
	}
	port := URLPort(u)
	if in.Plan != nil {
		if res := in.Plan.ResolveURL(in.App, u.Hostname(), port, u.EscapedPath()); res.Verdict != plan.Unclassified {
			return res
		}
	}
	names := make([]string, 0, len(in.Targets))
	for name := range in.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	h := normalizeHost(u.Hostname())
	for _, name := range names {
		ep := in.Targets[name]
		if ep.Port == port && normalizeHost(ep.Host) == h {
			return plan.Resolution{Verdict: plan.Rewrite, App: name}
		}
	}
	return plan.Resolution{}
}

// URL is the address an app's endpoint answers at inside the release. Writing it
// drops whatever path the source stored: box serves the app under its own base.
func (e Endpoint) URL() string {
	return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) + strings.TrimSuffix(e.URLBase, "/")
}

// normalizeHost is a stored host lower-cased, without a trailing dot or IPv6
// brackets.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

// URLPort is the port a stored URL addresses: the one it names, or the scheme's
// default (https is 443), because the plan settles by port and the refusal
// prints it. Any other scheme is 0.
func URLPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		n, _ := strconv.Atoi(p)
		return n
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}

// Runner runs one tool in an app's pinned image with the staged config at
// /config, returning its stdout. argv[0] is the command inside the image and
// argv[1:] its arguments; stdin is the script or data it reads. A failure carries
// what the tool wrote to stderr: that is the only account of why the app's own
// binary refused.
type Runner func(ctx context.Context, app string, argv []string, stdin []byte) ([]byte, error)

// Change is one kind of edit in one place, counted; a plan shows counts per file
// and table, never row by row.
type Change struct {
	File  string // relative to Dir
	Where string // table.column, section/key, JSON pointer
	What  string // "path", "host", "port", "url base", "dropped"
	Count int
}

// Report is the result of a pass.
type Report struct {
	App     string
	Changes []Change
	// Uncovered are absolute paths no rule covers. Paths in history (records of
	// what happened, never acted on) are rewritten when a rule covers them and
	// are never listed here.
	Uncovered []plan.Uncovered
	// Unclassified are stored addresses no host rule settles.
	Unclassified []Address
	// Left are stored values left exactly as they are, on purpose, and why. None
	// blocks the run: leaving is the decision, not a gap (a client the app never
	// dials, a type box does not run, a column naming two apps).
	Left []Left
	// Defaults are settlements proposed for an address the plan does not
	// settle, because the field itself says what it can only mean (a
	// PeerBanHelper downloader of type qBittorrent is the box's qBittorrent). A
	// Check writes nothing: Settle turns these into defaulted rules (settle.go).
	Defaults []Default
	// Accounts are the app's own account rows this pass renames to the account
	// box's init container signs in as (WiringAccount): the app allows one
	// account, and the init then writes the release Secret's password onto it.
	Accounts []Account
	// Notes are things the operator should know that stop nothing.
	Notes []string
	// Owned are the connections this app holds to another app that the rewrite
	// pointed at the box; wiring adopts them by ID instead of creating seconds
	// (docs/box.md, "Migrated connections are adopted by ID").
	Owned []Owned
	// Stopped is how many stored objects were written stopped (a rehearsal's
	// torrents): state the app would otherwise act on, not a stored setting.
	Stopped int
}

// Owned is one adopted connection: a download client or a Prowlarr
// application, by the ID the app gave it, and the box app it now points at.
type Owned struct {
	Kind string // "downloadclient", "application", "pbh-downloader"
	// ID is the numeric id a database-backed app gave the object.
	ID int
	// StringID is the id an app with string ids stores instead: PeerBanHelper's
	// downloaders are keys under `client:` (internal/wiring/owned.go,
	// PbhDownloaderID). Exactly one of ID and StringID is set.
	StringID string
	Name     string // as the source named it, kept
	Target   string // box instance name
}

// Address is a stored host reference no rule covers.
type Address struct {
	App, File, Where string
	Host             string
	Port             int
}

func (a Address) String() string {
	return fmt.Sprintf("%s %s %s: %s:%d", a.App, a.File, a.Where, a.Host, a.Port)
}

// Text is the address as the app stored it: host:port, or the host alone when it
// stored no port.
func (a Address) Text() string {
	if a.Port == 0 {
		return a.Host
	}
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// Left is one stored value a pass leaves exactly as it is, and why. The why is
// the reason only; the caller puts "left as it is" in front of it.
type Left struct {
	File  string // relative to Dir
	Where string // table.column, section/key, JSON pointer
	What  string // the stored value, as the app has it
	// Why is the one reason in a short clause, for the screen, where a row is one
	// fact (docs/output.md, rule 2).
	Why string
	// Detail is the whole sentence for the report: what the value means, what a
	// rewrite would have done, and what to do (rule 7).
	Detail string
}

// Default is one settlement a pass proposes for an address the plan does not
// settle, where the field it was found in fixes the answer. It is a proposal,
// not a write: Settle turns it into a host rule with default and why. An address
// no proposal names is left as it is.
type Default struct {
	Host string // as the app stored it
	Port int    // as the app stored it; 0 when it stored none
	// App is the box instance the address can only mean.
	App string
	// Why is one line saying what makes it that app, and is kept as the rule's
	// own reason.
	Why string
}

// WiringAccount is the one account box owns in an app with its own user table
// (charts/s5n/templates/_helpers.tpl, s5n.wiringAccount): the init container
// creates it and writes the release Secret's password onto it, and wiring signs
// in with it. The app allows one account, so the source's must become this one;
// TestWiringAccountIsTheCharts pins the name.
const WiringAccount = "s5n"

// Account is one row of an app's user table renamed to the account box owns. The
// row keeps its id, so everything keyed by it stays; only the name changes, and
// the report prints both names.
type Account struct {
	// From is the name the source stored.
	From string
	// To is the name box's init container signs in as: WiringAccount.
	To string
}

// Blocked reports whether apply has to stop on this report.
func (r Report) Blocked() bool { return len(r.Uncovered) > 0 || len(r.Unclassified) > 0 }

// Rewriter is one app's rewriter: Check writes nothing, and Write makes exactly
// what the Check before it reported.
type Rewriter interface {
	Check(ctx context.Context, in Input) (Report, error)
	Write(ctx context.Context, in Input) (Report, error)
}

// RunnerUser is a rewriter that needs Input.Run: apply asks before staging
// anything, so a run that cannot start images refuses up front.
type RunnerUser interface {
	NeedsRunner() bool
}

var registry = map[string]Rewriter{}

// Register files a rewriter under a catalog app name ("sonarr", not
// "sonarr-4k"), shared by every instance; a second registration panics.
func Register(app string, r Rewriter) {
	if _, dup := registry[app]; dup {
		panic("rewrite: second rewriter registered for " + app)
	}
	registry[app] = r
}

// For returns the rewriter for a catalog app. An app with no rewriter cannot be
// migrated: apply refuses rather than copy a config it cannot read.
func For(app string) (Rewriter, bool) {
	r, ok := registry[app]
	return r, ok
}

// Apps is every app with a rewriter, sorted.
func Apps() []string {
	out := make([]string, 0, len(registry))
	for a := range registry {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
