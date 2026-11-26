// Command release decides whether a push to main may cut a chart release, and writes
// the body of the release it cuts.
//
// A release is cut on a push to main, after CI is green (docs/box.md, "Where a release
// lives"). The decision is whether the net change since the last s5n-v* tag is only a bump
// Renovate may merge unattended: a patch or digest bump of charts/s5n/variants.yaml or
// charts/s5n/platform.yaml, and nothing else. Anything else -- a hand edit, a minor or major
// bump, any other file -- waits for the maintainer's own tag, and the job names it.
//
// The decision is a pure function over the changed paths and the two files at the tag and
// at HEAD (decide.go), so the case table is the test; a thin git wrapper builds those
// inputs. It is a hack because the decision belongs to the release workflow, which cannot
// unit-test itself.
//
// Usage:
//
//	release decide              the push's verdict, KEY=VALUE on stdout
//	release notes PREV NEW      the s5n-v* release body between two refs
//	release notes --all REF     every app image and platform pin a ref ships
//	release images BASE HEAD    whether a pull request only bumps those files
//	release overlay REF         lay those files from REF over the working tree
//
// The last two are what a pull request into main runs when it may be an image bump (see
// docs/box.md, "Automatic chart releases").
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Masterminds/semver/v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "decide":
		if len(os.Args) != 2 {
			usage()
		}
		err = decideCmd()
	case "notes":
		err = notesCmd(os.Args[2:])
	case "images":
		err = imagesCmd(os.Args[2:])
	case "overlay":
		err = overlayCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release decide | release notes <prev-ref> <new-ref> | release notes --all <ref> | release images <base> <head> | release overlay <ref>")
	os.Exit(2)
}

// decideCmd prints the verdict as KEY=VALUE on stdout, for $GITHUB_OUTPUT, and the reason
// on stderr, where it is the job's log.
func decideCmd() error {
	p := planner{root: "."}
	d, err := p.decide()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, d.Reason)
	fmt.Printf("release=%t\n", d.Release)
	fmt.Printf("version=%s\n", d.Version)
	fmt.Printf("reason=%s\n", d.Reason)
	return nil
}

// planner reads git history from a working tree: "" for the command, a temp dir for a test.
type planner struct {
	root string
}

func (p planner) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = p.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// decide builds the decision's inputs from git: the last tag, the paths that differ since
// it, and both files at each side.
func (p planner) decide() (Decision, error) {
	tag, version, err := p.lastTag()
	if err != nil {
		return Decision{}, err
	}
	if tag == "" {
		return decideRange(nil, nil, nil, nil, nil, "")
	}
	changed, err := p.changedSince(tag)
	if err != nil {
		return Decision{}, err
	}
	var files [4][]byte
	for i, side := range [4]struct{ ref, file string }{
		{tag, variantsFile}, {"HEAD", variantsFile},
		{tag, platformFile}, {"HEAD", platformFile},
	} {
		files[i], err = p.fileAt(side.ref, side.file)
		if err != nil {
			return Decision{}, err
		}
	}
	return decideRange(changed, files[0], files[1], files[2], files[3], version)
}

// lastTag is the highest s5n-v* tag by semver, and its version without the prefix; empty
// when the family has never been released.
func (p planner) lastTag() (tag, version string, err error) {
	out, err := p.git("tag", "--list", tagPrefix+"*")
	if err != nil {
		return "", "", err
	}
	var best *semver.Version
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v := strings.TrimPrefix(line, tagPrefix)
		sv, err := semver.NewVersion(v)
		if err != nil {
			continue
		}
		if best == nil || sv.GreaterThan(best) {
			best, tag, version = sv, line, v
		}
	}
	return tag, version, nil
}

// changedSince is the net diff between the tag and HEAD, so a merge commit contributes the
// files it actually changed.
func (p planner) changedSince(tag string) ([]string, error) {
	out, err := p.git("diff", "--name-only", tag, "HEAD")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// fileAt is one file's bytes at a ref. A file that did not exist there is empty, not an
// error: platform.yaml only became a chart file with the release that added it.
func (p planner) fileAt(ref, file string) ([]byte, error) {
	if _, err := p.git("cat-file", "-e", ref+":"+file); err != nil {
		return nil, nil
	}
	out, err := p.git("show", ref+":"+file)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// notesCmd prints the release body: the app images and platform pins that moved between two
// refs, through the decision's own parsers.
func notesCmd(args []string) error {
	if len(args) != 2 {
		usage()
	}
	p := planner{root: "."}
	first, newRef := args[0] == "--all", args[1]
	newVariants, err := p.fileAt(newRef, variantsFile)
	if err != nil {
		return err
	}
	newPlatform, err := p.fileAt(newRef, platformFile)
	if err != nil {
		return err
	}
	apps, err := parseVariants(newVariants)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", variantsFile, newRef, err)
	}
	pins, err := parsePins(newPlatform)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", platformFile, newRef, err)
	}
	if first {
		fmt.Print(body("", allImages(apps), allPins(pins)))
		return nil
	}

	oldVariants, err := p.fileAt(args[0], variantsFile)
	if err != nil {
		return err
	}
	oldPlatform, err := p.fileAt(args[0], platformFile)
	if err != nil {
		return err
	}
	images, err := movedImages(oldVariants, newVariants)
	if err != nil {
		return err
	}
	movedPinsList, err := movedPins(oldPlatform, newPlatform)
	if err != nil {
		return err
	}
	fmt.Print(body(args[0], images, movedPinsList))
	return nil
}

// body is the release's markdown: both sections, so a reader sees what an update brings.
// prev is the previous tag, empty for a first release.
func body(prev string, images, pins []string) string {
	var b strings.Builder
	if prev == "" {
		b.WriteString("The first s5n release: every image and platform pin it ships.\n\n")
	}
	section := func(title string, lines []string) {
		b.WriteString("### " + title + "\n\n")
		if len(lines) == 0 {
			b.WriteString("- none\n")
		}
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
		b.WriteString("\n")
	}
	if prev == "" {
		section("App images", images)
		section("Platform pins", pins)
		return b.String()
	}
	section("App images that moved since "+prev, images)
	section("Platform pins that moved since "+prev, pins)
	return b.String()
}
