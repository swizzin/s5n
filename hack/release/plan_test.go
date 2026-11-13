package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain clears every GIT_* the test process inherited before any test runs.
// Under `git rebase --exec`, or a hook, GIT_DIR and GIT_INDEX_FILE name the
// outer repository, and every git these tests start -- the fixtures' and the
// release tool's own -- would follow them out of the fixture directory: `init`
// re-initialises that repository (core.bare = true), and the fixtures' commits
// and tags land in it.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			_ = os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// gitIn runs git in a fixture repository. The environment is pinned so the test
// does not read the developer's git config and does not need one.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=release test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=release test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, dir, path string, data []byte) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitAll stages and commits the whole tree in a fixture repository.
func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", msg)
}

// seedRepo builds a fixture history: the two chart files committed and tagged
// s5n-v0.2.0, so a later commit is what a push to main would see.
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2907", hex64("a")))
	writeFile(t, dir, platformFile, platformYAML("1.9.3", "0.16.1", "4.39.28"))
	commitAll(t, dir, "seed the tree")
	gitIn(t, dir, "tag", "s5n-v0.2.0")
	return dir
}

// TestDecideFromGitHistory drives the git wrapper the workflow calls: a
// Renovate patch bump of the chart's images, and nothing else, is a release.
func TestDecideFromGitHistory(t *testing.T) {
	dir := seedRepo(t)
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	commitAll(t, dir, "chore(deps): update sonarr to 5.0.0.2910")

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !got.Release || got.Version != "0.2.1" {
		t.Fatalf("decision = %+v, want a release of 0.2.1", got)
	}
}

// TestDecideAcrossMergeCommits: the decision is the net diff since the tag, so
// two Renovate merges (or a maintainer's merge commits) whose net change is
// only bumps are one release, with no merge case of its own.
func TestDecideAcrossMergeCommits(t *testing.T) {
	dir := seedRepo(t)

	// A Renovate branch for the app images, merged with a merge commit.
	gitIn(t, dir, "checkout", "-b", "renovate/app-images")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	commitAll(t, dir, "chore(deps): update sonarr to 5.0.0.2910")
	gitIn(t, dir, "checkout", "main")
	gitIn(t, dir, "merge", "--no-ff", "-m", "Merge pull request #1 from renovate/app-images", "renovate/app-images")

	// And one for a platform pin, merged on top of it.
	gitIn(t, dir, "checkout", "-b", "renovate/metallb")
	writeFile(t, dir, platformFile, platformYAML("1.9.3", "0.16.2", "4.39.28"))
	commitAll(t, dir, "chore(deps): update metallb to 0.16.2")
	gitIn(t, dir, "checkout", "main")
	gitIn(t, dir, "merge", "--no-ff", "-m", "Merge pull request #2 from renovate/metallb", "renovate/metallb")

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !got.Release || got.Version != "0.2.1" {
		t.Fatalf("decision = %+v, want one release of 0.2.1 across both merges", got)
	}
}

// TestDecideRefusesAnUnrelatedCommit: a commit that touches anything but the
// two chart files means no automatic release, and the reason names the file.
func TestDecideRefusesAnUnrelatedCommit(t *testing.T) {
	dir := seedRepo(t)
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	writeFile(t, dir, "internal/foo.go", []byte("package foo\n"))
	commitAll(t, dir, "chart bump with a hand edit beside it")

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Release {
		t.Fatalf("decision = %+v, want a refusal for the hand edit", got)
	}
	if !strings.Contains(got.Reason, "internal/foo.go") {
		t.Fatalf("reason = %q, want it to name internal/foo.go", got.Reason)
	}
}

// TestDecideNoCommitsSinceTag: main has not moved, so there is nothing to
// release.
func TestDecideNoCommitsSinceTag(t *testing.T) {
	dir := seedRepo(t)

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Release {
		t.Fatalf("decision = %+v, want a refusal with no commits since the tag", got)
	}
}

// TestDecideWithoutS5nTag: v0.2.0 predates the s5n-v family, so it is not a
// base to count a patch from and nothing is released.
func TestDecideWithoutS5nTag(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2907", hex64("a")))
	commitAll(t, dir, "seed the tree")
	gitIn(t, dir, "tag", "v0.2.0")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	commitAll(t, dir, "chore(deps): update sonarr to 5.0.0.2910")

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Release {
		t.Fatalf("decision = %+v, want a refusal with only v0.2.0 tagged", got)
	}
	if !strings.Contains(got.Reason, "v0.2.0") {
		t.Fatalf("reason = %q, want it to say v0.2.0 is not a base", got.Reason)
	}
}

// TestDecidePlatformAddedSinceTag: platform.yaml did not exist at the tag, and
// a file that did not exist is empty rather than an error. Adding it is a pin
// added, which the maintainer releases, not the push.
func TestDecidePlatformAddedSinceTag(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2907", hex64("a")))
	commitAll(t, dir, "seed the tree")
	gitIn(t, dir, "tag", "s5n-v0.2.0")
	writeFile(t, dir, platformFile, platformYAML("1.9.3", "0.16.1", "4.39.28"))
	commitAll(t, dir, "the platform pins ship in the chart")

	got, err := planner{root: dir}.decide()
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Release {
		t.Fatalf("decision = %+v, want a refusal: the pins are new, so this is a maintainer's release", got)
	}
	if !strings.Contains(got.Reason, "pin") || !strings.Contains(got.Reason, "added") {
		t.Fatalf("reason = %q, want it to say a pin was added", got.Reason)
	}
}

// TestNotesBetweenRefs: the release body names both what moved in the images
// and what moved in the platform pins, read from the tag and the release ref.
func TestNotesBetweenRefs(t *testing.T) {
	dir := seedRepo(t)
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	writeFile(t, dir, platformFile, platformYAML("1.9.3", "0.16.2", "4.39.28"))
	commitAll(t, dir, "chore(deps): bump sonarr and metallb")
	gitIn(t, dir, "tag", "s5n-v0.2.1")

	p := planner{root: dir}
	oldVariants, err := p.fileAt("s5n-v0.2.0", variantsFile)
	if err != nil {
		t.Fatalf("fileAt: %v", err)
	}
	newVariants, err := p.fileAt("s5n-v0.2.1", variantsFile)
	if err != nil {
		t.Fatalf("fileAt: %v", err)
	}
	oldPlatform, err := p.fileAt("s5n-v0.2.0", platformFile)
	if err != nil {
		t.Fatalf("fileAt: %v", err)
	}
	newPlatform, err := p.fileAt("s5n-v0.2.1", platformFile)
	if err != nil {
		t.Fatalf("fileAt: %v", err)
	}
	images, err := movedImages(oldVariants, newVariants)
	if err != nil {
		t.Fatalf("movedImages: %v", err)
	}
	pins, err := movedPins(oldPlatform, newPlatform)
	if err != nil {
		t.Fatalf("movedPins: %v", err)
	}
	out := body("s5n-v0.2.0", images, pins)
	for _, want := range []string{"sonarr: 5.0.0.2907 -> 5.0.0.2910", "metallb: 0.16.1 -> 0.16.2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("body = %q, want %q", out, want)
		}
	}
}
