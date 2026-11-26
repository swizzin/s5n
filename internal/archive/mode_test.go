package archive

// mode_test.go proves that a config tree comes out of an archive with the modes
// and times it went in with. A config directory is the app's, and some of what
// it keeps is meant to run: a torrent client's on-complete script, a helper an
// Arr's custom script calls. Writing every file 0600 took the execute bit off
// each of them.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// modeTree is a config directory with a script, a group-readable file and a
// group-readable directory in it, every one with a time of its own.
func modeTree(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	for rel, mode := range map[string]os.FileMode{
		"scripts/on-complete.sh": 0o755,
		"settings.conf":          0o640,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		// The process umask is not the source's: the modes are set outright.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"scripts/on-complete.sh", "settings.conf", "scripts", "."} {
		if err := os.Chtimes(filepath.Join(dir, rel), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o751); err != nil {
		t.Fatal(err)
	}
	return dir, at
}

func modeArchive(t *testing.T) (string, time.Time) {
	t.Helper()
	tree, at := modeTree(t)
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if _, err := w.AddTree("config/app", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, at
}

func checkModes(t *testing.T, root string, want map[string]os.FileMode, at time.Time) {
	t.Helper()
	for rel, mode := range want {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s is %04o, want the source's %04o", rel, info.Mode().Perm(), mode)
		}
		if !info.ModTime().Equal(at) {
			t.Errorf("%s was modified %s, want the source's %s", rel, info.ModTime(), at)
		}
	}
}

// TestOpenWritesAConfigTreeWithItsSourceModes: the extraction a run reads and
// copies from holds the config tree as the source had it. What keeps it private
// is the extraction's own directory, which stays box's and 0700, and the files
// box wrote for itself, which stay 0600.
func TestOpenWritesAConfigTreeWithItsSourceModes(t *testing.T) {
	path, at := modeArchive(t)
	dir := filepath.Join(t.TempDir(), "extract")
	if _, err := Open(path, dir); err != nil {
		t.Fatalf("Open: %v", err)
	}
	checkModes(t, filepath.Join(dir, "config", "app"), map[string]os.FileMode{
		"scripts/on-complete.sh": 0o755,
		"settings.conf":          0o640,
		"scripts":                0o750,
		".":                      0o751,
	}, at)
	for rel, mode := range map[string]os.FileMode{".": 0o700, "config": 0o700, PlanName: 0o600} {
		info, err := os.Lstat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s is %04o, want %04o: it is box's own", rel, info.Mode().Perm(), mode)
		}
	}
}

// TestExtractTreeWritesTheSourceModes: the staged copy a full carry writes is
// the instance's config directory itself, so its modes are the source's, the
// directory's own included.
func TestExtractTreeWritesTheSourceModes(t *testing.T) {
	path, at := modeArchive(t)
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "app")
	if err := ExtractTree(path, dst, "config/app", m, &Owner{UID: os.Geteuid(), GID: os.Getegid()}); err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}
	checkModes(t, dst, map[string]os.FileMode{
		"scripts/on-complete.sh": 0o755,
		"settings.conf":          0o640,
		"scripts":                0o750,
		".":                      0o751,
	}, at)
}
