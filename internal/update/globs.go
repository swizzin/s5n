package update

import (
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/release"
)

// ChartGlobs is what one release's catalog says an image upgrade can rewrite:
// per app, the globs under its config directory that a migration moves forward
// (a database and the settings file that points at it). An app the catalog
// gives none has nothing to copy, so a step moves it without a snapshot and box
// will not roll it back.
//
// The chart is untrusted input to a process that may run as root, so the
// catalog is read through internal/catalog -- which caps the file, refuses a
// field it does not know and checks every glob against the chart's own values
// and schema -- rather than by taking the bytes apart here.
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
