package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rehearsal reads a running source, and a running app rewrites its cookie jar
// whenever it likes: the size a walk saw is not the size the read finds. A small
// file is entered as what was read, so the snapshot is not refused over a cookie.
func TestASmallFileThatChangedSinceItsStatIsEnteredAsRead(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "HTTPCookies")
	if err := os.WriteFile(file, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	grown := []byte(strings.Repeat("cookie;", 200))
	if err := os.WriteFile(file, grown, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "live.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.addFileFrom("config/plex/HTTPCookies", file, stale); err != nil {
		t.Fatalf("a file that grew since its stat was refused: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := ReadEntry(path, "config/plex/HTTPCookies")
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if !bytes.Equal(got, grown) {
		t.Fatalf("the entry holds %d bytes, want the %d that were read", len(got), len(grown))
	}
	if _, err := Verify(path, nil); err != nil {
		t.Fatalf("the archive does not verify against its manifest: %v", err)
	}
}

// A large file that changed under the copy is still a refusal: it is a database,
// and a torn one is worse than none.
func TestALargeFileThatChangedSinceItsStatIsRefused(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "library.db")
	if err := os.WriteFile(file, bytes.Repeat([]byte{1}, smallFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte{1}, smallFile+4096), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Create(filepath.Join(t.TempDir(), "live.tar.gz"), testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.addFileFrom("config/plex/library.db", file, stale); err == nil {
		t.Fatal("a large file that changed under the copy was entered")
	}
}
