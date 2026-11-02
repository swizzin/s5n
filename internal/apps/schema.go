package apps

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Change describes what an Ensure call had to do.
type Change int

const (
	// Unchanged means the live state already matched the desired state.
	Unchanged Change = iota
	// Created means the object was missing and was added.
	Created
	// Updated means the object existed and one or more fields were rewritten.
	Updated
	// Skipped means the step did not apply to this release.
	Skipped
)

func (c Change) String() string {
	switch c {
	case Created:
		return "created"
	case Updated:
		return "updated"
	case Skipped:
		return "skipped"
	default:
		return "unchanged"
	}
}

// Item is one JSON object from an Arr/Prowlarr API, kept generic so the schema
// can drive the payload.
type Item map[string]any

// Clone deep-copies an Item. Used to turn a schema template into a request body
// without mutating the cached template.
func (i Item) Clone() Item {
	b, _ := json.Marshal(i)
	var out Item
	_ = json.Unmarshal(b, &out)
	return out
}

// Str returns a string member, or "" when absent or not a string.
func (i Item) Str(key string) string {
	s, _ := i[key].(string)
	return s
}

// Int returns an integer member, or 0.
func (i Item) Int(key string) int {
	switch v := i[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// Fields returns the nested "fields" array as Items.
func (i Item) Fields() []Item {
	raw, ok := i["fields"].([]any)
	if !ok {
		return nil
	}
	out := make([]Item, 0, len(raw))
	for _, f := range raw {
		if m, ok := f.(map[string]any); ok {
			out = append(out, Item(m))
		}
	}
	return out
}

// FieldValue returns the value of a named schema field.
func (i Item) FieldValue(name string) (any, bool) {
	for _, f := range i.Fields() {
		if f.Str("name") == name {
			v, ok := f["value"]
			return v, ok
		}
	}
	return nil, false
}

// SetField sets a named field's value. It returns false when the schema has no
// such field, which is how version drift is tolerated: a field that no longer
// exists is simply not written.
func (i Item) SetField(name string, value any) bool {
	raw, ok := i["fields"].([]any)
	if !ok {
		return false
	}
	for _, f := range raw {
		m, ok := f.(map[string]any)
		if !ok || m["name"] != name {
			continue
		}
		m["value"] = value
		return true
	}
	return false
}

// TagList returns a string slice member.
func (i Item) TagList(key string) []string {
	raw, ok := i[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// FindImplementation returns the schema entry for an implementation name
// (case-insensitive), e.g. "QBittorrent" or "Sonarr".
func FindImplementation(list []Item, impl string) (Item, bool) {
	for _, e := range list {
		if strings.EqualFold(e.Str("implementation"), impl) {
			return e, true
		}
	}
	return nil, false
}

// FindByName returns the item whose name matches exactly (case-insensitive).
func FindByName(list []Item, name string) (Item, bool) {
	for _, e := range list {
		if strings.EqualFold(e.Str("name"), name) {
			return e, true
		}
	}
	return nil, false
}

// SameValue compares two decoded JSON values, tolerating int vs float64.
func SameValue(a, b any) bool {
	if af, ok := number(a); ok {
		bf, ok := number(b)
		return ok && af == bf
	}
	return reflect.DeepEqual(a, b)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// masked reports whether a value is an API secret placeholder.
func masked(v any) bool {
	s, ok := v.(string)
	return ok && strings.Trim(s, "*") == ""
}

// MatchesFields reports whether every field in want is already present with the
// same value in have. Fields whose live value is masked (passwords, API keys) are
// not compared: the API never returns them, and the connection test is what
// proves they still work.
func MatchesFields(want, have Item, names []string) bool {
	for _, name := range names {
		wv, ok := want.FieldValue(name)
		if !ok {
			continue
		}
		hv, ok := have.FieldValue(name)
		if !ok {
			return false
		}
		if masked(hv) {
			continue
		}
		if !SameValue(wv, hv) {
			return false
		}
	}
	return true
}
