package sqlitedb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyCannotWriteAndReadWriteCan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	if _, err := OpenReadWrite(ctx, path); err == nil {
		t.Fatal("OpenReadWrite created a database that did not exist")
	}

	// Make one the way an app would, outside this package's openers.
	seed, err := open(ctx, mustCreate(t, path), "rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE RootFolders (Id INTEGER PRIMARY KEY, Path TEXT); INSERT INTO RootFolders (Path) VALUES ('/library/media/tv')`); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`UPDATE RootFolders SET Path = '/data/media/tv'`); err == nil {
		t.Error("a read-only handle wrote")
	}
	if bad, err := IntegrityCheck(ctx, ro); err != nil || len(bad) != 0 {
		t.Errorf("IntegrityCheck = %v, %v; want sound", bad, err)
	}

	rw, err := OpenReadWrite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := rw.Exec(`UPDATE RootFolders SET Path = '/data/media/tv'`); err != nil {
		t.Errorf("a read-write handle could not write: %v", err)
	}
}

// mustCreate leaves an empty file so open's existence check passes for the
// seeding handle; SQLite treats an empty file as an empty database.
func mustCreate(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
