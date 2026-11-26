package apply

// bulk_test.go proves the other half of a full carry: the tree the snapshot holds
// and no check reads is written once, at the config root, out of the snapshot --
// and the pass before an install counts it without staging it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// fakeBulk is a snapshot's copy of one app's config: the numbers the pass that
// verified the archive counted, and a Stage that writes the tree the snapshot
// holds. It records what a run asked it, because which of the two copies a run
// stages is the thing these tests are about.
type fakeBulk struct {
	tree         string
	files, bytes int64
	whole        bool
	staged       []string
	measured     []string
}

func (b *fakeBulk) HoldsTree(string) bool { return b.whole }

func (b *fakeBulk) Measure(instance string) (Staged, error) {
	b.measured = append(b.measured, instance)
	return Staged{Files: b.files, Bytes: b.bytes}, nil
}

func (b *fakeBulk) Stage(instance, dst string, uid, gid int) (Staged, error) {
	b.staged = append(b.staged, instance)
	if _, err := copyTree(dst, b.tree, uid, gid); err != nil {
		return Staged{}, err
	}
	return Staged{Files: b.files, Bytes: b.bytes}, nil
}

// plexSplitEnv is a snapshot of Plex as an apply reads it now: the extract holds
// the state a check reads, and the tree -- the artwork Plex spent hours building
// -- is what the snapshot holds for the staging step.
func plexSplitEnv(t *testing.T) (Env, *plan.Plan, []byte, *fakeBulk) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "snapshot", "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	const home = "Library/Application Support/Plex Media Server"
	files := map[string]string{
		home + "/Preferences.xml": `<Preferences MachineIdentifier="synthetic-machine"/>` + "\n",
		home + "/Plug-in Support/Databases/com.plexapp.plugins.library.db": "sqlite\n",
		home + "/Metadata/Posters/poster.jpg":                              strings.Repeat("j", 4096),
	}
	var bytes int64
	for rel, body := range files {
		at := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		bytes += int64(len(body))
	}
	// The extract: the state alone, which is what a full carry's extraction keeps.
	extract := filepath.Join(root, "snapshot", "config")
	for _, rel := range []string{"Preferences.xml", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		from := filepath.Join(tree, filepath.FromSlash(home), filepath.FromSlash(rel))
		at := filepath.Join(extract, "plex", filepath.FromSlash(home), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := []byte(`version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    carry: full
    paths:
      - from: /srv/apps/plex
        to: /config
`)
	p, err := plan.Parse(doc)
	if err != nil {
		t.Fatalf("the fixture plan: %v", err)
	}
	configRoot := filepath.Join(root, "seedbox", "config")
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	env := Env{
		ConfigRoot: configRoot,
		Configs:    extract,
		Bulk:       &fakeBulk{tree: tree, files: int64(len(files)), bytes: bytes, whole: true},
		Catalog:    []string{"plex"},
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets:    map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Enabled:    func(string) bool { return false },
		Confirm:    func(Summary) (bool, error) { return true, nil },
		Values:     &fakeValues{},
		Run: func(context.Context, string, []string, []byte) ([]byte, error) {
			return []byte("@end|1\n"), nil
		},
	}
	return env, p, doc, env.Bulk.(*fakeBulk)
}

func TestTheTreeIsStagedOutOfTheSnapshotOnce(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	var staged []Staged
	env.Confirm = func(Summary) (bool, error) { return true, nil }
	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, s := range res.Staged {
		if s.App == "plex" {
			staged = append(staged, s)
		}
	}
	if len(bulk.staged) != 1 || bulk.staged[0] != "plex" {
		t.Errorf("the run staged %v out of the snapshot, want plex once", bulk.staged)
	}
	if len(bulk.measured) != 1 {
		// decideCarry asks for the numbers it checks the room against.
		t.Errorf("the run measured the snapshot %d times, want once", len(bulk.measured))
	}
	if len(staged) != 1 || staged[0].Files != bulk.files {
		t.Fatalf("the row counts %+v, want the snapshot's %d files", staged, bulk.files)
	}
	if !carriedFile(t, env, "Metadata/Posters/poster.jpg") {
		t.Error("a run with room did not stage the artwork the snapshot carried")
	}
}

func TestAShortConfigRootStagesTheStateItExtracted(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	env.Disk = shortDisk{free: 1 << 20}
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(bulk.staged) != 0 {
		t.Errorf("a run with 1 MiB at the config root staged %v out of the snapshot", bulk.staged)
	}
	if carriedFile(t, env, "Metadata/Posters/poster.jpg") {
		t.Error("a run with 1 MiB at the config root staged artwork it cannot hold")
	}
	if !carriedFile(t, env, "Preferences.xml") {
		t.Error("the run staged no preferences: the state it extracted is what it carries")
	}
	row := shown.Instances[0]
	if !strings.Contains(row.Carry, "its state only") {
		t.Errorf("the row says it carries %q", row.Carry)
	}
	if !strings.Contains(row.Short, "needs") || !strings.Contains(row.Short, "is free at") {
		t.Errorf("the row does not say why it carries less: %+v", row)
	}
}

func TestThePassBeforeAnInstallCountsTheSnapshotAndStagesNoneOfIt(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	env.Disk = shortDisk{free: 1 << 40}
	agreed, err := Survey(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if len(bulk.measured) == 0 || len(bulk.staged) != 0 {
		t.Errorf("the pre-install pass measured %v and staged %v, want the snapshot counted and nothing staged", bulk.measured, bulk.staged)
	}
	var row InstanceSummary
	for _, in := range agreed.Instances {
		if in.App == "plex" {
			row = in
		}
	}
	if row.Staged.Files != bulk.files || row.Staged.Bytes != bulk.bytes {
		t.Errorf("the row counts %+v, want the snapshot's %d files and %d bytes", row.Staged, bulk.files, bulk.bytes)
	}
	if row.Deferred == "" {
		t.Error("the pass the install has not happened yet says nothing about Plex's check")
	}
	if !strings.Contains(row.Carry, "its whole data") {
		t.Errorf("the row says the snapshot carries %q, and it holds the tree", row.Carry)
	}

	// The run after the install: same plan, same snapshot, a config root with
	// room, and the summary it shows is the one the question promised.
	bulk.measured, bulk.staged = nil, nil
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if why := shown.Holds(agreed); why != "" {
		t.Errorf("the run does not hold what the question promised: %s", why)
	}
}

func TestAPlanThatSaysTheWholeDataOverAStateIsTheState(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	bulk.whole = false
	env.Disk = shortDisk{free: 1 << 40}
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(bulk.staged) != 0 {
		t.Errorf("the snapshot holds no tree, and the run staged %v", bulk.staged)
	}
	if carriedFile(t, env, "Metadata/Posters/poster.jpg") {
		t.Error("the run staged artwork its snapshot does not hold")
	}
	row := shown.Instances[0]
	if !strings.Contains(row.Carry, "its state only") {
		t.Errorf("the row says the run carries %q, and it carried the state", row.Carry)
	}
	if row.Short != "" {
		t.Errorf("the row blames the disk for a snapshot that carried no tree: %q", row.Short)
	}
	// The plan itself is not edited: the snapshot is what it is.
	if p.Apps["plex"].Carry != plan.CarryFull {
		t.Errorf("apply rewrote the plan's carry to %q", p.Apps["plex"].Carry)
	}
	// A plan is a file a person edits, so the check above has to be the bytes and
	// not the plan: the same plan over a snapshot that *does* hold the tree is
	// staged whole.
	bulk.whole = true
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestTheCarrySizeIsTheSnapshotsNumbers(t *testing.T) {
	env, p, raw, bulk := plexSplitEnv(t)
	env.Disk = shortDisk{free: bulk.bytes}
	var shown Summary
	env.Confirm = func(s Summary) (bool, error) {
		shown = s
		return true, nil
	}
	if _, err := Run(context.Background(), p, raw, env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Exactly the tree's bytes and not a margin more: the carry's margin is
	// space.Choose's, so a disk with the tree and nothing else is short.
	if len(bulk.staged) != 0 {
		t.Error("a disk with the tree's bytes and no room beside it carried the tree")
	}
	if row := shown.Instances[0]; !strings.Contains(row.Short, "needs") {
		t.Errorf("the row does not say why it carries less: %+v", row)
	}
	// The plan is not rewritten by a short config root: a later run with room
	// carries the tree.
	enc, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := plan.Parse(enc)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	if back.Apps["plex"].Carry != plan.CarryFull {
		t.Errorf("the plan's carry is %q after a short run", back.Apps["plex"].Carry)
	}
}
