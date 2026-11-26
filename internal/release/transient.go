package release

import (
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/seedbox"
)

// A transient answer outlives no file: init asks for the Plex claim token after
// the confirm, hands it to one apply, and never writes it (docs/box.md, "The
// catalog and box").

// withoutTransient drops changes naming a transient answer: the stored previous
// value would otherwise print the secret and break a no-op re-run.
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

// mergeTransient sets AfterConfirm's answers by dotted path; an empty answer is
// skipped. It refuses a grant key even though the catalog cannot ask for one:
// an answer after the confirm is still an answer, and the grant is root's.
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

// setPath creates sections on the way but refuses to replace a scalar.
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
