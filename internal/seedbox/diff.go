package seedbox

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Kind is what happened to a key between two documents.
type Kind int

const (
	Added Kind = iota
	Removed
	Changed
)

// Change is one difference between two effective values documents, named by key
// path so a user can find it in their file.
type Change struct {
	Path string
	Kind Kind
	Old  any
	New  any
}

// Diff compares two effective values documents. It is what `box apply` shows
// before it applies anything: the differences, by key path, in the same words as
// the file. Lists are compared entry by entry, and an entry that appears or
// disappears is reported at its index.
func Diff(before, after map[string]any) []Change {
	var out []Change
	diffMaps("", before, after, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func diffMaps(prefix string, a, b map[string]any, out *[]Change) {
	for _, k := range unionKeys(a, b) {
		av, aok := a[k]
		bv, bok := b[k]
		diffValue(joinPath(prefix, k), av, bv, aok, bok, out)
	}
}

func diffValue(path string, a, b any, hasA, hasB bool, out *[]Change) {
	switch {
	case !hasA:
		emitAll(path, b, Added, out)
		return
	case !hasB:
		emitAll(path, a, Removed, out)
		return
	}
	if am, ok := a.(map[string]any); ok {
		if bm, ok := b.(map[string]any); ok {
			diffMaps(path, am, bm, out)
			return
		}
	}
	if al, ok := a.([]any); ok {
		if bl, ok := b.([]any); ok {
			diffList(path, al, bl, out)
			return
		}
	}
	if !equalValues(a, b) {
		*out = append(*out, Change{Path: path, Kind: Changed, Old: a, New: b})
	}
}

func diffList(path string, a, b []any, out *[]Change) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		diffValue(fmt.Sprintf("%s[%d]", path, i), a[i], b[i], true, true, out)
	}
	for i := n; i < len(b); i++ {
		emitAll(fmt.Sprintf("%s[%d]", path, i), b[i], Added, out)
	}
	for i := n; i < len(a); i++ {
		emitAll(fmt.Sprintf("%s[%d]", path, i), a[i], Removed, out)
	}
}

// emitAll reports a whole subtree as added or removed, one key at a time.
func emitAll(path string, v any, kind Kind, out *[]Change) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			*out = append(*out, whole(path, v, kind))
			return
		}
		for _, k := range sortedKeys(x) {
			emitAll(joinPath(path, k), x[k], kind, out)
		}
	case []any:
		if len(x) == 0 {
			*out = append(*out, whole(path, v, kind))
			return
		}
		for i, item := range x {
			emitAll(fmt.Sprintf("%s[%d]", path, i), item, kind, out)
		}
	default:
		*out = append(*out, whole(path, v, kind))
	}
}

func whole(path string, v any, kind Kind) Change {
	c := Change{Path: path, Kind: kind}
	if kind == Removed {
		c.Old = v
	} else {
		c.New = v
	}
	return c
}

// FormatChanges renders a diff as the lines of a confirmation prompt.
func FormatChanges(changes []Change) string {
	var b strings.Builder
	for i, c := range changes {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch c.Kind {
		case Added:
			fmt.Fprintf(&b, "+ %s: %s", c.Path, renderValue(c.New))
		case Removed:
			fmt.Fprintf(&b, "- %s: %s", c.Path, renderValue(c.Old))
		default:
			fmt.Fprintf(&b, "~ %s: %s -> %s", c.Path, renderValue(c.Old), renderValue(c.New))
		}
	}
	return b.String()
}

// renderValue is the short form a user reads: strings quoted so they can see the
// type, lists and maps inline. No Kubernetes words anywhere in a diff.
func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", x)
	case bool:
		return fmt.Sprintf("%t", x)
	case []any:
		parts := make([]string, len(x))
		for i := range x {
			parts[i] = renderValue(x[i])
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := make([]string, 0, len(x))
		for _, k := range sortedKeys(x) {
			parts = append(parts, k+": "+renderValue(x[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func unionKeys(a, b map[string]any) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// equalValues compares two decoded values, treating numbers of different widths
// as equal (the chart sees no difference between 1000 and 1000.0).
func equalValues(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	af, aok := asFloat(a)
	bf, bok := asFloat(b)
	return aok && bok && af == bf
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
