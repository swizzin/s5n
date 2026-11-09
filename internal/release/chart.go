package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"

	"github.com/swizzin/s5n/internal/catalog"
)

// Chart is one chart release box is about to install: the files it checks a
// seedbox against, the annotations that say which box may install it, and the
// artifact Helm installs.
//
// Which files matter is the chart's business, not the caller's: a chart release
// is read once, and apply's refusal (a schema violation, a grant key in values)
// and init's questions both come from this one value.
type Chart struct {
	// Ref is where the release came from, for box's own messages:
	// oci://ghcr.io/swizzin/charts/s5n:0.3.0, or a chart directory.
	Ref string
	// Name is Chart.yaml's name, which is also the chart repository's last
	// path element.
	Name string
	// Version and AppVersion are Chart.yaml's. A published release has both
	// equal to the tag it was published under (docs/box.md, "Where a release
	// lives"); box refuses a chart that disagrees with the version it asked
	// for.
	Version    string
	AppVersion string
	// Annotations is Chart.yaml's annotation map. box reads
	// box.s5n/min-box from it (minbox.go); box.s5n/upgrade-from is carried
	// for `box update`, which steps through intermediate releases and does not
	// exist yet.
	Annotations map[string]string
	// Values and Schema are the two files box merges a seedbox into and
	// checks it against.
	Values []byte
	Schema []byte
	// Catalog and Variants are the chart-owned manifests: init's questions
	// (internal/catalog) and the blessed image list (internal/admission). Nil
	// when the chart does not ship them.
	Catalog  []byte
	Variants []byte
	// Dir is the chart directory, when the chart came from a checkout.
	Dir string
	// Archive is the packaged chart (.tgz), when it came from a registry.
	// Upgrade installs one or the other, never both.
	Archive []byte
}

// ChartRef says which chart release to load. Dir names a local checkout and is
// the test-only --chart path; otherwise Repo and Version name a published
// release, with an empty Version meaning the newest release this box can
// install.
type ChartRef struct {
	Dir     string
	Repo    string
	Version string
}

// ChartSource loads a chart release. It is an interface for the same reason
// ReleaseClient is: apply's refusals have to be provable without a registry.
type ChartSource interface {
	Load(ctx context.Context, ref ChartRef) (Chart, error)
}

// MaxChartFile bounds one file box reads out of a chart. The real values.yaml
// is a few KB; the cap is here because a pulled chart is untrusted input to a
// process that may run as root, and "read whatever this file says" needs a
// limit. The catalog has its own, smaller cap (catalog.MaxManifest).
const MaxChartFile = 1 << 20

// DirChart reads a chart from a local directory.
//
// It is box's test path: box only uses published charts (docs/box.md), and
// --chart is hidden from help. It stays until the e2e pulls its chart from a
// local registry; a later commit removes it, with the flag. The min-box rule
// applies here too: where a chart came from is not what decides whether this
// box may install it.
type DirChart struct {
	// BoxVersion is this box's version, as OCIChart's is.
	BoxVersion string
	// Log receives the "built without a version" notice. A command loads one
	// chart, so saying it here is saying it once.
	Log io.Writer
}

// Load reads the chart directory's files through Helm's own loader, so a chart
// read here and the same chart pulled from a registry produce the same Chart.
func (d DirChart) Load(_ context.Context, ref ChartRef) (Chart, error) {
	if IsDevVersion(d.BoxVersion) && d.Log != nil {
		fmt.Fprintf(d.Log, "box was built without a version (%s): skipping the chart's %s check\n", DevVersion, MinBoxAnnotation)
	}
	if ref.Dir == "" {
		return Chart{}, errors.New("no chart: pass --chart <dir> for a local checkout, or --chart-version for a published release")
	}
	ch, err := loader.LoadDir(ref.Dir)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref.Dir, err)
	}
	chart, err := chartFrom(ref.Dir, ch)
	if err != nil {
		return Chart{}, err
	}
	if err := CheckMinBox(d.BoxVersion, chart); err != nil {
		return Chart{}, err
	}
	chart.Dir = ref.Dir
	return chart, nil
}

// chartFrom builds box's view of a chart Helm has already loaded. Both sources
// go through it: the same chart must not mean two different things depending on
// where it came from.
func chartFrom(ref string, ch *chartv2.Chart) (Chart, error) {
	if ch.Metadata == nil {
		return Chart{}, fmt.Errorf("chart %s: no Chart.yaml", ref)
	}
	values, err := chartFile(ch, "values.yaml", MaxChartFile)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref, err)
	}
	if len(values) == 0 {
		return Chart{}, fmt.Errorf("chart %s: no values.yaml", ref)
	}
	schema, err := chartFile(ch, "values.schema.json", MaxChartFile)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref, err)
	}
	if len(schema) == 0 {
		return Chart{}, fmt.Errorf("chart %s: no values.schema.json; box refuses to apply values nothing has checked", ref)
	}
	catalogFile, err := chartFile(ch, catalog.FileName, catalog.MaxManifest)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref, err)
	}
	variants, err := chartFile(ch, "variants.yaml", MaxChartFile)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref, err)
	}
	return Chart{
		Ref:         ref,
		Name:        ch.Metadata.Name,
		Version:     ch.Metadata.Version,
		AppVersion:  ch.Metadata.AppVersion,
		Annotations: ch.Metadata.Annotations,
		Values:      values,
		Schema:      schema,
		Catalog:     catalogFile,
		Variants:    variants,
	}, nil
}

// chartFile returns one file from a loaded chart, or nil when the chart does
// not ship it. Helm keeps every file of the archive in Raw, so one call serves
// a checkout and a pulled artifact.
func chartFile(ch *chartv2.Chart, name string, limit int) ([]byte, error) {
	for _, f := range ch.Raw {
		if f.Name != name {
			continue
		}
		if len(f.Data) > limit {
			return nil, fmt.Errorf("%s is %d bytes; box reads at most %d", name, len(f.Data), limit)
		}
		return f.Data, nil
	}
	return nil, nil
}

// open loads the chart the way Helm's install and upgrade want it: the archive
// for a pulled release, the directory for a checkout.
func (c Chart) open() (*chartv2.Chart, error) {
	if len(c.Archive) > 0 {
		return loader.LoadArchive(bytes.NewReader(c.Archive))
	}
	if c.Dir == "" {
		return nil, fmt.Errorf("chart %s: nothing to load", c.Ref)
	}
	return loader.Load(c.Dir)
}
