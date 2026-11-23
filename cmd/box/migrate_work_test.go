package main

// migrate_work_test.go proves where an apply works: beside the snapshot it is
// reading, never in the system's temporary directory, which on a Fedora or an
// Arch host is a tmpfs -- and what it extracts out of a full Plex carry, which is
// the state its checks read and not the library they never do.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/say"
)

// plexFullSnapshot writes one final snapshot of a source whose Plex carried its
// whole data, and returns its path and the config directory it was taken from.
func plexFullSnapshot(t *testing.T) (path, tree string) {
	t.Helper()
	dir := t.TempDir()
	tree = filepath.Join(dir, "source", "plex")
	home := filepath.Join(tree, "Library", "Application Support", "Plex Media Server")
	for rel, body := range map[string]string{
		"Preferences.xml": `<Preferences MachineIdentifier="synthetic-machine"/>` + "\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db": "sqlite\n",
		"Metadata/Posters/poster.jpg":                              strings.Repeat("j", 8192),
		"Cache/PhotoTranscoder/thumb.jpg":                          strings.Repeat("c", 4096),
	} {
		at := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := []byte(`version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    carry: full
    paths:
      - from: /srv/apps/plex
        to: /config
`)
	scope := plex.Snapshot(true)
	path = filepath.Join(dir, bundle.FileName(archive.ModeFinal, time.Now()))
	if _, err := bundle.Write(path, bundle.Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   doc,
		Report: []byte("plan: a read of the source\n"),
		Configs: []bundle.Config{{
			Instance: "plex",
			Dir:      tree,
			Sub:      scope.Sub,
			Skip:     scope.Skip,
		}},
	}); err != nil {
		t.Fatalf("write the snapshot: %v", err)
	}
	return path, tree
}

// TestAnApplyWorksBesideItsSnapshotAndNeverInTmp: a read-only $TMPDIR does not
// stop the run, the extraction lands under the snapshot's own directory, it holds
// Plex's state and not its library, and the working directory is gone when the
// run ends.
func TestAnApplyWorksBesideItsSnapshotAndNeverInTmp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory it cannot write, so the read-only $TMPDIR would prove nothing")
	}
	path, _ := plexFullSnapshot(t)
	// Everything this test needs is made before $TMPDIR is taken away, because
	// testing.TempDir is a temporary directory as well.
	snapshotDir := filepath.Dir(path)
	readOnly := filepath.Join(t.TempDir(), "no-tmp")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", readOnly)

	var out bytes.Buffer
	opened, cleanup, err := openSnapshot(path, &say.Plain{W: &out})
	if err != nil {
		t.Fatalf("open the snapshot with a read-only $TMPDIR: %v", err)
	}
	work := filepath.Dir(opened.Dir)
	if filepath.Dir(work) != snapshotDir {
		t.Errorf("the run works in %s, want a directory directly under the snapshot's %s", work, snapshotDir)
	}
	if info, err := os.Stat(work); err != nil {
		t.Fatalf("the working directory does not exist: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the working directory is %04o, want 0700: it holds the source's configs", perm)
	}
	// What the checks read: Plex's state, under the path its image reads it from.
	home := filepath.Join(opened.Configs["plex"], "Library", "Application Support", "Plex Media Server")
	if _, err := os.Stat(filepath.Join(home, "Preferences.xml")); err != nil {
		t.Errorf("the extraction holds no Preferences.xml: %v", err)
	}
	for _, left := range []string{"Metadata", "Cache"} {
		if _, err := os.Stat(filepath.Join(home, left)); !os.IsNotExist(err) {
			t.Errorf("the extraction holds Plex's %s, and no check reads it: %v", left, err)
		}
	}
	c := opened.Contents["plex"]
	if !c.Tree {
		t.Error("the snapshot holds Plex's whole data, and the run says it extracted all of it")
	}
	if c.Files != 3 || c.Bytes == 0 {
		t.Errorf("the snapshot counts %d files and %d bytes of Plex's config, want the tree without its caches", c.Files, c.Bytes)
	}
	entries, err := os.ReadDir(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the run left %d entries in the system's temporary directory, want none", len(entries))
	}

	cleanup()
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("the working directory is still there after the run ended: %v", err)
	}
}

// TestAnApplyOfASnapshotThatCarriesTheStateKeepsAllOfIt: the plan says which carry
// the snapshot took, so a snapshot that carries Plex's state is extracted whole
// and has no tree to leave behind.
func TestAnApplyOfASnapshotThatCarriesTheStateKeepsAllOfIt(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "source", "plex")
	home := filepath.Join(tree, "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(filepath.Join(home, "Plug-in Support", "Databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"Preferences.xml": "<Preferences/>\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db": "sqlite\n",
	} {
		if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := []byte(`version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    carry: minimal
    paths:
      - from: /srv/apps/plex
        to: /config
`)
	var files []bundle.File
	for _, f := range plex.Snapshot(false).Files {
		files = append(files, bundle.File{Path: f.Path, Required: f.Required})
	}
	path := filepath.Join(dir, bundle.FileName(archive.ModeFinal, time.Now()))
	if _, err := bundle.Write(path, bundle.Snapshot{
		Mode:   archive.ModeFinal,
		Plan:   doc,
		Report: []byte("plan: a read of the source\n"),
		Configs: []bundle.Config{{
			Instance: "plex",
			Dir:      tree,
			Sub:      plex.Snapshot(false).Sub,
			Files:    files,
		}},
	}); err != nil {
		t.Fatalf("write the snapshot: %v", err)
	}
	var out bytes.Buffer
	opened, cleanup, err := openSnapshot(path, &say.Plain{W: &out})
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	defer cleanup()
	if c := opened.Contents["plex"]; c.Tree {
		t.Errorf("a snapshot that carried the state alone says the run left a tree behind: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(opened.Configs["plex"], "Library", "Application Support", "Plex Media Server", "Preferences.xml")); err != nil {
		t.Errorf("the extraction holds no Preferences.xml: %v", err)
	}
}
