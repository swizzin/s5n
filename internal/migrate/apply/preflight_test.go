package apply

// preflight_test.go pins every refusal the read-only half makes. Each case
// checks two things: that the message names what the operator has to fix, and
// that the refusal wrote nothing -- a preflight that staged first would leave a
// copy of someone's config beside the one the next run refuses over.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

func TestPreflightRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fixture, env *Env)
		want   string
	}{
		{
			name: "data root is not the grant's path",
			mutate: func(f *fixture, env *Env) {
				env.DataRoot = filepath.Join(f.root, "elsewhere")
			},
			want: "storage.data.path",
		},
		{
			name: "the instance is already running",
			mutate: func(f *fixture, env *Env) {
				env.Enabled = func(name string) bool { return name == "sonarr" }
			},
			want: "already runs sonarr",
		},
		{
			name: "the instance directory holds something",
			mutate: func(f *fixture, env *Env) {
				writeFile(t, filepath.Join(f.configRoot, "sonarr", "sonarr.db"), "x", 0o600)
			},
			want: "is not empty",
		},
		{
			name: "the source config is gone",
			mutate: func(f *fixture, env *Env) {
				if err := os.RemoveAll(filepath.Join(f.configs, "sonarr")); err != nil {
					panic(err)
				}
			},
			want: "is not a directory",
		},
		{
			// The plan's `config:` is where the source kept the app's settings,
			// and apply never reads it: the snapshot is the only place configs
			// come from, so a source host that is still mounted changes nothing.
			name: "the plan names a source path the snapshot does not carry",
			mutate: func(f *fixture, env *Env) {
				// A directory at exactly the path the plan names, and no config
				// for the app in the snapshot.
				writeFile(t, filepath.Join(f.root, "srv", "apps", "sonarr", "config.xml"), sonarrConfigXML, 0o600)
				if err := os.RemoveAll(filepath.Join(f.configs, "sonarr")); err != nil {
					panic(err)
				}
			},
			want: "the snapshot holds no config for sonarr",
		},
		{
			name: "the app is not in the chart's catalog",
			mutate: func(f *fixture, env *Env) {
				env.Catalog = []string{"radarr", "qbittorrent"}
			},
			want: "no app in this chart's catalog is named that",
		},
		{
			name: "the release has no endpoint for the instance",
			mutate: func(f *fixture, env *Env) {
				delete(env.Targets, "qbittorrent")
			},
			want: "the release has no endpoint for it",
		},
		{
			name: "the config root is not there",
			mutate: func(f *fixture, env *Env) {
				if err := os.RemoveAll(f.configRoot); err != nil {
					panic(err)
				}
			},
			want: "storage.config.path",
		},
		{
			name: "the rename target holds something",
			mutate: func(f *fixture, env *Env) {
				writeFile(t, filepath.Join(f.dataRoot, "downloads", "old.mkv"), "x", 0o600)
			},
			want: "is not empty",
		},
		{
			// A file anywhere under the target is the operator's data, however
			// deep: the box's own directories hold no files at all.
			name: "the rename target holds a file below a category directory",
			mutate: func(f *fixture, env *Env) {
				writeFile(t, filepath.Join(f.dataRoot, "downloads", "movies", "archived.mkv"), "x", 0o600)
			},
			want: "is not empty",
		},
		{
			name: "the downloads directory the plan names is gone",
			mutate: func(f *fixture, env *Env) {
				if err := os.RemoveAll(filepath.Join(f.dataRoot, "Downloads")); err != nil {
					panic(err)
				}
			},
			want: "is not a directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p, raw := f.plan()
			env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
			tc.mutate(f, &env)
			before := hashTree(t, f.root)
			err := Preflight(p, raw, env)
			if err == nil {
				t.Fatal("preflight accepted it")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %q:\n%s", tc.want, err)
			}
			// A preflight writes nothing, and it is called before staging.
			sameTree(t, "the host", before, hashTree(t, f.root))
		})
	}
}

// TestPreflightAcceptsTheBoxsOwnRenameTarget: the release's wiring Job makes
// /data/downloads/<category> for every Arr the seedbox runs, so a box that was
// installed with any Arr on hands migrate a target that exists and holds only
// empty directories. That is not the operator's data to move: the rename
// removes it and proceeds.
func TestPreflightAcceptsTheBoxsOwnRenameTarget(t *testing.T) {
	f := newFixture(t)
	for _, dir := range []string{"downloads/movies", "downloads/tv/subdir"} {
		if err := os.MkdirAll(filepath.Join(f.dataRoot, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, raw := f.plan()
	if err := Preflight(p, raw, f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))); err != nil {
		t.Fatalf("preflight refused the box's own empty category directories: %v", err)
	}
}

// TestPreflightCollectsEveryProblem: one pass names everything, not the first
// thing, because the plan is a page a person edits.
func TestPreflightCollectsEveryProblem(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	env.DataRoot = filepath.Join(f.root, "elsewhere")
	env.Enabled = func(string) bool { return true }
	err := Preflight(p, raw, env)
	if err == nil {
		t.Fatal("preflight accepted it")
	}
	for _, want := range []string{"storage.data.path", "already runs qbittorrent", "already runs sonarr"} {
		if !contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err)
		}
	}
}

// TestPreflightRefusesAHalfDoneOtherPlan: two plans share two roots, so the
// second has to wait for the first to finish.
func TestPreflightRefusesAHalfDoneOtherPlan(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	other := stateDir(f.configRoot, "0123456789ab")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(State{Plan: "a-different-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "state.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Preflight(p, raw, env)
	if err == nil {
		t.Fatal("preflight accepted a plan beside a half-done one")
	}
	if !contains(err.Error(), "another migration is half done") || !contains(err.Error(), other) {
		t.Errorf("the refusal does not name the unfinished plan:\n%s", err)
	}

	// A record that finished does not block anything.
	done, err := json.Marshal(State{Plan: "a-different-plan", Values: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "state.json"), done, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(p, raw, env); err != nil {
		t.Errorf("preflight refused a plan beside a finished one: %v", err)
	}
}

// TestPreflightRefusesWhateverTheRecordSaysWhenTheRootsDisagree: the state file
// excuses the two checks it records (the instance directory and the renames),
// and nothing else.
func TestPreflightExcusesOnlyWhatItRecorded(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	// A run that moved both instances and renamed the downloads, then stopped.
	dir := stateDir(f.configRoot, PlanID(raw))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := &State{Plan: PlanDigest(raw), Renamed: []string{"Downloads"}, Moved: []string{"qbittorrent", "sonarr"}}
	if err := saveState(dir, st); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sonarr", "qbittorrent"} {
		if err := os.MkdirAll(filepath.Join(f.configRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The rename is recorded, so the plan's source name being gone is expected.
	if err := os.Rename(filepath.Join(f.dataRoot, "Downloads"), filepath.Join(f.dataRoot, "downloads")); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(p, raw, env); err != nil {
		t.Errorf("preflight refused a plan its own record says is half done: %v", err)
	}
}

// testApp is the app name the "a write that disagrees with its check" case uses.
// It is registered once, under a name no chart catalog has, so the failure is
// about the write pass and not about a real rewriter.
const testApp = "testapp"

func init() { rewrite.Register(testApp, liar{}) }

// liar reports one change on Check and two on Write, which is the one thing a
// rewriter must never do.
type liar struct{}

func (liar) Check(context.Context, rewrite.Input) (rewrite.Report, error) {
	return rewrite.Report{Changes: []rewrite.Change{{File: "x.db", Where: "T.C", What: "path", Count: 1}}}, nil
}

func (liar) Write(context.Context, rewrite.Input) (rewrite.Report, error) {
	return rewrite.Report{Changes: []rewrite.Change{{File: "x.db", Where: "T.C", What: "path", Count: 2}}}, nil
}

// TestRunRefusesAWriteThatDiffersFromItsCheck: the second pass is what the
// operator approved, and anything else is a rewriter bug that must not reach
// the config root.
func TestRunRefusesAWriteThatDiffersFromItsCheck(t *testing.T) {
	f := newFixture(t)
	doc := "version: 2\napps:\n  " + testApp + ":\n    kind: kubernetes\n    source: " + testApp +
		"\n    config: /srv/apps/" + testApp + "\n    ports: [9999]\n    paths: []\n"
	if err := os.MkdirAll(filepath.Join(f.configs, testApp), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse the plan: %v", err)
	}
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	env.Catalog = []string{testApp}
	env.Targets = map[string]rewrite.Endpoint{testApp: {Host: testApp, Port: 9999}}
	_, err = Run(context.Background(), p, []byte(doc), env)
	if err == nil {
		t.Fatal("the run accepted a write that made more changes than its check")
	}
	if !contains(err.Error(), testApp) || !contains(err.Error(), "x.db") {
		t.Errorf("the refusal does not name the app and the change:\n%s", err)
	}
	if exists(filepath.Join(f.configRoot, testApp)) {
		t.Error("the config was moved although the write disagreed with its check")
	}
}
