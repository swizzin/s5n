package release

import (
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/seedbox"
)

// A transient answer is one that outlives no file: `box init` asks for the Plex
// claim token after the confirm, hands it to this one apply, and never writes it
// to the grant or the values file (docs/box.md, "The catalog and box"). Two
// things follow, and they live here so Apply's main path stays readable.

// withoutTransient drops the changes that name a transient answer. Helm stores
// the values it rendered with, so the previous revision holds the last answer;
// comparing it with the file's empty default would print the secret on the very
// next run. Dropping it is also what keeps a re-run a no-op.
func withoutTransient(changes []seedbox.Change, keys []string) []seedbox.Change {
	if len(keys) == 0 || len(changes) == 0 {
		return changes
	}
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	out := changes[:0]
	for _, c := range changes {
		if drop[c.Path] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// mergeTransient sets the answers from AfterConfirm into the document handed to
// Helm, by dotted values path. An empty answer is skipped: an optional question
// the operator pressed enter on is not a value, and the chart treats an empty
// string as absent anyway.
//
// A transient answer may not set a grant key. The manifest cannot ask for one
// (internal/catalog refuses that), but this does not depend on the manifest
// being right: an answer that arrives after the confirm is still an answer, and
// the grant is root's.
func mergeTransient(values map[string]any, extra map[string]any) error {
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := extra[key]
		if value == nil {
			continue
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		path, err := seedbox.ParsePath(key)
		if err != nil {
			return fmt.Errorf("transient answer %q: %w", key, err)
		}
		for _, st := range path {
			if st.IsIndex {
				return fmt.Errorf("transient answer %s names a list entry; an answer sets one key", key)
			}
		}
		if rule, ok := seedbox.GrantRule(path.Keys()); ok {
			return fmt.Errorf("transient answer %s is set by %s (%s): it is root's decision, never an answer", key, ownerOf(rule), rule.Why)
		}
		if err := setPath(values, path.Keys(), value); err != nil {
			return err
		}
	}
	return nil
}

// setPath writes one value into a decoded values document, creating the
// sections on the way. It refuses to replace a scalar with a section: that
// would be box silently changing a value nobody asked it to touch.
func setPath(values map[string]any, keys []string, value any) error {
	cur := values
	for i, key := range keys {
		if i == len(keys)-1 {
			cur[key] = value
			return nil
		}
		next, ok := cur[key]
		if !ok || next == nil {
			m := map[string]any{}
			cur[key] = m
			cur = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("transient answer %s: %s is not a section", strings.Join(keys, "."), strings.Join(keys[:i+1], "."))
		}
		cur = m
	}
	return nil
}

func ownerOf(rule seedbox.Rule) string {
	if rule.BoxSets {
		return "box"
	}
	return "the grant"
}
