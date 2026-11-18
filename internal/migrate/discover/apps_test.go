package discover

import (
	"os"
	"path/filepath"
	"reflect"
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
		for _, dir := range info.scratch {
			if !strings.Contains(string(workloads), "mountPath: "+dir+"\n") {
				t.Errorf("%s: discovery maps %s onto itself as the chart's scratch directory, which the chart does not mount", name, dir)
			}
		}
	}
	for name := range catalogApps {
		if _, ok := catalog.Apps[name]; !ok {
			t.Errorf("%s is in discovery's table and not in the chart's catalog", name)
		}
	}
}

// TestSnapshotCarryIsPlexsStateAndNothingElse: a migration copies every planned
// app's config whole, except Plex's, whose config directory is mostly a library
// of covers and thumbnails it rebuilds. What Plex's snapshot carries is what it
// cannot rebuild: Preferences.xml, which holds its own settings, and the library
// databases -- and the two of those are state a snapshot without refuses.
// Nothing else in the table trims a tree.
func TestSnapshotCarryIsPlexsStateAndNothingElse(t *testing.T) {
	carry, ok := SnapshotCarry("plex")
	if !ok {
		t.Fatal("Plex's snapshot is its whole config directory: on a real library that is tens of gigabytes of metadata")
	}
	want := []CarryFile{
		{Path: "Library/Application Support/Plex Media Server/Preferences.xml", Required: true},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db", Required: true},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db-wal"},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db-shm"},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db"},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-wal"},
		{Path: "Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-shm"},
	}
	if !reflect.DeepEqual(carry.Files, want) {
		t.Errorf("Plex's snapshot carries\n  %v\nwant\n  %v", carry.Files, want)
	}
	if !strings.HasPrefix(carry.Note, "plex: carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)") {
		t.Errorf("the line about what Plex's snapshot leaves behind is %q", carry.Note)
	}
	for _, dir := range []string{"Metadata", "Cache"} {
		for _, f := range carry.Files {
			if strings.Contains(f.Path, dir) {
				t.Errorf("Plex's snapshot carries %s, and Plex rebuilds it", f.Path)
			}
		}
	}

	// Every other app's config is its state: an archive that trimmed one would
	// be one apply could not use.
	for app := range catalogApps {
		if app == "plex" {
			continue
		}
		if _, ok := SnapshotCarry(app); ok {
			t.Errorf("%s's snapshot is trimmed to a file list, and its config is its state", app)
		}
	}
	// An instance is its app's entry, so the list is not lost when an app that
	// takes several grows one.
	if _, ok := SnapshotCarry("sonarr-4k"); ok {
		t.Error("sonarr-4k carries a trimmed list, and sonarr does not")
	}
}
