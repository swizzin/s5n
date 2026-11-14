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

// FileName is update's record, written beside bootstrap's cluster record so
// `box status` can read the box's whole state from one directory.
const FileName = "update.json"

// The two things a step can have ended in.
const (
	// OutcomeOK says the step passed both gates and the box is on that
	// release.
	OutcomeOK = "ok"
	// OutcomeFailed says the step did not pass. The release is wherever the
	// step left it -- box never moves one back -- and the record's error
	// names what Helm reported when the step stopped.
	OutcomeFailed = "failed"
)

// State is `<record dir>/update.json`: what box last asked the registry and
// what it last did, so a later run and a later `box status` need not ask a
// registry or Helm to find out.
//
// It is box's own file and its field names are stable: `box status` reads them
// and so does the timer's "is there anything to do" check. A field is added,
// never renamed, and a missing file is a box that has never checked.
type State struct {
	// LastCheck is when box last listed the published releases, which every
	// run does -- `--check` included, and including a run that found nothing
	// and one that was declined at the confirm. It is not the last successful
	// update.
	LastCheck time.Time `json:"lastCheck"`
	// LastStep is the most recent step box attempted, whether it passed or
	// failed. Nil on a box that has never taken a step.
	LastStep *StepRecord `json:"lastStep,omitempty"`
	// Failed lists release versions box tried to install and could not: a
	// step whose gate failed, or an apply that refused the release outright.
	// Selection passes them over -- with a message -- until one of them is
	// superseded by a passing step, which clears every version at or below
	// the release it reached. `--chart-version` naming one is the operator's
	// override.
	Failed []string `json:"failed,omitempty"`
}

// StepRecord is one attempted step, as `box status` shows it: the versions it
// moved between, the revision it began at and the apps it moved, when it
// stopped, how it ended and, for a failure, the gate and the error in box's
// own words.
type StepRecord struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Error   string    `json:"error,omitempty"`
	// Revision is the Helm revision the step began at: where a rollback
	// command for this step returns to.
	Revision int `json:"revision,omitempty"`
	// Moved is the apps whose image the step moved, empty for a step that
	// moved none.
	Moved []string `json:"moved,omitempty"`
	// Gate is the gate that failed: "apply", "wiring" or "health". Empty on
	// a step that passed.
	Gate string `json:"gate,omitempty"`
}

// StateStore is where update's record lives. It is an interface so the flow's
// tests can hand it a value and assert on it, the same way release's fakes
// stand in for Helm.
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

// Load reads the record. A missing file is a box that has never checked, which
// is not an error; a malformed one is, because guessing what a half-written
// record meant could skip a release box already failed on.
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

// Save writes the record atomically, mode 0600: it is box's own state, and a
// half-written file would be read as a box that never checked.
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

// FilePath is the record's path for a box whose bootstrap record is at
// recordPath: the same directory, so the two files travel together.
func FilePath(recordPath string) string {
	return filepath.Join(filepath.Dir(recordPath), FileName)
}
