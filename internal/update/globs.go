package update

import (
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/release"
)

// ChartGlobs is one release's catalog: per app, the config globs an upgrade can
// rewrite. An app with none is moved without a snapshot and cannot be rolled
// back. The chart is untrusted input to a root process, so internal/catalog
// parses it, not this function.
func ChartGlobs(chart release.Chart) (map[string][]string, error) {
	if len(chart.Catalog) == 0 {
		return nil, nil
	}
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string)
	for _, app := range cat.Apps() {
		if len(app.Snapshot) > 0 {
			out[app.Name] = app.Snapshot
		}
	}
	return out, nil
}
