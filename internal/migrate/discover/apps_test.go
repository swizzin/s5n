package discover

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestTableMatchesTheChart is what keeps discovery's app table honest. The table
// is a compile-time copy of two chart files -- the catalog's app list, and the
// config mount each app's pod carries -- because discovery reads a source
// install, where no chart release is present to ask. A chart that adds an app,
// renames one or moves an app's config must fail here, in the commit that does
// it, rather than migrate the wrong directory on someone's box.
func TestTableMatchesTheChart(t *testing.T) {
	chart := filepath.Join("..", "..", "..", "charts", "s5n")
	var catalog struct {
		Apps map[string]struct {
			Instances bool `yaml:"instances"`
		} `yaml:"apps"`
	}
	raw, err := os.ReadFile(filepath.Join(chart, "catalog.yaml"))
	if err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if err := yaml.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	if len(catalog.Apps) == 0 {
		t.Fatal("the catalog names no apps")
	}
	workloads, err := os.ReadFile(filepath.Join(chart, "templates", "workloads.yaml"))
	if err != nil {
		t.Fatalf("read the workloads: %v", err)
	}

	var apps []string
	for name := range catalog.Apps {
		apps = append(apps, name)
	}
	sort.Strings(apps)
	for _, name := range apps {
		info, ok := catalogApps[name]
		if !ok {
			t.Errorf("%s is in the chart's catalog and discovery does not know it, so a source running it is not migrated", name)
			continue
		}
		if info.instances != catalog.Apps[name].Instances {
			t.Errorf("%s: discovery says instances %v, the catalog says %v", name, info.instances, catalog.Apps[name].Instances)
		}
		if len(info.config) == 0 {
			t.Errorf("%s: discovery knows no config directory for it", name)
			continue
		}
		// The chart's own mount is the first candidate: the others are the
		// upstream image's, for a source that ran that image unmodified.
		mount := "mountPath: " + info.config[0] + "\n"
		if !strings.Contains(string(workloads), mount) {
			t.Errorf("%s: discovery reads its config from %s, which the chart does not mount", name, info.config[0])
		}
	}
	for name := range catalogApps {
		if _, ok := catalog.Apps[name]; !ok {
			t.Errorf("%s is in discovery's table and not in the chart's catalog", name)
		}
	}
}
