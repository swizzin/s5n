package main

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is whether the e2e runs, and one line saying why -- the line the
// job logs, whether it skipped the e2e or spent the runners.
type Verdict struct {
	Run    bool
	Reason string
}

// A change is one path the push touched, as `git diff --name-status` names it:
// status is its letter (M, A, D, R, C), path is where it is now, and old is
// where it came from for a rename or a copy and empty otherwise.
type change struct {
	status byte
	old    string
	path   string
}

// docsOnly is the verdict for a set of changed paths: the e2e is skipped only
// when every one of them is documentation.
//
// Documentation is a path ending in .md anywhere, or any path under docs/.
// Nothing in this repository reads either at test or e2e time -- no go:embed of
// a .md, no test opening one, no hack/ script reading README or docs/ (hack/
// brand writes them, and nothing compares what it writes) -- so a change that
// touches only those cannot alter what an e2e asserts.
//
// A rename is documentation only when both of its paths are: a file moved out
// of docs/ changes the code that reads it, even though the only path git lists
// for an ordinary diff is the new one. A deletion counts by the path it
// removed, so deleting docs/x.md is documentation and deleting a Go file is
// not.
func docsOnly(changed []change) Verdict {
	if len(changed) == 0 {
		return Verdict{Run: true, Reason: "the e2e runs: the push changed no path"}
	}
	var other []string
	for _, c := range changed {
		if !isDoc(c.path) || (c.old != "" && !isDoc(c.old)) {
			other = append(other, c.path)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		reason := "the e2e runs: " + strings.Join(first(other, 4), ", ")
		if len(other) > 4 {
			reason += fmt.Sprintf(" and %d more", len(other)-4)
		}
		if len(other) == 1 {
			return Verdict{Run: true, Reason: reason + " is not documentation"}
		}
		return Verdict{Run: true, Reason: reason + " are not documentation"}
	}
	return Verdict{Run: false, Reason: fmt.Sprintf("the e2e is skipped: every changed path (%d) is documentation", len(changed))}
}

// isDoc reports whether a path is documentation, by the definition above.
func isDoc(path string) bool {
	return strings.HasSuffix(path, ".md") || strings.HasPrefix(path, "docs/")
}

// first is at most n of paths, for a reason line a person reads.
func first(paths []string, n int) []string {
	if len(paths) > n {
		return paths[:n]
	}
	return paths
}

// parseNameStatus reads `git diff --name-status -z`: one STATUS NUL path NUL
// per change, and STATUS NUL old NUL new NUL for a rename or a copy, with the
// status carrying git's similarity score (R100).
func parseNameStatus(out string) ([]change, error) {
	fields := strings.Split(out, "\x00")
	var changed []change
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" { // the trailing NUL, and nothing else can be empty
			i++
			continue
		}
		i++
		if i >= len(fields) {
			return nil, fmt.Errorf("unterminated status %q in git diff", status)
		}
		switch status[0] {
		case 'R', 'C':
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("unterminated rename %q in git diff", status)
			}
			changed = append(changed, change{status: status[0], old: fields[i], path: fields[i+1]})
			i += 2
		default:
			changed = append(changed, change{status: status[0], path: fields[i]})
			i++
		}
	}
	return changed, nil
}
