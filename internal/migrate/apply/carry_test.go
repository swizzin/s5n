package apply

// carry_test.go is the other end of the read's decision: what a run stages at the
// config root, where a full carry may not fit even though the snapshot was taken
// with room. A run that carries less than the plan says says so, and its staged
// copy is the state alone.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// shortDisk is a filesystem with one answer for every path.
type shortDisk struct{ free int64 }

func (d shortDisk) Free(string) (int64, error) { return d.free, nil }

func (d shortDisk) Same(string, string) (bool, error) { return true, nil }

// plexCarryEnv is one plan naming Plex and the source's config tree, as a
// snapshot's config/ directory holds it: the state, the artwork Plex built, and
// the caches it rebuilds.
func plexCarryEnv(t *testing.T) (Env, *plan.Plan, []byte) {
	t.Helper()
	root := t.TempDir()
	configs := filepath.Join(root, "snapshot", "config")
	home := filepath.Join(configs, "plex", "Library", "Application Support", "Plex Media Server")
	for rel, body := range map[string]string{
		"Preferences.xml": `<Preferences MachineIdentifier="synthetic-machine"/>` + "\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db": "sqlite\n",
		"Metadata/Posters/poster.jpg":                              strings.Repeat("j", 4096),
		"Media/localhost/bif/index.bif":                            strings.Repeat("b", 4096),
		"Cache/PhotoTranscoder/thumb.jpg":                          strings.Repeat("c", 4096),
	} {
		full := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := []byte(`version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    carry: ` + plan.CarryFull + `
    paths:
      - from: /srv/apps/plex
        to: /config
`)
	p, err := plan.Parse(doc)
	if err != nil {
		t.Fatalf("the fixture plan: %v", err)
	}
	env := Env{
		ConfigRoot: filepath.Join(root, "seedbox", "config"),
		Configs:    configs,
		Catalog:    []string{"plex"},
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets:    map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Enabled:    func(string) bool { return false },
		Confirm:    func(Summary) (bool, error) { return true, nil },
		Values:     &fakeValues{},
		// Plex's database is read through its own image, which a test cannot
		// start: the runner answers the query the check makes.
		Run: func(context.Context, string, []string, []byte) ([]byte, error) {
			return []byte("@end|1\n"), nil
		},
	}
	if err := os.MkdirAll(env.ConfigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return env, p, doc
}

// stagedPlex is what one run left in the config root for Plex.
func stagedPlex(t *testing.T, env Env, p *plan.Plan, raw []byte) (Staged, bool) {
	t.Helper()
	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var staged Staged
	for _, s := range res.Staged {
		if s.App == "plex" {
			staged = s
		}
	}
	if !carriedFile(t, env, "Metadata/Posters/poster.jpg") {
		return staged, false
	}
	return staged, true
}

// carriedFile is whether one file is in the Plex home under the config root.
func carriedFile(t *testing.T, env Env, rel string) bool {
	t.Helper()
	at := filepath.Join(env.ConfigRoot, "plex", "Library", "Application Support", "Plex Media Server", filepath.FromSlash(rel))
	_, err := os.Stat(at)
	return err == nil
}

func TestAFullCarryIsStagedWholeWhenTheConfigRootCanHoldIt(t *testing.T) {
	env, p, raw := plexCarryEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	staged, artwork := stagedPlex(t, env, p, raw)
	if !artwork {
		t.Error("a run with room did not stage the artwork the snapshot carried")
	}
	if staged.Files < 3 {
		t.Errorf("the run staged %d files, want the tree", staged.Files)
	}
}

func TestAShortConfigRootCarriesTheStateAndSaysSo(t *testing.T) {
	env, p, raw := plexCarryEnv(t)
	env.Disk = shortDisk{free: 1 << 20}
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	staged, artwork := stagedPlex(t, env, p, raw)
	if artwork {
		t.Error("a run with 1 MiB at the config root staged a tree it cannot hold")
	}
	if staged.Files == 0 {
		t.Error("the run staged nothing at all")
	}
	var row InstanceSummary
	for _, in := range shown.Instances {
		if in.App == "plex" {
			row = in
		}
	}
	if !strings.Contains(row.Short, "needs") || !strings.Contains(row.Short, "is free at") {
		t.Errorf("the row does not say why it carries less: %+v", row)
	}
	if !strings.Contains(row.Carry, "its state only") {
		t.Errorf("the row says it carries %q", row.Carry)
	}
	if len(shown.Carried) != 1 || !strings.Contains(shown.Carried[0].Note, "carried Preferences.xml and its databases") {
		t.Errorf("the notes say %+v, want the state-only line", shown.Carried)
	}
	// The plan is not edited: the snapshot is what it is, and the next run of
	// this snapshot on a host with room carries the tree.
	if p.Apps["plex"].Carry != plan.CarryFull {
		t.Errorf("apply rewrote the plan's carry to %q", p.Apps["plex"].Carry)
	}
}

func TestAPlanThatSaysTheStateNeverStagesTheTree(t *testing.T) {
	env, p, _ := plexCarryEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	app := p.Apps["plex"]
	app.Carry = plan.CarryMinimal
	p.Apps["plex"] = app
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("encode the plan: %v", err)
	}
	staged, artwork := stagedPlex(t, env, p, raw)
	if artwork {
		t.Error("a plan carrying the state staged the artwork anyway")
	}
	if staged.Files != 2 {
		t.Errorf("the run staged %d files, want the preferences and the database", staged.Files)
	}
	if phrase := carriedPhrase(t, envelopeFor(t, env, p, raw)); !strings.Contains(phrase, "its state only") {
		t.Errorf("the row says it carries %q", phrase)
	}
}

// envelopeFor is the summary Survey shows for one snapshot, which is the read
// that has no cluster.
func envelopeFor(t *testing.T, env Env, p *plan.Plan, raw []byte) Summary {
	t.Helper()
	env.Run = func(context.Context, string, []string, []byte) ([]byte, error) {
		return []byte("@end|1\n"), nil
	}
	s, err := Survey(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	return s
}

// carriedPhrase is Plex's row in a summary.
func carriedPhrase(t *testing.T, s Summary) string {
	t.Helper()
	for _, in := range s.Instances {
		if in.App == "plex" {
			return in.Carry
		}
	}
	t.Fatalf("the summary has no plex row: %+v", s.Instances)
	return ""
}

func TestAnAgreementHoldsWhenTheConfigRootShortensTheCarry(t *testing.T) {
	env, p, raw := plexCarryEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	agreed := envelopeFor(t, env, p, raw)
	if phrase := carriedPhrase(t, agreed); !strings.Contains(phrase, "its whole data") {
		t.Fatalf("the pre-install pass says it carries %q, and it had room", phrase)
	}

	env.Disk = shortDisk{free: 1 << 20}
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if why := shown.Holds(agreed); why != "" {
		t.Errorf("a run that carried less than the question promised is a fault: %s", why)
	}
	rows := shown.CheckedRows(agreed)
	if len(rows) != 1 || rows[0].Name != "plex" {
		t.Fatalf("the rows the question could not fill in are %+v", rows)
	}
	joined := strings.Join(rows[0].Facts, " · ")
	if !strings.Contains(joined, "is free at") || !strings.Contains(joined, "its state only") {
		t.Errorf("the filled-in row does not say why it carried less: %q", joined)
	}
	// Anything else is still a fault: the same run with something the question
	// never showed, here a workload that is not coming, is not what was agreed.
	other := shown
	other.Outside = append(append([]string(nil), shown.Outside...), "default/jellyfin")
	if why := other.Holds(agreed); why == "" {
		t.Error("a summary with a workload the question never showed held")
	}
}

func TestTheStateSetIsWhatTheSnapshotNames(t *testing.T) {
	state := plex.Snapshot(false)
	if len(state.Files) == 0 {
		t.Fatal("Plex has no state-only set")
	}
	for _, f := range state.Files {
		if !strings.HasSuffix(f.Path, ".xml") && !strings.Contains(f.Path, "Databases/") {
			t.Errorf("the state set carries %s", f.Path)
		}
	}
}

func TestTheStateLandsWhereTheAppReadsIt(t *testing.T) {
	env, p, raw := plexCarryEnv(t)
	env.Disk = shortDisk{free: 1 << 20}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	home := filepath.Join(env.ConfigRoot, "plex", "Library", "Application Support", "Plex Media Server")
	for _, want := range []string{"Preferences.xml", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(want))); err != nil {
			t.Errorf("the staged config holds no %s where Plex reads it: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(env.ConfigRoot, "plex", "Preferences.xml")); err == nil {
		t.Error("the state was staged at the top of the mount, where Plex does not read it")
	}
}
