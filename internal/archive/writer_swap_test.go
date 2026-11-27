package archive

// writer_swap_test.go proves the same for a tree being read into an archive: a file
// or a directory swapped for a link while it is walked is not what the archive
// carries.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file of a tree being added, swapped for a link to one outside it after the walk
// listed it, is not what the archive carries: the descriptor box reads has to be the
// file it checked.
func TestAddTreeDoesNotCarryAFileSwappedForALink(t *testing.T) {
	const mine, secret = "mine: 1\n", "hidden: 2\n"
	src := t.TempDir()
	settings := filepath.Join(src, "settings.conf")
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.conf")
	if err := os.WriteFile(target, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(target, at, at); err != nil {
		t.Fatal(err)
	}
	beforeRead = func(file string) error {
		if filepath.Base(file) != "settings.conf" {
			return nil
		}
		if err := os.Remove(file); err != nil {
			return err
		}
		return os.Symlink(target, file)
	}
	t.Cleanup(func() { beforeRead = func(string) error { return nil } })
	path := filepath.Join(t.TempDir(), "swap.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/app", src); err == nil {
		t.Fatal("the archive carries a file that was a link to something else by the time it was read")
	} else if !strings.Contains(err.Error(), "settings.conf") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused add left %s behind: %v", path, err)
	}
	assertOutsideUntouched(t, target, secret, at)
}

// A directory of a tree being added, swapped for a link to one outside it after the
// walk listed it, is not walked into: the files under it are read from the tree, not
// from the directory the link names -- which holds a file of the same name, since
// that is the swap a by-path read cannot see.
func TestAddTreeDoesNotCarryADirectorySwappedWhileItWalks(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"one.conf": "one\n", "two.conf": "two\n"} {
		if err := os.WriteFile(filepath.Join(src, "a", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	at := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	for name := range map[string]string{"one.conf": "", "two.conf": ""} {
		target := filepath.Join(outside, name)
		if err := os.WriteFile(target, []byte("out\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(target, at, at); err != nil {
			t.Fatal(err)
		}
	}
	beforeRead = func(file string) error {
		if filepath.Base(file) != "one.conf" {
			return nil
		}
		if err := os.Rename(filepath.Join(src, "a"), filepath.Join(src, "a.real")); err != nil {
			return err
		}
		return os.Symlink(outside, filepath.Join(src, "a"))
	}
	t.Cleanup(func() { beforeRead = func(string) error { return nil } })
	path := filepath.Join(t.TempDir(), "swap.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/app", src); err == nil {
		t.Fatal("the archive carries a directory that was a link out of the tree by the time it was walked")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused add left %s behind: %v", path, err)
	}
	for _, name := range []string{"one.conf", "two.conf"} {
		assertOutsideUntouched(t, filepath.Join(outside, name), "out\n", at)
	}
}

// assertOutsideUntouched is the whole promise of a swap test: the file the link
// pointed at keeps its bytes, its mode and its time.
func assertOutsideUntouched(t *testing.T, path, body string, at time.Time) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s, outside the tree, is %04o, want the 0600 it had", path, info.Mode().Perm())
	}
	if !info.ModTime().Equal(at) {
		t.Errorf("%s, outside the tree, was modified %s, want %s", path, info.ModTime(), at)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != body {
		t.Errorf("%s, outside the tree, holds %q (%v), want %q", path, got, err, body)
	}
}
