package release

import (
	"fmt"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// loadChartDir reads a chart checkout for tests; box itself loads only published
// charts, and the round-trip test compares a registry's bytes with these.
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

// testChart is the fixture chart apply's tests run against.
func testChart(t *testing.T) Chart {
	t.Helper()
	c, err := loadChartDir(DevVersion, "testdata/chart")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testChartPtr is testChart as Options.Chart takes it.
func testChartPtr(t *testing.T) *Chart {
	c := testChart(t)
	return &c
}
