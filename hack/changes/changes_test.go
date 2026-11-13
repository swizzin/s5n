package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain clears every GIT_* the test process inherited before any test runs.
// Under a hook or a rebase, GIT_DIR and GIT_INDEX_FILE name the outer
// repository, and the git these tests run -- init, commit, diff -- would follow
// them out of the fixture directory into it.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			_ = os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// gitIn runs git in a fixture repository, with the environment pinned so the
// test neither reads nor needs the developer's config.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=changes test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=changes test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// headOf is a fixture repository's HEAD.
func headOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, path, body string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
}

// newRepo seeds a fixture with one commit holding a Go file and a doc.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "develop")
	writeFile(t, dir, "code.go", "package main\n")
	writeFile(t, dir, "docs/guide.md", "one\n")
	commitAll(t, dir, "seed")
	return dir, headOf(t, dir)
}

// TestDocsOnly is the case table for the rule itself: the paths decide, and
// only a set where every path is documentation may skip the e2e.
func TestDocsOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed []change
		run     bool
	}{
		{"documentation only", []change{
			{status: 'M', path: "README.md"},
			{status: 'A', path: "docs/box.md"},
			{status: 'M', path: "docs/brand/logo.svg"},
		}, false},
		{"one code path", []change{
			{status: 'M', path: "internal/box/box.go"},
		}, true},
		{"a mix", []change{
			{status: 'M', path: "docs/box.md"},
			{status: 'M', path: ".github/workflows/test.yaml"},
		}, true},
		{"nothing changed", nil, true},
		{"a doc deleted", []change{{status: 'D', path: "docs/old.md"}}, false},
		{"a code file deleted", []change{{status: 'D', path: "internal/old.go"}}, true},
		{"a rename out of docs", []change{
			{status: 'R', old: "docs/box.md", path: "box.txt"},
		}, true},
		{"a rename into docs", []change{
			{status: 'R', old: "internal/notes.go", path: "docs/notes.md"},
		}, true},
		{"a rename inside docs", []change{
			{status: 'R', old: "docs/one.md", path: "docs/two.md"},
		}, false},
		{"a copy out of docs", []change{
			{status: 'C', old: "docs/box.md", path: "charts/notes.txt"},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := docsOnly(tc.changed)
			if v.Run != tc.run {
				t.Fatalf("docsOnly = %+v, want Run=%t", v, tc.run)
			}
			if v.Reason == "" {
				t.Fatal("no reason for the log")
			}
		})
	}
}

// TestParseNameStatus pins the wire format `git diff --name-status -z` writes:
// a rename carries two paths, and reading only the second is how a file moved
// out of docs/ would pass for documentation.
func TestParseNameStatus(t *testing.T) {
	out := "M\x00README.md\x00R100\x00docs/box.md\x00box.txt\x00D\x00internal/x.go\x00"
	got, err := parseNameStatus(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{
		{status: 'M', path: "README.md"},
		{status: 'R', old: "docs/box.md", path: "box.txt"},
		{status: 'D', path: "internal/x.go"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestE2EFromGit is the wrapper's half: what a real push looks like to the
// command, including the cases where it must not claim to know.
func TestE2EFromGit(t *testing.T) {
	dir, seed := newRepo(t)
	repo := repo{root: dir}

	// A branch's first push, as GitHub reports it: no base commit at all.
	if v, err := repo.e2e(strings.Repeat("0", 40), seed); err != nil || v.Run != true {
		t.Fatalf("all-zero base: %+v, %v; want the e2e to run", v, err)
	}

	// One documentation commit: the skip this exists for.
	writeFile(t, dir, "docs/guide.md", "two\n")
	commitAll(t, dir, "docs: a second paragraph")
	docs := headOf(t, dir)
	if v, err := repo.e2e(seed, docs); err != nil || v.Run != false {
		t.Fatalf("documentation only: %+v, %v; want the e2e skipped", v, err)
	}

	// One code commit on top of it: the diff spans both, and a path that is
	// not documentation runs the e2e.
	writeFile(t, dir, "code.go", "package main\n\n// more\n")
	commitAll(t, dir, "code: a line")
	code := headOf(t, dir)
	if v, err := repo.e2e(seed, code); err != nil || v.Run != true {
		t.Fatalf("doc then code: %+v, %v; want the e2e to run", v, err)
	}

	// Nothing between base and head: no evidence to skip on.
	if v, err := repo.e2e(code, code); err != nil || v.Run != true {
		t.Fatalf("empty diff: %+v, %v; want the e2e to run", v, err)
	}

	// A force push: the base is still an object here, but head does not
	// descend from it.
	gitIn(t, dir, "commit", "-q", "--amend", "-m", "code: a line, rewritten")
	rewritten := headOf(t, dir)
	if v, err := repo.e2e(code, rewritten); err != nil || v.Run != true {
		t.Fatalf("force push: %+v, %v; want the e2e to run", v, err)
	}

	// A base this checkout does not have: nothing to diff, so nothing to skip.
	if v, err := repo.e2e(strings.Repeat("a", 40), rewritten); err != nil || v.Run != true {
		t.Fatalf("unknown base: %+v, %v; want the e2e to run", v, err)
	}
}

// TestE2ERenameOutOfDocs is the case that reads both sides of a rename: git
// lists only the new path for an ordinary diff, so a doc that became code must
// still run the e2e.
func TestE2ERenameOutOfDocs(t *testing.T) {
	dir, seed := newRepo(t)
	gitIn(t, dir, "mv", "docs/guide.md", "GUIDE.txt")
	commitAll(t, dir, "move the guide out of docs")
	if v, err := (repo{root: dir}).e2e(seed, headOf(t, dir)); err != nil || v.Run != true {
		t.Fatalf("rename out of docs: %+v, %v; want the e2e to run", v, err)
	}
}

// TestVerdictLine pins the contract with the workflow: KEY=VALUE for
// $GITHUB_OUTPUT, one line.
func TestVerdictLine(t *testing.T) {
	if got := verdictLine(Verdict{Run: true}); got != "e2e=true\n" {
		t.Errorf("verdictLine(true) = %q", got)
	}
	if got := verdictLine(Verdict{Run: false}); got != "e2e=false\n" {
		t.Errorf("verdictLine(false) = %q", got)
	}
}
