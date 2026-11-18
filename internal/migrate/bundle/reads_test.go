package bundle

// reads_test.go covers listing a directory's snapshots with the plan each one
// carries: the search `box migrate cutover` runs for the read already on record.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/archive"
)

func TestReadsListsNewestFirstWithEachPlan(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	// Written oldest first, so the order cannot come from the order of writes.
	var names []string
	for _, at := range []struct {
		at   time.Time
		mode archive.Mode
		plan string
	}{
		{base, archive.ModeSurvey, "version: 2\n# earliest\n"},
		{base.Add(time.Hour), archive.ModeSurvey, "version: 2\n# latest\n"},
		{base.Add(30 * time.Minute), archive.ModeFinal, "version: 2\n# middle\n"},
	} {
		path := filepath.Join(dir, FileName(at.mode, at.at))
		if _, err := Write(path, Snapshot{
			Mode:   at.mode,
			Plan:   []byte(at.plan),
			Report: []byte("a read\n"),
			Now:    func() time.Time { return at.at },
		}); err != nil {
			t.Fatal(err)
		}
		names = append(names, path)
	}
	// A stray file, and a file that is named like a snapshot and is not one: both
	// are left alone rather than being the directory's answer.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName(archive.ModeSurvey, base.Add(2*time.Hour))), []byte("not an archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reads, err := Reads(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 3 {
		t.Fatalf("%d reads, want the three snapshots", len(reads))
	}
	want := []string{names[1], names[2], names[0]}
	for i, r := range reads {
		if r.Path != want[i] {
			t.Errorf("read %d is %s, want %s", i, filepath.Base(r.Path), filepath.Base(want[i]))
		}
	}
	if reads[1].Mode != archive.ModeFinal {
		t.Errorf("the final snapshot's mode = %q", reads[1].Mode)
	}
	if got := string(reads[2].Plan); got != "version: 2\n# earliest\n" {
		t.Errorf("the oldest read's plan = %q", got)
	}
	if !reads[0].Taken.Equal(base.Add(time.Hour)) {
		t.Errorf("the newest read's time = %s, want the name's", reads[0].Taken)
	}
}

// TestReadsOfAMissingDirectoryIsNoReads: a host that has never been read has no
// snapshot directory, which is not an error on the way to its first read.
func TestReadsOfAMissingDirectoryIsNoReads(t *testing.T) {
	reads, err := Reads(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Reads of a missing directory = %v", err)
	}
	if len(reads) != 0 {
		t.Errorf("%d reads out of a directory that is not there", len(reads))
	}
}
