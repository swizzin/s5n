package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write replaces the file, ends at the mode the caller asked for, and leaves
// nothing of the temporary file behind.
func TestWriteReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte("wiring:\n  timeout: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("wiring:\n  timeout: 900\n"), 0o640); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "wiring:\n  timeout: 900\n" {
		t.Fatalf("file = %q, want the new bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
	assertNoLeftovers(t, path)
}

// A new file gets the caller's mode too: the record is 0600 and the app configs
// are 0600, and neither may be world-readable by the umask's accident.
func TestWriteCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seedbox.yaml")
	if err := Write(path, []byte("kubeconfig: /etc/rancher/k0s/k0s.yaml\n"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// A failed rename is the case this package exists for: the target still holds
// exactly its old bytes, and the temporary file is not left beside it.
func TestFailedRenameLeavesTheTargetWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.yaml")
	before := []byte("namespace: testbox\napps: [sonarr]\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	rename = func(_, _ string) error { return errors.New("simulated rename failure") }
	t.Cleanup(func() { rename = os.Rename })

	err := Write(path, []byte("namespace: testbox\napps: []\n"), 0o600)
	if err == nil {
		t.Fatal("Write reported success after the rename failed")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("the target changed after a failed rename: %q", after)
	}
	assertNoLeftovers(t, path)
}

// A missing parent directory is refused rather than silently written somewhere
// else: the callers that need one create it with their own mode.
func TestWriteRefusesAMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "values.yaml")
	if err := Write(path, []byte("x: 1\n"), 0o600); err == nil {
		t.Fatal("Write succeeded into a directory that is not there")
	}
}

// Copy is Write for a stream, and has to behave the same way: the content
// arrives, the file ends at the caller's mode, and a reader that fails partway
// leaves the target whole rather than truncated.
func TestCopyStreamsTheContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonarr.db")
	if err := os.WriteFile(path, []byte("the old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Copy(path, strings.NewReader("the restored database"), 0o640); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the restored database" {
		t.Fatalf("file = %q, want the new bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	assertNoLeftovers(t, path)
}

func TestFailedCopyLeavesTheTargetWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonarr.db")
	before := []byte("the old database")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Copy(path, &failingReader{after: 4}, 0o600)
	if err == nil {
		t.Fatal("Copy reported success after the reader failed")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the target changed after a failed copy: %q", after)
	}
	assertNoLeftovers(t, path)
}

// failingReader hands out a few bytes and then fails, which is the shape of a
// snapshot file disappearing under a restore.
type failingReader struct {
	after int
	sent  int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent >= r.after {
		return 0, errors.New("the snapshot file went away")
	}
	p[0] = 'x'
	r.sent++
	return 1, nil
}

// assertNoLeftovers is the temporary-file check every failure path needs: a
// refused write leaves nothing of the file it was making.
func assertNoLeftovers(t *testing.T, path string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

// openRoot opens dir as the tree WriteIn writes inside, closed with the test.
func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// WriteIn is Write for a target inside a tree, and writes exactly what Write
// writes: the new bytes, the caller's mode, and no temporary file left behind.
func TestWriteInReplacesTheFileInTheTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".box", "holds.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"holds":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}

	root := openRoot(t, dir)
	if err := WriteIn(root, ".box/holds.json", []byte(`{"holds":[{"app":"sonarr"}]}`), 0o640); err != nil {
		t.Fatalf("WriteIn: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"holds":[{"app":"sonarr"}]}` {
		t.Fatalf("file = %q, want the new bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	assertNoLeftovers(t, path)
}

// A missing directory is refused rather than created: the callers that need one
// create it with their own mode, exactly as they do for Write.
func TestWriteInRefusesAMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	if err := WriteIn(root, "missing/holds.json", []byte("x"), 0o600); err == nil {
		t.Fatal("WriteIn succeeded into a directory that is not there")
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("the refused write created a directory: %v", err)
	}
}

// A link where the target goes is replaced, never followed: the write lands on
// the name inside the tree, and the file the link pointed at is not touched.
// (Following it would put a snapshot restore's bytes, or a hold, in whichever
// file the seedbox's user chose.)
func TestWriteInReplacesALinkRatherThanFollowingIt(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(secret, []byte("someone else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "holds.json")); err != nil {
		t.Fatal(err)
	}

	root := openRoot(t, dir)
	if err := WriteIn(root, "holds.json", []byte(`{"holds":[]}`), 0o640); err != nil {
		t.Fatalf("WriteIn: %v", err)
	}
	body, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "someone else's file" {
		t.Fatalf("the link's target = %q, want it untouched", body)
	}
	info, err := os.Lstat(filepath.Join(dir, "holds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("holds.json = %s, want the write to have replaced the link", info.Mode())
	}
	assertNoLeftovers(t, filepath.Join(dir, "holds.json"))
}

// A directory that a link points out of the tree is a refusal, and nothing is
// created on either side: the root names the file it was given, and neither the
// temporary file nor the target exists afterwards.
func TestWriteInRefusesADirectoryThatLeavesTheTree(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	root := openRoot(t, dir)
	err := WriteIn(root, "escape/holds.json", []byte("x"), 0o640)
	if err == nil {
		t.Fatal("WriteIn wrote through a link out of the tree")
	}
	if !strings.Contains(err.Error(), "holds.json") {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused write left %d entries outside the tree", len(entries))
	}
}

// CopyIn is the stream form, and behaves the same way: the content arrives, the
// file ends at the caller's mode, and a reader that fails partway leaves the
// target whole rather than truncated.
func TestCopyInStreamsTheContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonarr.db")
	if err := os.WriteFile(path, []byte("the old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, dir)
	if err := CopyIn(root, "sonarr.db", strings.NewReader("the restored database"), 0o640); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the restored database" {
		t.Fatalf("file = %q, want the new bytes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	assertNoLeftovers(t, path)
}

// The failure that matters on a restore: a reader that dies mid-copy leaves the
// file it was replacing exactly as it was, and nothing of the copy behind.
func TestFailedCopyInLeavesTheTargetWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sonarr.db")
	before := []byte("the old database")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, dir)
	err := CopyIn(root, "sonarr.db", &failingReader{after: 4}, 0o600)
	if err == nil {
		t.Fatal("CopyIn reported success after the reader failed")
	}
	if !strings.Contains(err.Error(), "sonarr.db") {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the target changed after a failed copy: %q", after)
	}
	assertNoLeftovers(t, path)
}

// Create is the write for content the caller produces in pieces: nothing appears
// at the path until Commit, and then the whole file does.
func TestCreateCommitsAWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.tar.gz")
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write([]byte("first ")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if exists(t, path) {
		t.Fatal("the file appeared before it was committed")
	}
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first second" {
		t.Errorf("content = %q, want both writes", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	assertNoLeftovers(t, path)
}

func TestCreateRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.tar.gz")
	if err := os.WriteFile(path, []byte("the only copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path, 0o600); err == nil {
		t.Fatal("Create accepted a path that already exists")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "the only copy" {
		t.Fatalf("the existing file changed: %q (%v)", got, err)
	}
}

func TestDiscardLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.tar.gz")
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("half a snapshot")); err != nil {
		t.Fatal(err)
	}
	f.Discard()
	f.Discard()
	if exists(t, path) {
		t.Fatal("a discarded file left its name behind")
	}
	if err := f.Commit(); err == nil {
		t.Fatal("Commit after Discard was accepted")
	}
	assertNoLeftovers(t, path)
}

// exists is whether anything is at path.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}
