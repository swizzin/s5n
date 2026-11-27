package apply

// stage_swap_test.go proves what a config copy cannot be made to do: a file of the
// copy, or a directory of it, swapped for a link while it is written leaves the
// link's target -- a path outside staging -- untouched, and the copy refuses by name.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The copy owns each file through the descriptor it is already writing, so a file
// swapped for a link to one outside the copy after it was written is not owned, its
// mode is not set and its time is not touched at the link's target.
func TestCopyDoesNotOwnAFileSwappedForALink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "settings.conf"), "mine\n", 0o600)
	dst := filepath.Join(t.TempDir(), "copy")
	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.conf")
	writeFile(t, target, "not mine\n", 0o600)
	at := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(target, at, at); err != nil {
		t.Fatal(err)
	}
	beforeHand = func(file string) error {
		if filepath.Base(file) != "settings.conf" {
			return nil
		}
		if err := os.Remove(file); err != nil {
			return err
		}
		return os.Symlink(target, file)
	}
	t.Cleanup(func() { beforeHand = func(string) error { return nil } })

	if _, err := copyTree(dst, src, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("the copy finished with its own file switched to a link out of it")
	} else if !strings.Contains(err.Error(), "settings.conf") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s, outside the copy, is %04o, want the 0600 it had", target, info.Mode().Perm())
	}
	if !info.ModTime().Equal(at) {
		t.Errorf("%s, outside the copy, was modified %s, want %s", target, info.ModTime(), at)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "not mine\n" {
		t.Errorf("%s, outside the copy, holds %q (%v)", target, got, err)
	}
}

// The directories are owned, given their modes and times deepest first, once nothing
// is written into them, and the copy's own directory last: a directory swapped for a
// link out of the copy is a refusal naming it, not the link's target read-only or
// given the source's time.
func TestCopyDoesNotGiveADirectorySwappedForALink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "sub", "keep.conf"), "mine\n", 0o600)
	if err := os.Chmod(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	outside := t.TempDir()
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chmod(outside, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(outside, at, at); err != nil {
		t.Fatal(err)
	}
	beforeDirs = func(dir string) error {
		if dir != dst {
			return nil
		}
		if err := os.Rename(filepath.Join(dst, "sub"), filepath.Join(dst, "sub.real")); err != nil {
			return err
		}
		return os.Symlink(outside, filepath.Join(dst, "sub"))
	}
	t.Cleanup(func() { beforeDirs = func(string) error { return nil } })

	if _, err := copyTree(dst, src, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("the copy finished with a directory switched to a link out of it")
	} else if !strings.Contains(err.Error(), "sub") {
		t.Errorf("the refusal does not name the directory: %v", err)
	}
	info, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o701 {
		t.Errorf("%s, outside the copy, is %04o: the copy set the source's mode on the link's target", outside, info.Mode().Perm())
	}
	if !info.ModTime().Equal(at) {
		t.Errorf("%s, outside the copy, was modified %s, want %s", outside, info.ModTime(), at)
	}
}
