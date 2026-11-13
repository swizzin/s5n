// Command appcalls prints the in-release call edges a seedbox release contains:
// one "caller callee" per line, for every pair the chart's catalog declares and
// this release has both ends of switched on.
//
// It exists so an e2e can prove those edges without keeping a second list of
// them: the chart derives its NetworkPolicy from the same `calls` entries, and a
// hand-typed copy in a test script is exactly the thing that drifts from the
// policy it is meant to check (a shipped release proved it by declaring an edge
// its ingress refused). The catalog is read through internal/catalog, so this
// prints what a validated manifest says, not what a YAML reader happens to find.
//
// Usage:
//
//	go run ./hack/appcalls [chart dir] <enabled app>...
//
// The chart dir defaults to charts/s5n, relative to the repository root, which
// is where the e2e scripts run it from. The app names are the release's enabled
// apps -- an e2e reads them from the pods it installed, so nothing here names an
// app either.
package main

import (
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/catalog"
)

// defaultDir is the chart, relative to the repository root.
const defaultDir = "charts/s5n"

func main() {
	args := os.Args[1:]
	dir := defaultDir
	if len(args) > 0 && args[0] != "" {
		dir = args[0]
		args = args[1:]
	}
	cat, err := catalog.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "appcalls: %v\n", err)
		os.Exit(1)
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: appcalls [chart dir] <enabled app>...")
		os.Exit(2)
	}
	enabled := make(map[string]bool, len(args))
	for _, name := range args {
		enabled[name] = true
	}
	for _, edge := range pairs(cat, enabled) {
		fmt.Printf("%s %s\n", edge[0], edge[1])
	}
}

// pairs is the catalog's edges filtered to a release: both ends enabled, in
// catalog order and then in the order each entry lists its calls. An app that is
// not in the release contributes nothing, which is the same rule the chart's
// ingress follows -- an app that is switched off admits nothing and is admitted
// by nothing.
func pairs(cat *catalog.Catalog, enabled map[string]bool) [][2]string {
	var out [][2]string
	for _, app := range cat.Apps() {
		if !enabled[app.Name] {
			continue
		}
		for _, callee := range app.Calls {
			if enabled[callee] {
				out = append(out, [2]string{app.Name, callee})
			}
		}
	}
	return out
}
