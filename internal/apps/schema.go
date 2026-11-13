package apps

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
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

// The field names qui's instances and autobrr's download clients share with the
// Arrs. They are the apps' own API spellings: a wrong one is a silent no-op
// (the field is absent from the update), so each is written once here rather
// than at every call site.
const (
	fieldHost     = "host"
	fieldPort     = "port"
	fieldUsername = "username"
	fieldPassword = "password"
)

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

// ownedValue reads one owned field from a live object, whichever shape the API
// returns: an Arr or Prowlarr object carries its settings in a "fields" list,
// while qui's instances and autobrr's download clients are plain objects.
func (i Item) ownedValue(name string) (any, bool) {
	if _, ok := i["fields"].([]any); ok {
		return i.FieldValue(name)
	}
	v, ok := i[name]
	return v, ok
}

// setOwned writes one owned field onto an object, in the shape that object uses
// (see ownedValue). An Arr field the schema no longer has is simply not
// written, which is how version drift is tolerated; a plain object always takes
// the key, because its API decodes the struct it is sent.
func (i Item) setOwned(name string, value any) {
	if _, ok := i["fields"].([]any); ok {
		i.SetField(name, value)
		return
	}
	i[name] = value
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

// FieldChange records one owned field that had drifted from what the wiring
// Job last set and was restored. Old and New are human-readable; a secret
// field is never represented here at all (see ownedField.secret) so a
// password or API key can never leak into the receipt.
type FieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
}

// EnsureResult is what an Ensure call against an owned Arr/Prowlarr object did.
// ID is the object's id after the call, which the caller persists as
// ownership. Drift lists owned fields that were found different on the live
// object and were restored. Recreated/PreviousID describe a deleted owned
// object that had to be created again.
//
// StringID and PreviousStringID are the same two facts for an app that keys its
// objects by a name rather than a number (PBH's downloaders are keys in its own
// config file), so one receipt line can name either.
type EnsureResult struct {
	Change           Change
	ID               int
	StringID         string
	Drift            []FieldChange
	Recreated        bool
	PreviousID       int
	PreviousStringID string
}

// ownedField is one field the wiring Job may write on an object it owns.
// Secret fields (password, apiKey) are always overlaid but never compared:
// the API never returns their real value, only a mask.
type ownedField struct {
	name   string
	value  any
	secret bool
}

// locateOwned finds the object the wiring Job owns, by rule, not by habit:
//
//   - ownedID != 0: only an id match counts. A rename in the app's own UI is
//     the user's business and must never cause a second object to be created.
//     If no object carries that id, it was deleted: report recreated=true so
//     the caller creates a fresh one and the receipt can say so.
//   - ownedID == 0: nothing is recorded yet (first run, or an owned.json the
//     Job could not read). Adopt an existing object with the expected name,
//     exactly as a first run always has.
func locateOwned(list []Item, ownedID int, name string) (cur Item, found, recreated bool, previousID int) {
	if ownedID != 0 {
		for _, item := range list {
			if item.Int("id") == ownedID {
				return item, true, false, 0
			}
		}
		return nil, false, true, ownedID
	}
	if item, ok := FindByName(list, name); ok {
		return item, true, false, 0
	}
	return nil, false, false, 0
}

// overlayOwned clones the live object and writes only the owned fields onto
// it, so every other field — name, enable, priority, tags, syncLevel, and
// anything a future schema adds — is left exactly as the user last set it.
// It reports drift for every non-secret owned field whose live value did not
// already match.
//
// The fields go on in the shape the live object uses (ownedValue): an Arr's
// "fields" list, or a plain key for qui's instances and autobrr's clients.
func overlayOwned(live Item, fields []ownedField) (Item, []FieldChange) {
	desired := live.Clone()
	var drift []FieldChange
	for _, f := range fields {
		if !f.secret {
			if cv, ok := live.ownedValue(f.name); ok && !SameValue(cv, f.value) {
				drift = append(drift, FieldChange{Field: f.name, Old: formatValue(cv), New: formatValue(f.value)})
			}
		}
		desired.setOwned(f.name, f.value)
	}
	return desired, drift
}

// formatValue renders a decoded JSON scalar the way a human reading the
// receipt expects: whole numbers without a trailing ".0".
func formatValue(v any) string {
	if f, ok := number(v); ok {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
