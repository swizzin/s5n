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

// Runner runs argv in the pinned image of an app with the staged config
// mounted at /config, and returns its stdout.
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
