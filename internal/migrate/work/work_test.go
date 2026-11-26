package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewIsBesideTheSnapshotAndGoneAfter(t *testing.T) {
	dir := t.TempDir()
	got, done, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(got, dir+string(os.PathSeparator)) {
		t.Errorf("the working directory is %s, want one under %s", got, dir)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("the working directory does not exist: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the working directory is %04o, want 0700", perm)
	}
	if err := os.WriteFile(filepath.Join(got, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	done()
	if _, err := os.Lstat(got); !os.IsNotExist(err) {
		t.Errorf("the working directory is still there after the run ended: %v", err)
	}
}

func TestNewTheSystemTemporaryDirectoryIsNeverUsed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory it cannot write, so the read-only $TMPDIR would prove nothing")
	}
	readOnly := filepath.Join(t.TempDir(), "no-tmp")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", readOnly)
	dir := t.TempDir()
	got, done, err := New(dir)
	if err != nil {
		t.Fatalf("New with a read-only $TMPDIR: %v", err)
	}
	defer done()
	if strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("the working directory %s is in the system's temporary directory", got)
	}
}

func TestNewMakesTheSnapshotsDirectory(t *testing.T) {
	under := filepath.Join(t.TempDir(), "migrate")
	got, done, err := New(under)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer done()
	info, err := os.Stat(under)
	if err != nil {
		t.Fatalf("the snapshot directory was not made: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the snapshot directory is %04o, want 0700: it holds a host's snapshots", perm)
	}
	if filepath.Dir(got) != under {
		t.Errorf("the working directory is %s, want one directly under %s", got, under)
	}
}

func TestNewRefusesWhatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(file); err == nil {
		t.Error("New accepted a file as the snapshot's directory")
	}
	if _, _, err := New(""); err == nil {
		t.Error("New accepted no directory at all")
	}
}
