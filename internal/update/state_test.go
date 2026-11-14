package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStateRoundTripsAndStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", FileName)
	store := FileState{Path: path}

	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load on a box that never checked: %v", err)
	}
	if !state.LastCheck.IsZero() || state.LastStep != nil || len(state.Failed) != 0 {
		t.Fatalf("Load = %+v, want the zero state", state)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := State{
		LastCheck: at,
		LastStep:  &StepRecord{From: "0.4.0", To: "0.5.0", At: at, Outcome: OutcomeFailed, Error: "health gate failed"},
		Failed:    []string{"0.5.0"},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %v, want 0600: the record is box's own state", mode)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.LastCheck.Equal(want.LastCheck) || got.LastStep == nil || *got.LastStep != *want.LastStep {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	if len(got.Failed) != 1 || got.Failed[0] != "0.5.0" {
		t.Fatalf("Failed = %v, want the recorded version", got.Failed)
	}
}

// TestFileStateRefusesAnUnknownField: a hand-edited or newer record is a
// refusal, not a guess. Silently dropping a field could forget the release a
// box already failed on.
func TestFileStateRefusesAnUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(`{"lastCheck":"2026-01-02T03:04:05Z","mystery":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := FileState{Path: path}.Load()
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load = %v, want the unknown field named", err)
	}
}

func TestFilePathIsBesideTheClusterRecord(t *testing.T) {
	got := FilePath("/etc/swizzin/cluster.yaml")
	if want := "/etc/swizzin/" + FileName; got != want {
		t.Fatalf("FilePath = %q, want %q", got, want)
	}
}
