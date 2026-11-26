package plan

import (
	"fmt"
	"sort"
	"strings"
)

// Error is a plan this box refuses, with every problem found in it rather than
// the first: a hand-edited file should need one pass.
type Error struct {
	// Path is the file the plan was read from, empty when parsed from memory.
	Path string
	// Problems are the lines the operator has to fix.
	Problems []string
}

func (e *Error) Error() string {
	name := "plan"
	if e.Path != "" {
		name += " " + e.Path
	}
	if len(e.Problems) == 1 {
		return name + ": " + e.Problems[0]
	}
	return name + ":\n  " + strings.Join(e.Problems, "\n  ")
}

// Uncovered is one absolute path a rewrite met with no rule behind it: the app
// whose table was asked, the file and key it was found in, and the path.
type Uncovered struct {
	App  string
	File string
	Key  string
	Path string
}

// UncoveredError is every gap found in one run: a config tree holds thousands of
// stored paths, so failing on the first would cost an edit and a re-run per gap
// while the source keeps running. Nothing is written while any gap exists.
type UncoveredError struct {
	Paths []Uncovered
}

func (e *UncoveredError) Error() string {
	gaps := e.sorted()
	count := fmt.Sprintf("%d paths", len(gaps))
	if len(gaps) == 1 {
		count = "1 path"
	}
	var b strings.Builder
	b.WriteString(count)
	b.WriteString(" no rule covers; add each to its app's paths in the plan:")
	for _, g := range gaps {
		b.WriteString("\n  ")
		b.WriteString(g.String())
	}
	return b.String()
}

// sorted is the gaps in a stable order, a duplicate listed once.
func (e *UncoveredError) sorted() []Uncovered {
	out := append([]Uncovered(nil), e.Paths...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.App != b.App:
			return a.App < b.App
		case a.File != b.File:
			return a.File < b.File
		case a.Key != b.Key:
			return a.Key < b.Key
		}
		return a.Path < b.Path
	})
	// Sorting is what makes a duplicate adjacent.
	deduped := out[:0]
	for i, g := range out {
		if i > 0 && g == out[i-1] {
			continue
		}
		deduped = append(deduped, g)
	}
	return deduped
}

// String renders one gap: where it is, then the path.
func (u Uncovered) String() string {
	where := u.App
	if u.File != "" {
		where += " " + u.File
	}
	if u.Key != "" {
		where += " " + u.Key
	}
	return where + ": " + u.Path
}
