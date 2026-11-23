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

// TestSnapshotCarryIsPlexsStateOrItsWholeData: a migration copies every planned
// app's config whole, except Plex's, whose config directory is mostly a library
// of covers, artwork, thumbnails and preview thumbnails it built from the media
// it was given. What Plex's snapshot carries is either its whole data -- less the
// caches and records it rebuilds or downloads again -- or the state it cannot
// rebuild: Preferences.xml and the library databases, the two of which a
// snapshot without refuses. Nothing else in the table has a scope of its own.
func TestSnapshotCarryIsPlexsStateOrItsWholeData(t *testing.T) {
	full, ok := SnapshotCarry("plex", true)
	if !ok {
		t.Fatal("Plex's snapshot is its whole config directory whatever box is told: a full carry is a decision, not the only answer")
	}
	state, ok := SnapshotCarry("plex", false)
	if !ok {
		t.Fatal("Plex has no state-only snapshot, and a library does not always fit")
	}
	checkFullCarry(t, full)
	checkStateCarry(t, state)

	// Every other app's config is its state: an archive that trimmed one would
	// be one apply could not use.
	for app := range catalogApps {
		if app == "plex" {
			continue
		}
		if _, ok := SnapshotCarry(app, true); ok {
			t.Errorf("%s's snapshot has a scope of its own, and its config is its state", app)
		}
	}
	// An instance is its app's entry, so the rule is not lost when an app that
	// takes several grows one.
	if _, ok := SnapshotCarry("sonarr-4k", true); ok {
		t.Error("sonarr-4k has a scope of its own, and sonarr does not")
	}
}

// checkFullCarry is the whole-data scope: the tree, its subtree, and the caches
// it leaves out.
func checkFullCarry(t *testing.T, full Carry) {
	t.Helper()
	if !full.Full {
		t.Error("a full carry of Plex is not the tree")
	}
	// Both lists are relative to the app's own home, which Sub says where is:
	// a config mount is not a library, and Plex's is under Library/Application
	// Support in the image.
	if full.Sub != "Library/Application Support/Plex Media Server" {
		t.Errorf("a full carry's subtree is %q, want Plex's home", full.Sub)
	}
	wantSkip := []string{
		"Cache",
		"Plug-in Support/Caches",
		"Codecs",
		"Logs",
		"Crash Reports",
		"Diagnostics",
		"Updates",
	}
	if !reflect.DeepEqual(full.Skip, wantSkip) {
		t.Errorf("a full carry leaves out\n  %v\nwant\n  %v", full.Skip, wantSkip)
	}
	if len(full.Files) != 0 {
		t.Errorf("a full carry names %d files as well as the tree", len(full.Files))
	}
	for _, kept := range []string{"Metadata", "Media", "Plug-in Support/Databases"} {
		for _, left := range full.Skip {
			if left == kept || strings.HasPrefix(left, kept+"/") {
				t.Errorf("a full carry leaves out %s, which is where the artwork and the preview thumbnails are", left)
			}
		}
	}
	if !strings.Contains(full.Note, "its whole data") || !strings.HasPrefix(full.Note, "plex: ") {
		t.Errorf("the line about a full Plex carry is %q", full.Note)
	}
	if !strings.Contains(full.Summary, "its whole data") {
		t.Errorf("the confirm's row for a full Plex carry is %q", full.Summary)
	}
}

// checkStateCarry is the state-only scope: the two files Plex cannot start
// without, the companions a writer leaves, and nothing of the library.
func checkStateCarry(t *testing.T, state Carry) {
	t.Helper()
	if state.Full {
		t.Fatalf("Plex's state-only snapshot is %+v", state)
	}
	want := []CarryFile{
		{Path: "Preferences.xml", Required: true},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.db", Required: true},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.db-wal"},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.db-shm"},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db"},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-wal"},
		{Path: "Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db-shm"},
	}
	if !reflect.DeepEqual(state.Files, want) {
		t.Errorf("Plex's state-only snapshot carries\n  %v\nwant\n  %v", state.Files, want)
	}
	if len(state.Skip) != 0 {
		t.Errorf("a state-only carry leaves %v out of a tree it does not carry", state.Skip)
	}
	if !strings.HasPrefix(state.Note, "plex: carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)") {
		t.Errorf("the line about what Plex's state-only snapshot leaves behind is %q", state.Note)
	}
	for _, dir := range []string{"Metadata", "Cache"} {
		for _, f := range state.Files {
			if strings.Contains(f.Path, dir) {
				t.Errorf("Plex's state-only snapshot carries %s, and Plex rebuilds it", f.Path)
			}
		}
	}
}
