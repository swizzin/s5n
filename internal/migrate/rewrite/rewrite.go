// Package rewrite is the contract between `box migrate` and the per-app
// rewriters: what apply hands a rewriter, what a rewriter must report, and the
// two passes every one runs.
//
// Apply copies each app's source config into a staging directory and calls the
// app's rewriter twice on it. The first pass, Check, reads everything and
// writes nothing: it returns every change it would make and every stored value
// the plan does not settle. Apply runs Check for every app before any app's
// Write, so one gap in one app stops the whole migration before its first
// write (docs/box.md, "Shown before it's done"). The second pass, Write, makes
// exactly the changes Check reported, in the staged copy only. The source is
// never opened for writing; it stays the rollback.
//
// Rewriters run before the target's first start (research: the target's
// schema migrations carry rewritten rows through, and the first start already
// writes paths), so a rewriter reads and writes the source version's formats.
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

// Endpoint is where an app answers on the box, which is what a rewritten
// connection between two apps points at.
type Endpoint struct {
	Host    string // the in-cluster Service name
	Port    int    // the chart's port for the app
	URLBase string // the chart's URL base, "" for none
}

// Input is everything a rewriter is given. It holds no clients: a rewriter
// works on files.
type Input struct {
	// App is box's instance name, the key in the plan.
	App string
	// Dir is the staged copy of the app's config: the directory the app will
	// see as /config. Check reads it; Write edits it in place.
	Dir string
	// Plan is the whole plan: the app's own path table, every app's ports and
	// the host rules.
	Plan *plan.Plan
	// Targets is every migrated app's endpoint on the box, by instance name.
	Targets map[string]Endpoint
	// Runs is every instance the plan has of each app, by catalog app name
	// ("sonarr" -> [sonarr sonarr-4k]). A rewriter that proposes a settlement
	// for an address asks Sole with it: an address whose field names one app
	// can only be settled by itself when the box runs one instance of it.
	Runs map[string][]string
	// Run executes a tool inside the app's pinned image with Dir mounted at
	// /config, for formats only the app's own binary may write (Plex's
	// database). Nil when the caller can't run images; a rewriter that needs
	// it returns an error saying so.
	Run Runner
	// Rehearsal is a copy of a live source applied locked: nothing leaves the
	// box. A rewriter whose app acts on the network with nothing else to stop
	// it writes its state quiet here (qBittorrent's torrents start stopped);
	// every other app is quiet because the release's egress policy is.
	Rehearsal bool
}

// Sole is the one instance this box runs of a catalog app, and whether it runs
// exactly one. It is what a proposal is built on: two qBittorrents and an
// address nothing settles cannot mean one of them, so the address is left and
// called out rather than pointed at a guess.
func (in Input) Sole(app string) (string, bool) {
	names := in.Runs[app]
	if len(names) != 1 {
		return "", false
	}
	return names[0], true
}

// Resolve settles an address this app stored for another app, and is the one
// way a rewriter asks. The plan answers first: its host rules and the apps'
// ports decide. An address the plan does not settle, but that already names one
// of this run's endpoints, is that app.
//
// The second chance is what keeps a rewriter's two passes consistent. A Check
// that rewrote a connection leaves the box's own host and port in the staged
// copy, and no source rule names them: the plan describes the install being
// left behind, not the box. Without this, the Write that follows the very Check
// that made the rewrite would stop on its own work.
//
// The plan still wins wherever it says something, so a host rule the operator
// wrote is never overridden by an endpoint this run happens to hold.
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

// ResolveURL settles an address this app stored as a URL, with the path it
// stored when it has one. It is what a rewriter holding a whole URL asks, and
// it differs from Resolve exactly as plan.ResolveURL does: one host can serve
// several apps under paths (box's own shape is one name with each app under a
// path), and the path is then the only thing that says which app an address
// meant. An address with no path on such a host is not settled by guessing at
// one of them.
//
// The plan answers first, then this run's own endpoints, exactly as Resolve
// does, so a rewriter's two passes stay consistent.
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

// URL is the address an app's endpoint answers at: plain HTTP inside the
// release, the chart's Service and port, and the chart's URL base as the path.
// A rewriter that stores a URL writes this, which drops whatever path the
// source's URL had: box serves the app under its own base, not the source's.
func (e Endpoint) URL() string {
	return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) + strings.TrimSuffix(e.URLBase, "/")
}

// normalizeHost is the form two stored hosts are compared in: no trailing dot,
// no IPv6 brackets, lowercase. A host is a DNS name here, and DNS is
// case-insensitive.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

// URLPort is the port a stored URL addresses: the one it names, or the one its
// scheme implies when it names none, so https://host/path is port 443 and not
// 0. The plan settles an address by its port, and the refusal prints it, so an
// http URL written without one is the port it is actually reached on. A URL of
// any other scheme is 0. (url.Parse has already refused a malformed port, so
// the explicit case is a number.)
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

// Runner runs one tool inside the pinned image of an app with the staged config
// directory mounted at /config, and returns the tool's stdout.
//
// The convention is the container's: argv[0] is the command inside the image
// (the entrypoint Plex's image ships, for the one rewriter that needs it),
// argv[1:] are that command's arguments, and the staged directory is at
// /config, which is where the chart mounts the app's config claim. stdin is the
// script or data the tool reads, when it takes any. A run that fails returns an
// error carrying what the tool wrote to stderr: that text is the only account of
// why the app's own binary refused, and a rewriter passes it on rather than
// reporting a bare exit status.
type Runner func(ctx context.Context, app string, argv []string, stdin []byte) ([]byte, error)

// Change is one kind of edit in one place, counted: "sonarr.db Series.Path",
// 412. A plan shows these as counts per file and table, never row by row.
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
	// Uncovered are absolute paths no rule covers. Paths in history (records
	// of what happened, never read back to act on) are rewritten when a rule
	// covers them and are never listed here.
	Uncovered []plan.Uncovered
	// Unclassified are stored addresses no host rule settles.
	Unclassified []Address
	// Left are stored values this pass leaves exactly as it is, on purpose,
	// and why. None of them blocks the run: they are the cases where leaving
	// is the decision, not a gap in the plan (a client the app never dials, a
	// client of a type box does not run, a database column that names two apps
	// at once). The operator is told what happens to each one.
	Left []Left
	// Defaults are settlements this pass proposes for an address the plan does
	// not settle, because the field itself says which app the address can only
	// mean: a PeerBanHelper downloader of type qBittorrent can only be the
	// box's qBittorrent, whatever address it stored. A Check writes nothing, so
	// this is a proposal: Settle turns it into a rule with default and why (see
	// internal/migrate/apply, settle.go). An address with no proposal here is
	// left as it is.
	Defaults []Default
	// Notes are things the operator should know that stop nothing: a table
	// emptied on purpose, an import list's remote filter left alone.
	Notes []string
	// Owned are the connections this app holds to another app that the
	// rewrite pointed at the box. Wiring adopts them by ID on its first run
	// instead of creating a second one beside each (docs/box.md, "Migrated
	// connections are adopted by ID").
	Owned []Owned
	// Stopped is how many stored objects this pass wrote in a stopped state:
	// a rehearsal's torrents. It is a count of state the app would otherwise
	// act on, not a change to a stored setting, so it has its own number
	// rather than a Change row.
	Stopped int
}

// Owned is one adopted connection: a download client or a Prowlarr
// application, by the ID the app gave it, and the box app it now points at.
type Owned struct {
	Kind string // "downloadclient", "application", "pbh-downloader"
	// ID is the numeric id a database-backed app gave the object.
	ID int
	// StringID is the id an app that names its objects with a string stores
	// instead: PeerBanHelper's downloaders are keys under `client:` in its own
	// config.yml, and wiring records the one it manages by that string
	// (internal/wiring/owned.go, PbhDownloaderID). Exactly one of ID and
	// StringID is set.
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

// Text is the address the way the app stored it, for a line the operator
// reads: host:port, or the host alone when the app stored no port.
func (a Address) Text() string {
	if a.Port == 0 {
		return a.Host
	}
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// Left is one stored value a pass leaves exactly as it is, and why. The why is
// one line and is the reason only: it says what the value is and what happens to
// it, and the caller puts "left as it is" in front of it, because that is the
// decision rather than part of the reason.
type Left struct {
	File  string // relative to Dir
	Where string // table.column, section/key, JSON pointer
	What  string // the stored value, as the app has it
	Why   string // one line: why it is left, and what happens to it
}

// Default is one settlement a pass proposes for an address the plan does not
// settle, because the field it was found in says which app the address can only
// mean. It is a proposal, not a write: Settle turns it into a host rule with
// default and why, and apply then makes the rewrite the rule describes.
//
// A proposal is only ever made where the app's own field fixes the answer: the
// type is the app's, not a guess at the address. Nothing else about an
// unsettled address is a default, and an address no proposal names is left as
// it is.
type Default struct {
	Host string // as the app stored it
	Port int    // as the app stored it; 0 when it stored none
	// App is the box instance the address can only mean.
	App string
	// Why is one line saying what makes it that app, and is kept as the rule's
	// own reason.
	Why string
}

// Blocked reports whether apply has to stop on this report.
func (r Report) Blocked() bool { return len(r.Uncovered) > 0 || len(r.Unclassified) > 0 }

// Rewriter is one app's rewriter. Check must not write anything, anywhere;
// Write must make exactly the changes the Check before it reported, and fail
// rather than make a change Check did not.
type Rewriter interface {
	Check(ctx context.Context, in Input) (Report, error)
	Write(ctx context.Context, in Input) (Report, error)
}

// RunnerUser is a rewriter that can only work with Input.Run: its app keeps a
// format only the app's own image may write. Apply asks before it stages
// anything, so a run that cannot start images refuses up front instead of
// stopping halfway through a Check, and apply never has to name the app.
type RunnerUser interface {
	NeedsRunner() bool
}

var registry = map[string]Rewriter{}

// Register files a rewriter under a catalog app name ("sonarr", not
// "sonarr-4k"): every instance of an app shares one. Called from init in each
// rewriter's package; a second registration for one app is a programming
// error.
func Register(app string, r Rewriter) {
	if _, dup := registry[app]; dup {
		panic("rewrite: second rewriter registered for " + app)
	}
	registry[app] = r
}

// For returns the rewriter for a catalog app. An app with no rewriter can't be
// migrated: every app's stored paths are rewritten, with no exceptions, so
// apply refuses rather than copy a config it can't read.
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
