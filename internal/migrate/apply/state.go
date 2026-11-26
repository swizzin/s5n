package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// State is `<config>/.box/migrate/<plan-id>/state.json`: what one plan has done. A
// crash can leave the config and data roots half-changed, so this file makes the
// second run skip exactly the finished steps.
type State struct {
	// Plan is the plan's sha256; the directory is its first 12 hex, so a mismatch
	// means a hand-written directory.
	Plan string `json:"plan"`
	// Renamed are the data directories renamed under the data root, by their
	// source name.
	Renamed []string `json:"renamed,omitempty"`
	// Moved are the instances whose staged config was moved into the config
	// root, by instance name.
	Moved []string `json:"moved,omitempty"`
	// Secret says each app's own API key was written into the release Secret.
	Secret bool `json:"secret"`
	// Owned says the adopted connections were seeded into the release's
	// owned.json.
	Owned bool `json:"owned"`
	// Values says the instances were switched on, the last step: a state with it has
	// nothing left, and one without must be finished before another starts.
	Values bool `json:"values"`
}

// PlanID is the first 12 hex of the plan's sha256: short for paths, long enough
// not to collide.
func PlanID(planBytes []byte) string { return PlanDigest(planBytes)[:12] }

// PlanDigest is the full sha256 hex of the plan file, which is what State
func PlanDigest(planBytes []byte) string {
	sum := sha256.Sum256(planBytes)
	return hex.EncodeToString(sum[:])
}

// stateDir is where one plan stages and records: `<config>/.box/migrate/<id>`.
// subPath of the same claim at /config and never a second volume.
func stateDir(configRoot, id string) string {
	return filepath.Join(configRoot, ".box", "migrate", id)
}

// stateFile is the record inside stateDir.
func stateFile(dir string) string { return filepath.Join(dir, "state.json") }

// loadState reads one plan's record. An absent file is an empty record: a plan
func loadState(dir string) (*State, error) {
	raw, err := os.ReadFile(stateFile(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", stateFile(dir), err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%s: %w; a half-written record cannot be trusted, so remove %s and run migrate again from the start",
			stateFile(dir), err, dir)
	}
	return &st, nil
}

// saveState writes the record atomically, keeping the staging directory. Every
// so a crash loses at most the step in flight.
func saveState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("record %s: %w", stateFile(dir), err)
	}
	if err := atomicfile.Write(stateFile(dir), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return nil
}

// otherPlan refuses a run while another plan is half done.
// Two plans share the data root and the config root, and the second one would
// never ran is therefore a stop, not something to work around: the operator
func otherPlan(root, own string, digest string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == own {
			continue
		}
		dir := filepath.Join(root, e.Name())
		st, err := loadState(dir)
		if err != nil {
			return err
		}
		if st.Plan == "" || st.Values || st.Plan == digest {
			continue
		}
		return fmt.Errorf("another migration is half done: %s; run `box migrate apply` on that plan again, or remove the directory if the source is untouched", dir)
	}
	return nil
}
