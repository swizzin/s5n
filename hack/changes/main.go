// Command changes decides whether a push needs the acceptance test.
//
// The e2e jobs are the expensive half of CI: a kind cluster each, ten to
// twenty minutes apiece. A push that only writes documentation cannot change
// what any of them asserts, and on develop a documentation commit is the
// common case, so those pushes skip them.
//
// The rule lives here rather than in a `paths:` filter because the edges are
// the whole point: a branch's first push, a force push, a rename out of docs/,
// an empty diff. A workflow cannot test its own `if:`; this can, and does
// (changes_test.go). The release decision is a command for the same reason.
//
// Usage:
//
//	changes e2e BASE HEAD   e2e=true|false on stdout, the reason on stderr
//
// The caller appends stdout to $GITHUB_OUTPUT and prints stderr as the job's
// log. Which commits BASE and HEAD are is the caller's question: CI asks it
// only for a push to develop, with BASE the head of develop's last green run.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "e2e":
		err = e2eCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "changes:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: changes e2e <base> <head>")
	os.Exit(2)
}

// e2eCmd prints the verdict as KEY=VALUE on stdout, for $GITHUB_OUTPUT, with
// the reason on stderr, where it is the job's log -- the shape hack/release
// decide prints, so a reader of either job sees the same thing.
func e2eCmd(args []string) error {
	if len(args) != 2 {
		usage()
	}
	v, err := (repo{root: "."}).e2e(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, v.Reason)
	fmt.Print(verdictLine(v))
	return nil
}

// verdictLine is the one line the workflow appends to $GITHUB_OUTPUT.
func verdictLine(v Verdict) string {
	return fmt.Sprintf("e2e=%t\n", v.Run)
}

// repo reads git history from a working tree. root is the repository root:
// "." for the command, a temp directory for a test.
type repo struct {
	root string
}

func (r repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.root
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

// e2e is the push's verdict: the e2e runs unless it is certain that only
// documentation changed.
//
// Every way of not knowing answers "run it". An all-zero base is a branch's
// first push or a tag; a base this checkout cannot resolve is a shallow clone;
// a base that is not an ancestor of head is a force push that rewrote the
// history the diff would have been against; an empty diff proves nothing about
// what the commit carries. A wasted runner costs minutes, a skipped assertion
// costs the reason CI exists.
func (r repo) e2e(base, head string) (Verdict, error) {
	if strings.Trim(base, "0") == "" {
		return Verdict{Run: true, Reason: "the e2e runs: no base commit (a branch's first push)"}, nil
	}
	ancestor, err := r.isAncestor(base, head)
	if err != nil {
		// The base is not in this checkout -- a shallow clone, or a push
		// whose base was garbage-collected -- so there is no diff to read.
		return Verdict{Run: true, Reason: fmt.Sprintf("the e2e runs: cannot tell whether %s is behind %s (%v)", short(base), short(head), err)}, nil
	}
	if !ancestor {
		return Verdict{Run: true, Reason: fmt.Sprintf("the e2e runs: %s is not an ancestor of %s (a force push)", short(base), short(head))}, nil
	}
	changed, err := r.changed(base, head)
	if err != nil {
		return Verdict{}, err
	}
	return docsOnly(changed), nil
}

// isAncestor reports whether base is an ancestor of head. git answers "no"
// with exit 1 and nothing on stderr; every other failure is an error, because
// "cannot tell" must never be read as "no, and skip".
func (r repo) isAncestor(base, head string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", base, head)
	cmd.Dir = r.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// changed is every path that differs between base and head, with git's status
// letter and both sides of a rename. `--name-only` would report only the new
// path, and a file moved out of docs/ would then look like documentation.
func (r repo) changed(base, head string) ([]change, error) {
	out, err := r.git("diff", "--name-status", "-z", "-M", base, head)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(out)
}

// short is a commit for a log line, not for an error message: full shas make
// the reason unreadable and the reader has the run in front of them.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
