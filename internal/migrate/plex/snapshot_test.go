package plex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// snapshotTree writes a Plex config directory as the image makes one: state, the
// library, and the caches.
func snapshotTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, filepath.FromSlash(supportDir))
	for rel, body := range map[string]string{
		prefsName:                                    "<Preferences/>\n",
		dbRel:                                        "sqlite\n",
		dbRel + "-wal":                               "wal\n",
		blobsRel:                                     "sqlite\n",
		"Metadata/Posters/poster.jpg":                "jpeg\n",
		"Media/localhost/bif/index.bif":              "bif\n",
		"Plug-in Support/Caches/c.bin":               "cache\n",
		"Cache/PhotoTranscoder/thumb.jpg":            "jpeg\n",
		"Logs/Plex Media Server.log":                 "log\n",
		"Crash Reports/crash.dmp":                    "dmp\n",
		"Updates/update.bin":                         "update\n",
		"Codecs/lib.dll":                             "codec\n",
		"Diagnostics/health.txt":                     "diag\n",
		"Plug-in Support/Databases/schema-notes":     "notes\n",
		"Plug-in Support/Data/agent/preferences.ini": "ini\n",
	} {
		full := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSizeIsTheTreeAFullCarryWouldTake: the tree minus the caches.
func TestSizeIsTheTreeAFullCarryWouldTake(t *testing.T) {
	dir := snapshotTree(t)
	got, err := Size(dir)
	if err != nil {
		t.Fatalf("size %s: %v", dir, err)
	}
	// What a full carry writes: prefs, databases and -wal, artwork, .bif
	// thumbnails, agent data and schema notes.
	want := int64(len("<Preferences/>\n") + len("sqlite\n")*2 + len("wal\n") +
		len("jpeg\n") + len("bif\n") + len("notes\n") + len("ini\n"))
	if got != want {
		t.Errorf("a full carry of the tree is %d bytes, want %d: the caches, codecs, logs and crash reports are not part of it", got, want)
	}
}

// TestSizeOfADirectoryPlexIsNotIn: no Plex home is a refusal, not a size of zero.
func TestSizeOfADirectoryPlexIsNotIn(t *testing.T) {
	if _, err := Size(t.TempDir()); err == nil {
		t.Fatal("sizing a directory that holds no Plex home was answered")
	}
}

// TestSnapshotNamesWhatIsDisposableAndWhy: the tree less what Plex rebuilds, and
// the state with its sentence.
func TestSnapshotNamesWhatIsDisposableAndWhy(t *testing.T) {
	full := Snapshot(true)
	if !full.Full || len(full.Files) != 0 || len(full.Skip) == 0 {
		t.Fatalf("a full carry is %+v, want the tree and what it leaves out", full)
	}
	if !reflect.DeepEqual(full.Skip, disposable) {
		t.Errorf("a full carry leaves out\n  %v\nwant\n  %v", full.Skip, disposable)
	}
	if full.Note == Snapshot(false).Note || full.Summary == Snapshot(false).Summary {
		t.Error("the two scopes are described the same way")
	}
}
