package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// imagesCmd prints whether a pull request's whole change is the two chart files an automatic
// release ships, as KEY=VALUE for $GITHUB_OUTPUT with the reason on stderr.
//
// The workflow asks it of a pull request into main: an image bump has to be tested by the
// tree it will merge onto (develop), against the decision's own file set (releasedFiles),
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

// overlayCmd lays the release files from one ref over the working tree: the pull
// request's files, over the branch whose suite tests them.
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
// decide's, so a file added to it is overlaid without a second list.
func releasedPaths() []string {
	paths := make([]string, 0, len(releasedFiles))
	for path := range releasedFiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// overlay writes one file's bytes from ref over the working tree. The ref is fetched when
// the checkout lacks it, and a file the ref does not ship is an error rather than a
// deletion: removing one on develop is a maintainer's change.
func (p planner) overlay(ref, path string) error {
	if _, err := p.git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		if _, ferr := p.git("fetch", "--depth", "1", "origin", ref); ferr != nil {
			return fmt.Errorf("the checkout has no %s, and fetching it failed: %w", ref, ferr)
		}
	}
	b, err := p.fileAt(ref, path)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("%s ships no %s: an image bump changes files that are already there", ref, path)
	}
	if err := os.WriteFile(filepath.Join(p.root, path), b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "overlaid %s from %s\n", path, ref)
	return nil
}
