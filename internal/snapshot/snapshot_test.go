package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// taken is the mtime every fixture file gets, so a restored mtime can be
// compared exactly: the filesystem stores whole seconds either way.
var taken = time.Unix(1700000000, 0).UTC()

// configDir builds an app's config directory with the state an Arr keeps: a
// database, its WAL, the file that points at the database, and a log directory
// that no glob names.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.xml"), "<Config/>", 0o640)
	write(t, filepath.Join(dir, "app.db"), "the database", 0o640)
	write(t, filepath.Join(dir, "app.db-wal"), "its wal", 0o600)
	write(t, filepath.Join(dir, "logs", "logs.db"), "a log database", 0o640)
	return dir
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, taken, taken); err != nil {
		t.Fatal(err)
	}
}

func globs() []string { return []string{"config.xml", "app.db", "app.db-wal", "app.db-shm"} }

// images are the pair one step knows about an app: what it ran when the copy
// was taken and what the step moves it to. The values are invented; none of
// these tests is about which image, only that the copy names both.
func images() Images {
	return Images{
		Ran:     ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14", Digest: "sha256:" + strings.Repeat("a", 64)},
		MovedTo: ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.15", Digest: "sha256:" + strings.Repeat("b", 64)},
	}
}

// nestedGlobs adds a database one directory down, where some apps keep theirs.
func nestedGlobs() []string { return append(globs(), "db/db.sqlite3", "db/db.sqlite3-wal") }

func TestTakeCopiesTheMatchingFiles(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")

	man, err := Take(dir, globs(), dest, images())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if man.App != filepath.Base(dir) {
		t.Fatalf("manifest app = %q, want %q", man.App, filepath.Base(dir))
	}
	if len(man.Files) != 3 {
		t.Fatalf("manifest files = %+v, want config.xml, app.db, app.db-wal", man.Files)
	}
	want := []string{"app.db", "app.db-wal", "config.xml"}
	for i, f := range man.Files {
		if f.Path != want[i] {
			t.Fatalf("files[%d].Path = %q, want %q (path order)", i, f.Path, want[i])
		}
		body, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s sha256 = %s, want the file's", f.Path, f.SHA256)
		}
		if f.Size != int64(len(body)) {
			t.Fatalf("%s size = %d, want %d", f.Path, f.Size, len(body))
		}
		if !f.ModTime.Equal(taken) {
			t.Fatalf("%s mtime = %s, want %s", f.Path, f.ModTime, taken)
		}
	}
	// The WAL keeps its mode and the config file its own: a restore has to be
	// able to put each back as the app had it.
	modes := map[string]os.FileMode{"config.xml": 0o640, "app.db": 0o640, "app.db-wal": 0o600}
	for _, f := range man.Files {
		if f.Mode != uint32(modes[f.Path]) {
			t.Fatalf("%s mode = %o, want %o", f.Path, f.Mode, modes[f.Path])
		}
		info, err := os.Lstat(filepath.Join(dest, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != modes[f.Path] {
			t.Fatalf("copied %s mode = %o, want %o", f.Path, info.Mode().Perm(), modes[f.Path])
		}
		if !info.ModTime().Equal(taken) {
			t.Fatalf("copied %s mtime = %s, want %s", f.Path, info.ModTime(), taken)
		}
	}
	// The log directory is not state a migration rewrites.
	if _, err := os.Lstat(filepath.Join(dest, "logs")); !os.IsNotExist(err) {
		t.Fatalf("the snapshot holds logs: %v", err)
	}
	// The tree is renamed, not built in place, so no partial is left behind.
	if _, err := os.Lstat(dest + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("a partial tree was left behind: %v", err)
	}
	// The manifest is readable as it was written: Restore depends on it.
	data, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var reread Manifest
	if err := json.Unmarshal(data, &reread); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(reread.Files) != len(man.Files) || reread.App != man.App {
		t.Fatalf("reread manifest = %+v, want %+v", reread, man)
	}
}

// TestTakeRecordsTheImages: the copy and the images that produced it belong
// together, because nothing else on the box remembers them once the release has
// moved. Both are read back from the file, not from the value Take returned.
func TestTakeRecordsTheImages(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")
	want := images()

	man, err := Take(dir, globs(), dest, want)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if man.Image != want.Ran || man.MovedTo != want.MovedTo {
		t.Fatalf("manifest images = %+v -> %+v, want %+v -> %+v", man.Image, man.MovedTo, want.Ran, want.MovedTo)
	}
	data, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var reread Manifest
	if err := json.Unmarshal(data, &reread); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if reread.Image != want.Ran {
		t.Fatalf("stored image = %+v, want %+v", reread.Image, want.Ran)
	}
	if reread.MovedTo != want.MovedTo {
		t.Fatalf("stored movedTo = %+v, want %+v", reread.MovedTo, want.MovedTo)
	}
	// The keys are always written, tag and digest included: a rollback reads
	// them by name, and one omitted key is a field that silently reads as
	// empty.
	for _, key := range []string{`"image"`, `"movedTo"`, `"repository"`, `"tag"`, `"digest"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("the manifest does not carry %s: %s", key, data)
		}
	}
}

// TestTakeRefusesWithoutBothImages: a copy that names only one of the two
// images cannot be rolled back, so Take refuses before it creates anything --
// before it even reads the config directory, which is why the missing-directory
// case still reports the images.
func TestTakeRefusesWithoutBothImages(t *testing.T) {
	ran, moved := images().Ran, images().MovedTo
	cases := []struct {
		name   string
		images Images
	}{
		{"nothing at all", Images{}},
		{"the image it ran is missing", Images{MovedTo: moved}},
		{"the image it moves to is missing", Images{Ran: ran}},
		{"the image it ran has no repository", Images{Ran: ImageRef{Tag: "4.0.14"}, MovedTo: moved}},
		{"the image it moves to has no repository", Images{Ran: ran, MovedTo: ImageRef{Digest: "sha256:" + strings.Repeat("c", 64)}}},
		{"the image it ran has neither tag nor digest", Images{Ran: ImageRef{Repository: ran.Repository}, MovedTo: moved}},
		{"the image it moves to has neither tag nor digest", Images{Ran: ran, MovedTo: ImageRef{Repository: moved.Repository}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := configDir(t)
			dest := filepath.Join(t.TempDir(), "step", "app")
			_, err := Take(dir, globs(), dest, tc.images)
			if err == nil {
				t.Fatal("the snapshot was taken without both images")
			}
			// The sentence is box's, verbatim: it is the one thing the
			// operator reads when a step cannot say what it is moving from.
			const want = "snapshot: the image the app ran and the image it moves to are both needed"
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
			for _, p := range []string{dest, dest + ".partial"} {
				if _, statErr := os.Lstat(p); !os.IsNotExist(statErr) {
					t.Fatalf("%s exists after the refusal: %v", p, statErr)
				}
			}
			// Nothing was copied: the refusal comes before the tree, so the
			// rest of the config directory is untouched too.
			if _, statErr := os.Lstat(filepath.Join(dir, "app.db")); statErr != nil {
				t.Fatalf("the config directory was touched: %v", statErr)
			}
			// And before the config directory is even read: a step that
			// cannot name its images is refused whether or not the app has
			// state on disk.
			_, err = Take(filepath.Join(dir, "gone"), globs(), filepath.Join(t.TempDir(), "app"), tc.images)
			if err == nil || err.Error() != want {
				t.Fatalf("error with no config directory = %v, want the images' refusal", err)
			}
		})
	}
}

// TestRestoreAManifestFromBeforeImages: an update step taken by an older box
// wrote a manifest with app, taken and files only. It still restores, and the
// images read back zero, which is the caller's to interpret rather than a
// reason to refuse a copy that is otherwise intact.
func TestRestoreAManifestFromBeforeImages(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")
	if _, err := Take(dir, globs(), dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	// Exactly what the older manifest held: the same object with the two keys
	// gone, which is what a file written before them reads as.
	path := filepath.Join(dest, ManifestName)
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(old, &doc); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	delete(doc, "image")
	delete(doc, "movedTo")
	old, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(old, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	man, err := readManifest(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !man.Image.IsZero() || !man.MovedTo.IsZero() {
		t.Fatalf("a manifest without the keys read as %+v -> %+v, want zero images", man.Image, man.MovedTo)
	}

	write(t, filepath.Join(dir, "app.db"), "the migrated database", 0o600)
	if err := Restore(dir, globs(), dest); err != nil {
		t.Fatalf("restore: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the database" {
		t.Fatalf("app.db = %q, want the snapshot's copy", body)
	}
}

func TestTakeRefuses(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T) (dir string, globs []string, dest string)
		wants   []string
		// destExists is the case that refuses because a snapshot is already
		// there, so the destination is the one thing a refusal leaves alone.
		destExists bool
	}{
		{
			name: "destination exists",
			prepare: func(t *testing.T) (string, []string, string) {
				dir := configDir(t)
				dest := filepath.Join(t.TempDir(), "app")
				if err := os.MkdirAll(dest, 0o750); err != nil {
					t.Fatal(err)
				}
				return dir, globs(), dest
			},
			wants:      []string{"exists", "never overwrites"},
			destExists: true,
		},
		{
			name: "config directory missing",
			prepare: func(t *testing.T) (string, []string, string) {
				return filepath.Join(t.TempDir(), "gone"), globs(), filepath.Join(t.TempDir(), "app")
			},
			wants: []string{"config directory", "no such file"},
		},
		{
			name: "a match is a symlink",
			prepare: func(t *testing.T) (string, []string, string) {
				dir := configDir(t)
				if err := os.Symlink(filepath.Join(dir, "app.db"), filepath.Join(dir, "app.db-shm")); err != nil {
					t.Fatal(err)
				}
				return dir, globs(), filepath.Join(t.TempDir(), "app")
			},
			wants: []string{"app.db-shm", "symlink", "never follows"},
		},
		{
			// The walk does not follow the link, so without a refusal the
			// snapshot would hold config.xml and app.db and silently lack the
			// database one directory down.
			name: "a glob names files under a symlinked directory",
			prepare: func(t *testing.T) (string, []string, string) {
				dir := configDir(t)
				elsewhere := t.TempDir()
				write(t, filepath.Join(elsewhere, "db.sqlite3"), "a database somewhere else", 0o640)
				if err := os.Symlink(elsewhere, filepath.Join(dir, "db")); err != nil {
					t.Fatal(err)
				}
				return dir, nestedGlobs(), filepath.Join(t.TempDir(), "app")
			},
			wants: []string{"db is a symlink", "under it", "never follows"},
		},
		{
			name: "nothing matches",
			prepare: func(t *testing.T) (string, []string, string) {
				return configDir(t), []string{"nomatch.db"}, filepath.Join(t.TempDir(), "app")
			},
			wants: []string{"nothing matches", "nomatch.db"},
		},
		{
			name: "malformed glob",
			prepare: func(t *testing.T) (string, []string, string) {
				return configDir(t), []string{"app[.db"}, filepath.Join(t.TempDir(), "app")
			},
			wants: []string{"app[.db", "path.Match"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, globs, dest := tc.prepare(t)
			_, err := Take(dir, globs, dest, images())
			if err == nil {
				t.Fatalf("the snapshot was taken: %s", dest)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not name %q: %v", want, err)
				}
			}
			// A refusal leaves nothing a later restore could mistake for a
			// snapshot.
			paths := []string{dest + ".partial"}
			if !tc.destExists {
				paths = append(paths, dest)
			}
			for _, p := range paths {
				if _, statErr := os.Lstat(p); !os.IsNotExist(statErr) {
					t.Fatalf("%s exists after a refusal: %v", p, statErr)
				}
			}
		})
	}
}

// TestTakeReplacesAStalePartial: a run that died before its rename leaves a
// partial tree, and the next run must not be blocked by its own crash.
func TestTakeReplacesAStalePartial(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "app")
	write(t, filepath.Join(dest+".partial", "app.db"), "half a copy", 0o600)

	if _, err := Take(dir, globs(), dest, images()); err != nil {
		t.Fatalf("take over a stale partial: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the database" {
		t.Fatalf("app.db = %q, want the config directory's copy", body)
	}
}

func TestRestorePutsEverythingBack(t *testing.T) {
	dir := configDir(t)
	// As root, hand the config file to the seedbox's user so the restore's
	// ownership is a real assertion; a non-root test can only own its own
	// files, and Take records what it finds either way.
	if os.Geteuid() == 0 {
		if err := os.Chown(filepath.Join(dir, "config.xml"), 1234, 1234); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "step", "app")
	if _, err := Take(dir, globs(), dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}

	// What a failed update leaves: a migrated database, a new WAL and SHM, and
	// a settings file the new version rewrote.
	write(t, filepath.Join(dir, "app.db"), "the migrated database", 0o600)
	write(t, filepath.Join(dir, "app.db-wal"), "the new wal", 0o600)
	write(t, filepath.Join(dir, "app.db-shm"), "the new shm", 0o600)
	write(t, filepath.Join(dir, "config.xml"), "<Config migrated=\"yes\"/>", 0o600)

	if err := Restore(dir, globs(), dest); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for path, want := range map[string]string{
		"app.db":     "the database",
		"app.db-wal": "its wal",
		"config.xml": "<Config/>",
	} {
		body, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("%s = %q, want %q", path, body, want)
		}
	}
	// The SHM was created by the failed run and is in no manifest: a database
	// restored under a stranger's shared-memory file is a corrupted one.
	if _, err := os.Lstat(filepath.Join(dir, "app.db-shm")); !os.IsNotExist(err) {
		t.Fatalf("app.db-shm survived the restore: %v", err)
	}
	for path, mode := range map[string]os.FileMode{"config.xml": 0o640, "app.db": 0o640, "app.db-wal": 0o600} {
		info, err := os.Lstat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
		if !info.ModTime().Equal(taken) {
			t.Fatalf("%s mtime = %s, want %s", path, info.ModTime(), taken)
		}
		if os.Geteuid() == 0 {
			uid, gid := owner(info)
			want := 1234
			if path != "config.xml" {
				want = os.Geteuid()
			}
			if uid != want || gid != want {
				t.Fatalf("%s owned %d:%d, want %d:%d", path, uid, gid, want, want)
			}
		}
	}
	// The log directory is untouched: no glob names it, so restore is not a
	// wholesale replacement of the config directory.
	body, err := os.ReadFile(filepath.Join(dir, "logs", "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "a log database" {
		t.Fatalf("logs/logs.db = %q, want it untouched", body)
	}
}

// TestRestoreRemovesANewWAL: the database was copied while nothing else held
// it open, so the manifest names no WAL. A failed run leaves one behind, and it
// describes transactions that no longer exist: replaying it over the restored
// database is how a rollback corrupts one.
func TestRestoreRemovesANewWAL(t *testing.T) {
	dir := configDir(t)
	if err := os.Remove(filepath.Join(dir, "app.db-wal")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "step", "app")
	man, err := Take(dir, globs(), dest, images())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	for _, f := range man.Files {
		if f.Path == "app.db-wal" {
			t.Fatal("the manifest holds a WAL the config directory did not have")
		}
	}
	write(t, filepath.Join(dir, "app.db"), "the migrated database", 0o600)
	write(t, filepath.Join(dir, "app.db-wal"), "a wal the new version wrote", 0o600)

	if err := Restore(dir, globs(), dest); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "app.db-wal")); !os.IsNotExist(err) {
		t.Fatalf("the run's own WAL survived the restore: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the database" {
		t.Fatalf("app.db = %q, want the snapshot's copy", body)
	}
}

// TestRestoreRefusesATamperedSnapshot: verification is what makes the copy
// trustworthy, and it has to happen before the first removal, not after the
// database has already been deleted.
func TestRestoreRefusesATamperedSnapshot(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")
	if _, err := Take(dir, globs(), dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	write(t, filepath.Join(dest, "app.db"), "the databasX", 0o640)
	write(t, filepath.Join(dir, "app.db"), "the migrated database", 0o600)
	write(t, filepath.Join(dir, "app.db-shm"), "the new shm", 0o600)

	err := Restore(dir, globs(), dest)
	if err == nil {
		t.Fatal("a tampered snapshot was restored")
	}
	if !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("error = %v, want a verification refusal", err)
	}
	body, readErr := os.ReadFile(filepath.Join(dir, "app.db"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "the migrated database" {
		t.Fatalf("app.db = %q, want the config directory untouched", body)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "app.db-shm")); statErr != nil {
		t.Fatalf("the refusal removed a file before verifying: %v", statErr)
	}
}

// TestRestoreRefusesAManifestPathOutside: the manifest is a file on disk inside
// a config root the seedbox's user can write, and Restore writes where it says.
func TestRestoreRefusesAManifestPathOutside(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")
	man, err := Take(dir, globs(), dest, images())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	man.Files[0].Path = "../escaped"
	data, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ManifestName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Restore(dir, globs(), dest)
	if err == nil {
		t.Fatal("a manifest path outside the config directory was restored")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Fatalf("error = %v, want it to name the escaping path", err)
	}
	if _, statErr := os.Lstat(filepath.Join(filepath.Dir(dir), "escaped")); !os.IsNotExist(statErr) {
		t.Fatalf("a file was written outside the config directory: %v", statErr)
	}
}

// TestRestoreRefusesALinkedDirectory: the version that failed ran as the
// seedbox's user and can leave a link where its database directory was. Restore
// runs as whoever runs box, so writing through that link is writing wherever
// the app pointed it.
func TestRestoreRefusesALinkedDirectory(t *testing.T) {
	dir := configDir(t)
	write(t, filepath.Join(dir, "db", "db.sqlite3"), "the nested database", 0o640)
	dest := filepath.Join(t.TempDir(), "step", "app")
	if _, err := Take(dir, nestedGlobs(), dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	elsewhere := t.TempDir()
	if err := os.RemoveAll(filepath.Join(dir, "db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "db")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "app.db-shm"), "the new shm", 0o600)

	err := Restore(dir, nestedGlobs(), dest)
	if err == nil {
		t.Fatal("a snapshot was restored through a symlinked directory")
	}
	for _, want := range []string{"db is a symlink", "db/db.sqlite3", "never writes through"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not name %q: %v", want, err)
		}
	}
	left, readErr := os.ReadDir(elsewhere)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(left) != 0 {
		t.Fatalf("the restore wrote %s through the link", left[0].Name())
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "app.db-shm")); statErr != nil {
		t.Fatalf("the refusal removed a file first: %v", statErr)
	}
}

// TestRestoreRebuildsAMissingDirectory: a failed version that removed its
// database directory gets it back as a real directory, with the file in it.
func TestRestoreRebuildsAMissingDirectory(t *testing.T) {
	dir := configDir(t)
	write(t, filepath.Join(dir, "db", "db.sqlite3"), "the nested database", 0o640)
	dest := filepath.Join(t.TempDir(), "step", "app")
	if _, err := Take(dir, nestedGlobs(), dest, images()); err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "db")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(dir, nestedGlobs(), dest); err != nil {
		t.Fatalf("restore: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != dirMode {
		t.Fatalf("db = %s, want a directory at %o", info.Mode(), dirMode)
	}
	body, err := os.ReadFile(filepath.Join(dir, "db", "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the nested database" {
		t.Fatalf("db/db.sqlite3 = %q, want the snapshot's copy", body)
	}
}

// TestReachesBelow: which directories the walk reads, and which symlinks stand
// where a glob names files.
func TestReachesBelow(t *testing.T) {
	globs := []string{"config.xml", "db/*.db", "a/*/c/file"}
	cases := []struct {
		rel  string
		want bool
	}{
		{"db", true},
		{"a", true},
		{"a/b", true},
		{"a/b/c", true},
		{"a/b/d", false},
		{"a/b/c/file", false},
		{"db/app.db", false},
		{"config.xml", false},
		{"logs", false},
	}
	for _, tc := range cases {
		if got := reachesBelow(globs, tc.rel); got != tc.want {
			t.Errorf("reachesBelow(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// TestTakeDoesNotReadADirectoryNoGlobNames: the directory is closed, so a walk
// that entered it would fail the snapshot (run as root nothing is closed, and
// TestReachesBelow is what holds the rule).
func TestTakeDoesNotReadADirectoryNoGlobNames(t *testing.T) {
	dir := configDir(t)
	closed := filepath.Join(dir, "Metadata")
	write(t, filepath.Join(closed, "bundle"), "artwork", 0o600)
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })
	if _, err := Take(dir, globs(), filepath.Join(t.TempDir(), "app"), images()); err != nil {
		t.Fatalf("take read a directory no glob names: %v", err)
	}
}

// TestRestoreRefusesAnEmptyManifest: an unreadable manifest that decodes to no
// files would make Restore delete every file its globs match.
func TestRestoreRefusesAnEmptyManifest(t *testing.T) {
	dir := configDir(t)
	dest := filepath.Join(t.TempDir(), "step", "app")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ManifestName), []byte(`{"app":"app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Restore(dir, globs(), dest)
	if err == nil || !strings.Contains(err.Error(), "lists no files") {
		t.Fatalf("error = %v, want a refusal of the empty manifest", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "app.db")); statErr != nil {
		t.Fatalf("app.db was removed: %v", statErr)
	}
}

// TestRestoreWithoutAManifest: a directory under the snapshots root that is not
// a snapshot box made is refused, not read as an empty one.
func TestRestoreWithoutAManifest(t *testing.T) {
	dir := configDir(t)
	err := Restore(dir, globs(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), ManifestName) {
		t.Fatalf("error = %v, want a missing-manifest refusal", err)
	}
}

// TestMatchesStayInsideTheirDirectory: a glob sees a whole relative path, so
// `*` never crosses a directory separator and there is no `**`. What a glob can
// reach is what a reviewer can read in the catalog.
func TestMatchesStayInsideTheirDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app.db"), "top level", 0o600)
	write(t, filepath.Join(dir, "db", "app.db"), "nested", 0o600)
	write(t, filepath.Join(dir, "db", "nested", "app.db"), "deeper", 0o600)

	cases := []struct {
		glob  string
		paths []string
	}{
		{glob: "*.db", paths: []string{"app.db"}},
		// `**` is two stars in one segment, not recursion: the whole path is
		// matched, so neither form reaches a file below `db/`.
		{glob: "**", paths: []string{"app.db"}},
		{glob: "db/*.db", paths: []string{"db/app.db"}},
		{glob: "db/**", paths: []string{"db/app.db"}},
		{glob: "db/**/app.db", paths: []string{"db/nested/app.db"}},
		{glob: "db/nested/app.db", paths: []string{"db/nested/app.db"}},
		{glob: "db/*/*.db", paths: []string{"db/nested/app.db"}},
		{glob: "*.nomatch", paths: nil},
	}
	for _, tc := range cases {
		t.Run(tc.glob, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "app")
			man, err := Take(dir, []string{tc.glob}, dest, images())
			if tc.paths == nil {
				if err == nil || !strings.Contains(err.Error(), "nothing matches") {
					t.Fatalf("error = %v, want nothing to match", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			var got []string
			for _, f := range man.Files {
				got = append(got, f.Path)
			}
			if strings.Join(got, ",") != strings.Join(tc.paths, ",") {
				t.Fatalf("%q matched %v, want %v", tc.glob, got, tc.paths)
			}
		})
	}
}

// TestOwnerIsReadFromTheFile keeps the manifest's owner honest on the platform
// box runs on: a stat box cannot read would record -1, which a restore would
// then try to apply.
func TestOwnerIsReadFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	write(t, path, "the database", 0o600)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := owner(info)
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform's stat carries no owner")
	}
	if uid != int(st.Uid) || gid != int(st.Gid) {
		t.Fatalf("owner = %d:%d, want %d:%d", uid, gid, st.Uid, st.Gid)
	}
}
