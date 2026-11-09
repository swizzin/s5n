package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckMinBox is the whole rule as a table: a chart declares the oldest box
// that may install it, and box refuses below that, before any write. "dev" is
// not a version, so a box built without one skips the check rather than
// pretending to meet a floor.
func TestCheckMinBox(t *testing.T) {
	chart := func(minBox string) Chart {
		c := Chart{Name: "s5n", Version: "0.4.0"}
		if minBox != "" {
			c.Annotations = map[string]string{MinBoxAnnotation: minBox}
		}
		return c
	}
	tests := []struct {
		name       string
		boxVersion string
		minBox     string
		wantErr    []string
	}{
		{name: "equal is enough", boxVersion: "0.4.0", minBox: "0.4.0"},
		{name: "newer box installs", boxVersion: "0.5.0", minBox: "0.4.0"},
		{name: "newer minor", boxVersion: "1.0.0", minBox: "0.4.0"},
		{name: "older box refuses", boxVersion: "0.3.9", minBox: "0.4.0",
			wantErr: []string{"needs box 0.4.0", "this is box 0.3.9"}},
		{name: "a prerelease is older than its release", boxVersion: "0.4.0-rc.1", minBox: "0.4.0",
			wantErr: []string{"needs box 0.4.0", "this is box 0.4.0-rc.1"}},
		{name: "no annotation has no floor", boxVersion: "0.1.0"},
		{name: "dev skips a floor it cannot meet", boxVersion: DevVersion, minBox: "9.9.9"},
		{name: "an empty version is a dev build", boxVersion: "", minBox: "9.9.9"},
		{name: "a floor that is not a version is refused", boxVersion: "0.4.0", minBox: "latest",
			wantErr: []string{MinBoxAnnotation, "not a version"}},
		{name: "a box version that is not a version is refused", boxVersion: "git-describe", minBox: "0.1.0",
			wantErr: []string{"box version", "not a version"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMinBox(tc.boxVersion, chart(tc.minBox))
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckMinBox(%q, min-box %q) = %v, want nil", tc.boxVersion, tc.minBox, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckMinBox(%q, min-box %q) allowed the release", tc.boxVersion, tc.minBox)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestACheckoutIsNotExemptFromTheFloor: where a chart came from does not
// decide whether this box may install it. The fixture loader refuses for the
// same reason the registry path does.
func TestACheckoutIsNotExemptFromTheFloor(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("..", "..", "charts", "s5n")
	for _, name := range []string{"Chart.yaml", "values.yaml", "values.schema.json"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(`apiVersion: v2
name: s5n
type: application
version: 0.4.0
appVersion: "0.4.0"
annotations:
  box.s5n/min-box: "0.5.0"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadChartDir("0.4.0", dir)
	if err == nil || !strings.Contains(err.Error(), "needs box 0.5.0") {
		t.Fatalf("error = %v, want the floor refusal", err)
	}
}
