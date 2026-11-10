package seedbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Merge returns the values document box hands Helm: the user's values with the
// grant overlaid on top. Two rules decide the result.
//
// Grant wins. The overlay is applied last and replaces whatever was there,
// including a whole list, because lists are never merged element by element in
// values files: `helm -f` replaces a list, and so does this. The only grant list
// is apps.*.instances, and it is grant-owned as a unit, so no list ever has to
// be merged entry by entry.
//
// A grant key in values is an error, not a value to override, because a user
// who sets gateway.host and sees no change will assume it worked. The error
// names the key and the line, in both directions: a grant key in values, and a
// non-grant key in the grant.
//
// Merge reads the two files and nothing else. The keys box writes from its own
// decision are not added here: they belong to Effective, which has the release
// being installed and can tell which of its keys the chart actually declares.
func (s *Seedbox) Merge() (map[string]any, error) {
	if err := s.checkGrant(); err != nil {
		return nil, err
	}
	if err := s.checkValues(); err != nil {
		return nil, err
	}
	out := cloneMap(s.Values.data)
	overlay(out, s.Grant.data, nil)
	if _, ok := out["apps"]; !ok {
		out["apps"] = map[string]any{}
	}
	return out, nil
}

// overlay applies src on top of dst. Scalars and lists are replaced; mappings
// are merged key by key so that a grant's global.puid does not discard the
// user's global.tz. A BoxOnly key is skipped at any depth: it is box's own
// record -- the namespace, the host paths -- and never a chart value.
func overlay(dst, src map[string]any, path []string) {
	for k, v := range src {
		keys := make([]string, len(path)+1)
		copy(keys, path)
		keys[len(path)] = k
		if rule, ok := GrantRule(keys); ok && rule.BoxOnly {
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				overlay(dm, sm, keys)
				continue
			}
		}
		dst[k] = cloneValue(v)
	}
}

// checkGrant refuses a grant that says something the grant cannot say. Root
// edits this file by hand and by command, so the mistakes to catch are a typo'd
// key (which would otherwise be tuned in two places), a missing domain (the box
// cannot be reached without one) and an app entry that does not say whether the
// app exists.
func (s *Seedbox) checkGrant() error {
	g := s.Grant
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if _, ok := g.data["namespace"]; !ok {
		add("%s: namespace is missing; it names the release namespace", g.Path)
	}
	if _, ok, _ := g.lookup("gateway.host"); !ok {
		add("%s: gateway.host is missing; a seedbox needs a domain", g.Path)
	}

	walkKeys(g.root, nil, func(keys []string, key, val *yaml.Node) {
		if val.Kind == yaml.MappingNode {
			return // a branch is not a decision; its leaves are
		}
		rule, ok := GrantRule(keys)
		switch {
		case ok && rule.BoxOnly:
			return
		case ok && rule.BoxSets:
			how := "box derives it from what the grant already says"
			if rule.Fixed {
				how = "box sets it itself"
			}
			add("%s:%d: %s is box's; %s, so it does not belong in either file",
				g.Path, key.Line, strings.Join(keys, "."), how)
		case !ok:
			add("%s:%d: %s is not a grant key; a grant holds only what decides isolation and which apps exist, so this belongs in the user's values file",
				g.Path, key.Line, strings.Join(keys, "."))
		}
	})

	if apps, ok := g.data["apps"].(map[string]any); ok {
		for _, name := range sortedKeys(apps) {
			entry, ok := apps[name].(map[string]any)
			if !ok {
				add("%s: apps.%s must be a mapping with an enabled key", g.Path, name)
				continue
			}
			if _, ok := entry["enabled"].(bool); !ok {
				line := 0
				if n, ok, _ := g.lookup("apps." + name); ok {
					line = n.Line
				}
				add("%s:%d: apps.%s.enabled is missing; the grant is what says which apps the box has", g.Path, line, name)
			}
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// checkValues refuses every grant key a user's file sets. It reports all of
// them at once: a user who copied a grant-shaped file wants the whole list, not
// one key per run. A branch is not a decision, so a rule on a branch reports
// the leaves beneath it -- that is what names the key the user actually wrote.
func (s *Seedbox) checkValues() error {
	v := s.Values
	var problems []string
	walkKeys(v.root, nil, func(keys []string, key, val *yaml.Node) {
		if val.Kind == yaml.MappingNode {
			return
		}
		rule, ok := GrantRule(keys)
		if !ok {
			return
		}
		by := "the grant"
		if rule.BoxSets {
			by = "box"
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s is set by %s (%s); remove it from this file",
			v.Path, key.Line, strings.Join(keys, "."), by, rule.Why))
	})
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// walkKeys visits every mapping key in the document, outermost first, with the
// key's path. A list is not descended: lists are values, they are replaced
// whole, and an index in a path is only meaningful to an editor.
func walkKeys(n *yaml.Node, keys []string, fn func(keys []string, key, val *yaml.Node)) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		child := make([]string, len(keys)+1)
		copy(child, keys)
		child[len(keys)] = k.Value
		fn(child, k, v)
		if v.Kind == yaml.MappingNode {
			walkKeys(v, child, fn)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
