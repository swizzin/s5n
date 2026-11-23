package apply

// state_test.go is the record a migration leaves behind: the file that turns a
// second run into a skip, and the one that must never be trusted when it is
// half written.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveStateRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	want := &State{
		Plan:    PlanDigest([]byte("version: 2\napps: {}\n")),
		Renamed: []string{"Downloads"},
		Moved:   []string{"sonarr"},
		Owned:   true,
	}
	if err := saveState(dir, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Errorf("state round-trips to %s, want %s", b, a)
	}
}

func TestLoadStateOfANewPlanIsEmpty(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "nothing"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Values || st.Plan != "" || len(st.Moved) != 0 {
		t.Errorf("an absent record read as %+v, want the zero value", st)
	}
}

func TestLoadStateRefusesAHalfWrittenRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"plan": "abc`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(dir); err == nil {
		t.Fatal("a truncated record was read as a record")
	} else if !contains(err.Error(), dir) {
		t.Errorf("the refusal does not name the directory to remove:\n%s", err)
	}
}

// TestPlanIDIsTheFirstTwelveHex pins the directory name the design names, so a
// plan's staging directory is predictable from the file.
func TestPlanIDIsTheFirstTwelveHex(t *testing.T) {
	id := PlanID([]byte("version: 2\napps: {}\n"))
	if len(id) != 12 {
		t.Errorf("plan id %q is %d characters, want 12", id, len(id))
	}
	if id != PlanDigest([]byte("version: 2\napps: {}\n"))[:12] {
		t.Error("the id is not the digest's first twelve characters")
	}
	if id == PlanID([]byte("version: 2\napps: {}\n\n")) {
		t.Error("two plans with different bytes share an id")
	}
}
