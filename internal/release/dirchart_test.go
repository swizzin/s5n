package release

import (
	"fmt"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// loadChartDir reads a chart checkout for a test. box itself only loads
// published charts (docs/box.md, "box only uses published charts"), so this
// fixture is the one place a directory is read: the round-trip test compares
// what a registry serves with these bytes, and apply's fakes need a real chart
// without a registry.
func loadChartDir(boxVersion, dir string) (Chart, error) {
	ch, err := loader.LoadDir(dir)
	if err != nil {
		return Chart{}, fmt.Errorf("chart %s: %w", dir, err)
	}
	chart, err := chartFrom(dir, ch)
	if err != nil {
		return Chart{}, err
	}
	if err := CheckMinBox(boxVersion, chart); err != nil {
		return Chart{}, err
	}
	return chart, nil
}

// testChart is release/testdata/chart, the three-key slice of the real chart
// apply's tests run against.
func testChart(t *testing.T) Chart {
	t.Helper()
	c, err := loadChartDir(DevVersion, "testdata/chart")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testChartPtr is the same value as Options.Chart takes it.
func testChartPtr(t *testing.T) *Chart {
	c := testChart(t)
	return &c
}
