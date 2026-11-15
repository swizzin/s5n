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

// State is `<config>/.box/migrate/<plan-id>/state.json`: what one plan has
// already done.
//
// Apply writes to a running box's config root and its data root, two trees a
// crash can leave half-changed: a config moved into place but the release not
// yet applied, or the downloads renamed and the instances not moved. The file
// is what makes the second run skip exactly the steps the first one finished
// instead of renaming a directory that is no longer there or moving a config
// over the app that already runs it.
type State struct {
	// Plan is the sha256 hex of the plan file this state belongs to. The
	// directory is named by the first 12 hex of the same digest, so a
	// mismatch means the directory was written by hand.
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
	// Values says the instances were switched on in the seedbox's files.
	// It is the last step before apply, so a state with this set has nothing
	// left to do; one without it is a migration that must be finished before
	// another can start.
	Values bool `json:"values"`
}

// PlanID is the plan's short name: the first 12 hex of its sha256, which is
// the directory under `<config>/.box/migrate` a run stages in and records to.
// Short because it ends up in paths a person reads, long enough that two plans
// never collide.
func PlanID(planBytes []byte) string { return PlanDigest(planBytes)[:12] }

// PlanDigest is the full sha256 hex of the plan file, which is what State
// records: the short id is a directory name, the digest is the identity.
func PlanDigest(planBytes []byte) string {
	sum := sha256.Sum256(planBytes)
	return hex.EncodeToString(sum[:])
}

// stateDir is where one plan stages and records: `<config>/.box/migrate/<id>`.
// It sits on the config claim, so the one app that has to write its own format
// (Plex's database, through the image's shell) runs a Job that mounts a
// subPath of the same claim at /config and never a second volume.
func stateDir(configRoot, id string) string {
	return filepath.Join(configRoot, ".box", "migrate", id)
}

// stateFile is the record inside stateDir.
func stateFile(dir string) string { return filepath.Join(dir, "state.json") }

// loadState reads one plan's record. An absent file is an empty record: a plan
// that has run nothing yet is the normal case.
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
// step that changes something on the host calls it before the next step starts,
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
//
// Two plans share the data root and the config root, and the second one would
// rename a directory the first has already renamed, or move a config over the
// instance the first put there. A plan whose last step (enabling the instances)
// never ran is therefore a stop, not something to work around: the operator
// finishes it, or removes its directory to say the source is untouched.
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
