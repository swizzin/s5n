package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// imagesCmd prints whether a pull request's whole change is the two chart files an automatic
// release ships, as KEY=VALUE for $GITHUB_OUTPUT with the reason on stderr.
//
// The workflow asks it of a pull request into main: an image bump has to be tested by the
// tree it will merge into (develop), against the decision's own file set (releasedFiles),
// not a second list in YAML.
func imagesCmd(args []string) error {
	if len(args) != 2 {
		usage()
	}
	p := planner{root: "."}
	images, reason, err := p.images(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, reason)
	fmt.Printf("images=%t\n", images)
	return nil
}

// images: every path that differs between a pull request's base and head is a file an
// automatic release ships. The diff is the three-dot one (what the branch changed), and an
// empty diff is nothing to test. Renovate's config does not count: it is inert.
func (p planner) images(base, head string) (bool, string, error) {
	base, head = strings.TrimSpace(base), strings.TrimSpace(head)
	if base == "" || head == "" {
		return false, "", errors.New("images needs a base and a head commit")
	}
	out, err := p.git("diff", "--name-only", base+"..."+head)
	if err != nil {
		return false, "", err
	}
	var changed, other []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			changed = append(changed, line)
		}
	}
	for _, path := range changed {
		if !releasedFiles[path] {
			other = append(other, path)
		}
	}
	switch {
	case len(changed) == 0:
		return false, "the pull request changes nothing", nil
	case len(other) > 0:
		sort.Strings(other)
		return false, "changed " + strings.Join(other, ", ") + " (an image bump moves only " +
			variantsFile + " and " + platformFile + ")", nil
	}
	sort.Strings(changed)
	return true, "only " + strings.Join(changed, ", ") + " changed", nil
}

// overlayCmd merges the release files from one ref into the working tree: the
// pull request's files, into the branch whose suite tests them.
func overlayCmd(args []string) error {
	if len(args) != 1 {
		usage()
	}
	p := planner{root: "."}
	for _, path := range releasedPaths() {
		if err := p.overlay(args[0], path); err != nil {
			return err
		}
	}
	return nil
}

// releasedPaths is every path an automatic release may move, sorted: the set is
// decide's, so a file added to it is merged without a second list.
func releasedPaths() []string {
	paths := make([]string, 0, len(releasedFiles))
	for path := range releasedFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// overlay writes what a three-way merge of ref's file into the working tree would write,
// their merge base as the ancestor: a copy would drop what develop added since the ref
// forked. A conflict is refused, naming the file, with the tree left alone.
func (p planner) overlay(ref, path string) error {
	if err := p.fetchForMerge(ref); err != nil {
		return err
	}
	base, err := p.mergeBase(ref)
	if err != nil {
		return err
	}
	theirs, err := p.fileAt(ref, path)
	if err != nil {
		return err
	}
	if theirs == nil {
		return fmt.Errorf("%s ships no %s: an image bump changes files that are already there", ref, path)
	}
	ancestor, err := p.fileAt(base, path)
	if err != nil {
		return err
	}
	ours, err := os.ReadFile(filepath.Join(p.root, path))
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		ours = nil
	}
	merged, clean, err := mergeFile(ours, ancestor, theirs)
	if err != nil {
		return fmt.Errorf("merging %s from %s: %w", path, ref, err)
	}
	if !clean {
		return fmt.Errorf("%s: a back-merge of %s would conflict with this tree; resolve it in the bump before testing it here", path, ref)
	}
	if err := os.WriteFile(filepath.Join(p.root, path), merged, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "merged %s from %s\n", path, ref)
	return nil
}

// fetchForMerge gives the checkout the history a merge base needs: the callers
// differ in depth (test.yaml: some fetch-depth 0, some the default 1) and a
// shallow checkout has no merge base at all, so it deepens itself.
func (p planner) fetchForMerge(ref string) error {
	out, err := p.git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "true" {
		if _, err := p.git("fetch", "--unshallow", "origin"); err != nil {
			return fmt.Errorf("the checkout is shallow, and deepening it failed: %w", err)
		}
	}
	if _, err := p.git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		if _, ferr := p.git("fetch", "origin", ref); ferr != nil {
			return fmt.Errorf("the checkout has no %s, and fetching it failed: %w", ref, ferr)
		}
	}
	return nil
}

// mergeBase is the commit a merge of ref into the tree under test starts from.
// The fetch above is what makes one exist; without it there is nothing to merge
// against and refusing is the only honest answer.
func (p planner) mergeBase(ref string) (string, error) {
	out, err := p.git("merge-base", "HEAD", ref)
	base := strings.TrimSpace(out)
	if err != nil || base == "" {
		return "", fmt.Errorf("%s and this checkout share no merge base: the bump cannot be merged into the tree that would test it", ref)
	}
	return base, nil
}

// mergeFile is git's own three-way merge for one file, so the result is what a
// back-merge would produce down to the conflict markers. A missing side is
// written empty, as git treats an absent file on one side.
func mergeFile(ours, base, theirs []byte) ([]byte, bool, error) {
	dir, err := os.MkdirTemp("", "s5n-overlay")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	write := func(name string, b []byte) (string, error) {
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, b, 0o600)
	}
	oursPath, err := write("ours", ours)
	if err != nil {
		return nil, false, err
	}
	basePath, err := write("base", base)
	if err != nil {
		return nil, false, err
	}
	theirsPath, err := write("theirs", theirs)
	if err != nil {
		return nil, false, err
	}
	cmd := exec.Command("git", "merge-file", "-p", oursPath, basePath, theirsPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		// 1 to 127 is the number of conflicts, and the markers are on stdout;
		// anything else (128+) is git failing, which is not a verdict.
		if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 128 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("git merge-file: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), true, nil
}
