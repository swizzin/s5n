package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImagesIsOnlyTheReleaseFiles pins the question a pull request into main is
// asked: is this branch nothing but an image bump? The set is decide's own, so
// the cases are the same files a release may ship, and everything else is a
// refusal -- including Renovate's own config, which is inert for a release but
// is not an image.
func TestImagesIsOnlyTheReleaseFiles(t *testing.T) {
	dir := seedRepo(t)
	seed, err := planner{root: dir}.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	base := strings.TrimSpace(seed)

	commit := func(paths map[string][]byte, msg string) string {
		for path, data := range paths {
			writeFile(t, dir, path, data)
		}
		commitAll(t, dir, msg)
		out, cerr := planner{root: dir}.git("rev-parse", "HEAD")
		if cerr != nil {
			t.Fatalf("rev-parse: %v", cerr)
		}
		return strings.TrimSpace(out)
	}

	tests := []struct {
		name   string
		paths  map[string][]byte
		images bool
		reason string
	}{
		{
			name:   "an app image patch",
			paths:  map[string][]byte{variantsFile: variantsYAML("5.0.0.2910", hex64("a"))},
			images: true,
			reason: variantsFile,
		},
		{
			name:   "a platform pin patch, beside an image",
			paths:  map[string][]byte{variantsFile: variantsYAML("5.0.0.2911", hex64("a")), platformFile: platformYAML("1.9.3", "0.16.2", "4.39.28")},
			images: true,
			reason: platformFile,
		},
		{
			name:   "a bump with a hand edit beside it",
			paths:  map[string][]byte{variantsFile: variantsYAML("5.0.0.2912", hex64("a")), "internal/foo.go": []byte("package foo\n")},
			images: false,
			reason: "internal/foo.go",
		},
		{
			name:   "Renovate's own config",
			paths:  map[string][]byte{".github/renovate.json": []byte("{}\n")},
			images: false,
			reason: ".github/renovate.json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			head := commit(tc.paths, tc.name)
			got, reason, err := planner{root: dir}.images(base, head)
			if err != nil {
				t.Fatalf("images: %v", err)
			}
			if got != tc.images {
				t.Fatalf("images = %v, want %v (%s)", got, tc.images, reason)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason = %q, want it to name %q", reason, tc.reason)
			}
		})
	}
}

// TestImagesRefusesAnEmptyPullRequest: a branch that changes nothing has no
// bump to test, and the reason says so rather than claiming an image moved.
func TestImagesRefusesAnEmptyPullRequest(t *testing.T) {
	dir := seedRepo(t)
	got, reason, err := planner{root: dir}.images("HEAD", "HEAD")
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	if got {
		t.Fatalf("an empty diff is an image bump: %s", reason)
	}
	if !strings.Contains(reason, "nothing") {
		t.Fatalf("reason = %q, want it to say the pull request changes nothing", reason)
	}
}

// TestImagesIsTheBranchesOwnDiff: the question is what the branch changed, not
// what the base did after it forked. Base moves on with an unrelated commit; the
// branch's own commit is still only the images, so the pull request is a bump.
func TestImagesIsTheBranchesOwnDiff(t *testing.T) {
	dir := seedRepo(t)
	p := planner{root: dir}
	base, err := p.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	base = strings.TrimSpace(base)

	gitIn(t, dir, "checkout", "-b", "renovate/app-images")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2910", hex64("a")))
	commitAll(t, dir, "chore(deps): update sonarr to 5.0.0.2910")
	head, err := p.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	head = strings.TrimSpace(head)

	// The base branch moves on while the pull request is open: a reading that
	// diffed the two tips without their merge base would see that commit too.
	gitIn(t, dir, "checkout", "main")
	writeFile(t, dir, "internal/foo.go", []byte("package foo\n"))
	commitAll(t, dir, "a maintainer's work on main, after the branch forked")

	got, reason, err := p.images(base, head)
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	if !got {
		t.Fatalf("images = false for a branch that only bumps: %s", reason)
	}
}

// TestOverlayLaysThePullsFilesOverTheTree: the develop checkout keeps develop's
// own files, and the pull request's two go over them -- nothing else moves.
func TestOverlayLaysThePullsFilesOverTheTree(t *testing.T) {
	dir := seedRepo(t)
	p := planner{root: dir}
	gitIn(t, dir, "checkout", "-b", "renovate/app-images")
	bumpedVariants := variantsYAML("5.0.0.2910", hex64("a"))
	bumpedPlatform := platformYAML("1.9.3", "0.16.2", "4.39.28")
	writeFile(t, dir, variantsFile, bumpedVariants)
	writeFile(t, dir, platformFile, bumpedPlatform)
	commitAll(t, dir, "chore(deps): bump sonarr and metallb")
	branch, err := p.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	branch = strings.TrimSpace(branch)

	// The run checks develop out: its own files, which the overlay replaces.
	gitIn(t, dir, "checkout", "main")
	writeFile(t, dir, variantsFile, variantsYAML("5.0.0.2900", hex64("f")))
	writeFile(t, dir, platformFile, platformYAML("1.9.2", "0.16.0", "4.39.27"))
	commitAll(t, dir, "develop moves on")

	for _, path := range releasedPaths() {
		if err := p.overlay(branch, path); err != nil {
			t.Fatalf("overlay %s: %v", path, err)
		}
	}
	for path, want := range map[string][]byte{variantsFile: bumpedVariants, platformFile: bumpedPlatform} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s is not the branch's file after the overlay:\n%s", path, got)
		}
	}
	// Only those two: an overlay that staged or restored anything else would
	// change what the suite underneath is testing.
	status, err := p.git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var dirty []string
	for _, line := range strings.Split(status, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dirty = append(dirty, line[2:])
		}
	}
	if strings.Join(dirty, ",") != strings.Join(releasedPaths(), ",") {
		t.Fatalf("the overlay changed %v, want only %v", dirty, releasedPaths())
	}
}

// TestOverlayRefusesAFileTheRefDoesNotShip: an image bump replaces files that are
// there. A ref that does not ship one is a refusal naming it, never a deletion
// of the tree's copy.
func TestOverlayRefusesAFileTheRefDoesNotShip(t *testing.T) {
	dir := seedRepo(t)
	p := planner{root: dir}
	gitIn(t, dir, "rm", "-q", platformFile)
	commitAll(t, dir, "a tree from before platform.yaml")
	ref, err := p.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	err = p.overlay(strings.TrimSpace(ref), platformFile)
	if err == nil {
		t.Fatal("the overlay accepted a ref with no platform.yaml")
	}
	if !strings.Contains(err.Error(), platformFile) {
		t.Fatalf("the refusal does not name %s: %v", platformFile, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, platformFile)); statErr == nil {
		// It was committed away, so a working tree without it is right; what
		// matters is that the overlay did not invent one.
		t.Fatal("the overlay wrote a file the ref does not ship")
	}
}

// TestOverlayFetchesARefTheCheckoutDoesNotHave: the run checks out the branch
// under test, and the pull request's commit is not in that checkout. The overlay
// fetches it from origin rather than refusing.
func TestOverlayFetchesARefTheCheckoutDoesNotHave(t *testing.T) {
	origin := t.TempDir()
	gitIn(t, origin, "init", "--bare", "-b", "main")

	work := t.TempDir()
	gitIn(t, work, "init", "-b", "main")
	writeFile(t, work, variantsFile, variantsYAML("5.0.0.2907", hex64("a")))
	writeFile(t, work, platformFile, platformYAML("1.9.3", "0.16.1", "4.39.28"))
	commitAll(t, work, "seed the tree")
	gitIn(t, work, "remote", "add", "origin", origin)
	gitIn(t, work, "push", "-q", "origin", "main")

	// The pull request's commit, on a branch of its own, pushed where the run
	// can fetch it.
	gitIn(t, work, "checkout", "-b", "renovate/app-images")
	bumped := variantsYAML("5.0.0.2910", hex64("a"))
	writeFile(t, work, variantsFile, bumped)
	commitAll(t, work, "chore(deps): update sonarr to 5.0.0.2910")
	gitIn(t, work, "push", "-q", "origin", "renovate/app-images")
	prHead, err := planner{root: work}.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	prHead = strings.TrimSpace(prHead)

	// The origin serves a fetch by commit, as GitHub does (a review tool's
	// "fetch this pull request's head" is the same request). file:// is what
	// makes the clone shallow: a local path is cloned by hardlink, depth and
	// all ignored.
	gitIn(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	// The run's own checkout: one branch, one commit, exactly what the reusable
	// workflow's checkout hands the jobs it runs.
	run := t.TempDir()
	gitIn(t, work, "clone", "-q", "--depth", "1", "--single-branch", "--branch", "main", "file://"+origin, run)
	writeFile(t, run, variantsFile, variantsYAML("5.0.0.2900", hex64("f")))
	commitAll(t, run, "the branch under test")

	p := planner{root: run}
	if _, err := p.git("rev-parse", "--verify", "--quiet", prHead+"^{commit}"); err == nil {
		t.Fatal("the fixture already has the pull request's commit: nothing is fetched")
	}
	if err := p.overlay(prHead, variantsFile); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(run, variantsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bumped) {
		t.Fatalf("%s is not the fetched ref's file:\n%s", variantsFile, got)
	}
}
