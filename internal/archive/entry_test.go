package archive

// entry_test.go covers reading one entry out of an archive without extracting the
// rest: how a run asks a snapshot who it is before opening it.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The rest of the archive -- where the config trees are -- is not touched.
func TestReadEntryReturnsOneEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	w, err := Create(path, Manifest{Kind: KindMigrate, Mode: "survey", Created: "2026-11-17T09:00:00Z", Box: "0.5.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile(ReportName, []byte("a report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEntry(path, PlanName)
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if string(got) != "version: 2\n" {
		t.Errorf("the plan read back as %q", got)
	}
}

// A name the archive does not hold, a path that would step outside it, and a
// directory are all named refusals rather than empty answers.
func TestReadEntryRefusesWhatIsNotAnEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	w, err := Create(path, Manifest{Kind: KindMigrate, Mode: "survey", Created: "2026-11-17T09:00:00Z", Box: "0.5.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile(ConfigPrefix+"/sonarr/config.xml", []byte("<Config/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEntry(path, "nothing.yaml"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("reading a name the archive does not hold = %v, want ErrNoEntry", err)
	}
	if _, err := ReadEntry(path, "../plan.yaml"); err == nil || !strings.Contains(err.Error(), "not a path inside an archive") {
		t.Errorf("reading a path outside the archive = %v, want it refused", err)
	}
	if _, err := ReadEntry(path, ConfigPrefix); err == nil {
		t.Error("reading a directory came back as an entry")
	}
	if _, err := ReadEntry(filepath.Join(t.TempDir(), "absent.tar.gz"), PlanName); err == nil {
		t.Error("reading a file that is not there came back as an entry")
	}
}
