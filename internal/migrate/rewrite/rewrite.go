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
	"sort"
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
	// Run executes a tool inside the app's pinned image with Dir mounted at
	// /config, for formats only the app's own binary may write (Plex's
	// database). Nil when the caller can't run images; a rewriter that needs
	// it returns an error saying so.
	Run Runner
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

// normalizeHost is the form two stored hosts are compared in: no trailing dot,
// no IPv6 brackets, lowercase. A host is a DNS name here, and DNS is
// case-insensitive.
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
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
	// Notes are things the operator should know that stop nothing: a table
	// emptied on purpose, an import list's remote filter left alone.
	Notes []string
	// Owned are the connections this app holds to another app that the
	// rewrite pointed at the box. Wiring adopts them by ID on its first run
	// instead of creating a second one beside each (docs/box.md, "Migrated
	// connections are adopted by ID").
	Owned []Owned
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
