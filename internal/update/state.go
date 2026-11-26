package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// FileName is update's record, beside bootstrap's cluster record so `box status`
// reads one directory.
const FileName = "update.json"

// The two things a step can have ended in.
const (
	// OutcomeOK says the step passed and the box is on that release.
	OutcomeOK = "ok"
	// OutcomeFailed says the step did not pass; the release is wherever it was
	// left, and Error names what Helm reported.
	OutcomeFailed = "failed"
)

// State is `<record dir>/update.json`: what box last asked the registry and did.
// The field names are stable -- `box status` and the timer read them -- and a
// missing file is a box that has never checked.
type State struct {
	// LastCheck is when box last listed releases, including a run that found
	// nothing or was declined; it is not the last update.
	LastCheck time.Time `json:"lastCheck"`
	// LastStep is the most recent step attempted, passed or failed.
	LastStep *StepRecord `json:"lastStep,omitempty"`
	// Failed lists versions box could not install. Selection skips them until a
	// passing step clears every version at or below the one it reached;
	// `--chart-version` is the operator's override.
	Failed []string `json:"failed,omitempty"`
	// Health is each app's readiness as last read. A notice follows a Ready
	// flip either way; the first read is recorded silently.
	Health map[string]AppHealth `json:"health,omitempty"`
}

// AppHealth is one app's readiness and a short reason when it is not.
type AppHealth struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// Due says whether a timer wake has a check to make: never checked, or a whole
// interval old.
func (s State) Due(now time.Time, interval time.Duration) bool {
	if s.LastCheck.IsZero() || interval <= 0 {
		return true
	}
	return !now.Before(s.LastCheck.Add(interval))
}

// StepRecord is one attempted step as `box status` shows it.
type StepRecord struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Error   string    `json:"error,omitempty"`
	// Revision is the Helm revision the step began at.
	Revision int `json:"revision,omitempty"`
	// Moved is the apps whose image the step moved.
	Moved []string `json:"moved,omitempty"`
	// RolledBack is the apps put back because the new image kept exiting; a
	// passing step can carry one.
	RolledBack []string `json:"rolledBack,omitempty"`
	// Gate is the gate that failed; empty on a step that passed.
	Gate string `json:"gate,omitempty"`
}

// StateStore is where update's record lives, an interface so tests can assert
// on it.
type StateStore interface {
	// Load returns the zero State when nothing has been written yet.
	Load() (State, error)
	// Save replaces the record.
	Save(State) error
}

// FileState is the record on disk.
type FileState struct {
	Path string
}

// Load reads the record. A missing file is a box that never checked; a
// malformed one is an error, because guessing could skip a failed release.
func (s FileState) Load() (State, error) {
	var state State
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return State{}, fmt.Errorf("%s: %w", s.Path, err)
	}
	return state, nil
}

// Save writes the record atomically, 0600: a half-written file reads as a box
// that never checked.
func (s FileState) Save(state State) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.Path, append(data, '\n'), 0o600)
}

// FilePath puts the record beside the bootstrap record, so they travel
// together.
func FilePath(recordPath string) string {
	return filepath.Join(filepath.Dir(recordPath), FileName)
}
