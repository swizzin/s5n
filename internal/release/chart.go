package release

import (
	"bytes"
	"context"
	"fmt"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"

	"github.com/swizzin/s5n/internal/catalog"
)

// Chart is one release: the files box checks a seedbox against, the annotations
// that say which box may install it, and the artifact Helm installs. Read once;
// apply's refusals and init's questions both come from this value.
type Chart struct {
	// Ref is where the release came from, for box's messages.
	Ref string
	// Name is Chart.yaml's name, also the repository's last path element.
	Name string
	// Version and AppVersion are equal to the tag for a published release; a
	// chart that disagrees with the requested version is refused.
	Version    string
	AppVersion string
	// Annotations carries box.s5n/min-box (minbox.go) and upgrade-from.
	Annotations map[string]string
	// Values and Schema are the files a seedbox is merged into and checked
	// against.
	Values []byte
	Schema []byte
	// Catalog and Variants are init's questions and the blessed image list.
	Catalog  []byte
	Variants []byte
	// Pins is platform.yaml; a release without it is refused as it loads, so
	// this is never zero.
	Pins Versions
	// Archive is the packaged chart (.tgz), when it came from a registry.
	Archive []byte
	// Digest is that archive's manifest digest in the registry: the digest its
	// release's signature covers, and the one box pulled. A directory chart has none.
	Digest string
}

// ChartRef says which release to load; empty Version is the newest this box
// can install, empty Repo is DefaultChartRepo.
type ChartRef struct {
	Repo    string
	Version string
}

// ChartSource loads a release, so apply's refusals are provable without a
// registry.
type ChartSource interface {
	Load(ctx context.Context, ref ChartRef) (Chart, error)
}

// MaxChartFile bounds one file read from a chart: a pulled chart is untrusted
// input to a root process. The catalog has a smaller cap (catalog.MaxManifest).
const MaxChartFile = 1 << 20

// chartFrom builds box's view of a chart; both sources go through it, so a
// chart cannot mean two things depending on where it came from.
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
	pinsFile, err := chartFile(ch, PlatformFile, MaxChartFile)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", ref, err)
	}
	// Refuse bad pins here, so bootstrap, init and apply all see one refusal
	// before any write.
	pins, err := platformPins(ch.Metadata.Name, ch.Metadata.Version, pinsFile)
	if err != nil {
		return Chart{}, err
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
		Pins:        pins,
	}, nil
}

// chartFile returns one file, or nil when absent; Helm keeps every file in Raw.
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

// open loads the packaged archive Helm's install and upgrade want.
func (c Chart) open() (*chartv2.Chart, error) {
	if len(c.Archive) == 0 {
		return nil, fmt.Errorf("chart %s: no archive to load", c.Ref)
	}
	return loader.LoadArchive(bytes.NewReader(c.Archive))
}
