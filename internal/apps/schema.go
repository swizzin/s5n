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

// The field names qui's instances and autobrr's clients share with the Arrs, in the
// apps' own spellings: a wrong one is a silent no-op, so each is written once.
const (
	fieldName     = "name"
	fieldHost     = "host"
	fieldPort     = "port"
	fieldUsername = "username"
	fieldPassword = "password"
	fieldAPIKey   = "apiKey"
	fieldSection  = "section"
	fieldKeyword  = "keyword"
	// qui's Arr rows name which Arr a row is, and whether it is on
	// (internal/models/arr_instance.go at v1.36.1).
	fieldType    = "type"
	fieldEnabled = "enabled"
)

// Clone deep-copies an Item, so a schema template becomes a body unmutated.
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
// returns: an Arr/Prowlarr object carries settings in "fields", while qui's
// instances and autobrr's clients are plain objects.
func (i Item) ownedValue(name string) (any, bool) {
	if _, ok := i["fields"].([]any); ok {
		return i.FieldValue(name)
	}
	v, ok := i[name]
	return v, ok
}

// setOwned writes one owned field in the object's own shape (see ownedValue). An Arr
// field the schema no longer has is not written, which is how version drift is
// tolerated.
func (i Item) setOwned(name string, value any) {
	if _, ok := i["fields"].([]any); ok {
		i.SetField(name, value)
		return
	}
	i[name] = value
}

// SetField sets a named field's value, reporting false when the schema has no such
// field -- how version drift is tolerated.
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

// FindImplementation returns the schema entry for an implementation name,
// case-insensitively.
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

// MatchesFields reports whether every field in want is present with the same value
// in have. Masked values are not compared: the API never returns them, and the
// connection test proves they work.
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

// FieldChange records one owned field that drifted and was restored. A secret
// field is never represented here, so a credential cannot leak into the receipt.
type FieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
}

// EnsureResult is what an Ensure call against an owned object did. ID is the id the
// caller persists; Drift lists restored owned fields; Recreated/PreviousID describe
// a deleted object created again.
//
// StringID and PreviousStringID are the same facts for an app keying objects by name
// (PBH's downloaders are keys in its config).
type EnsureResult struct {
	Change           Change
	ID               int
	StringID         string
	Drift            []FieldChange
	Recreated        bool
	PreviousID       int
	PreviousStringID string
	// Note is the one thing about how the object was found that the change does not
	// say: an object adopted in place of the recorded one, or one of several at the
	// same address.
	Note string
}

// ownedField is one field the Job may write on an object it owns. Secret fields are
// always overlaid but never compared: the API returns only a mask.
type ownedField struct {
	name   string
	value  any
	secret bool
}

// locateOwned finds the object the Job owns: a recorded id is authoritative (a rename
// is the user's, and a missing id means recreate), and with nothing recorded an object
// with the expected name is adopted, as a first run always has.
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

// locateArrClient finds one download client the Job owns, of the implementation the
// spec names.
//
// A recorded id is authoritative. With nothing recorded, an enabled client of that
// implementation pointing at the release's own host is adopted before one is created:
// a migrated box's client has the operator's own name ("qBT"), so the host is what
// makes it box's, and the name is the fallback. Only when neither exists was it
// deleted.
//
// Two enabled clients of that implementation at that host are a refusal rather than a
// pick: nothing says which the operator means.
//
//nolint:dupl // qui's instance and an Arr's client are different types with the same rules; sharing the body would mean sharing their specs too.
func locateArrClient(list []Item, spec DownloadClientSpec, ownedID int) (cur Item, found, recreated bool, previousID int, err error) {
	if ownedID != 0 {
		for _, item := range list {
			if item.Int("id") == ownedID {
				return item, true, false, 0, nil
			}
		}
		return nil, false, true, ownedID, nil
	}
	switch same := arrClientHostMatches(list, spec.implementation(), spec.Host); len(same) {
	case 0:
	case 1:
		return same[0], true, false, 0, nil
	default:
		return nil, false, false, 0, fmt.Errorf("this Arr has %d enabled %s download clients at %s (ids %d and %d); remove all but one, this box can own only one",
			len(same), spec.implementation(), spec.Host, same[0].Int("id"), same[1].Int("id"))
	}
	if item, ok := FindByName(list, spec.Name); ok {
		return item, true, false, 0, nil
	}
	return nil, false, false, 0, nil
}

// arrClientHostMatches returns the enabled clients of one implementation at the spec's
// host: another implementation is another downloader, and a disabled one is never
// dialled.
func arrClientHostMatches(list []Item, implementation, host string) []Item {
	want := normalizeArrHost(host)
	if want == "" {
		return nil
	}
	var out []Item
	for _, item := range list {
		if !strings.EqualFold(item.Str("implementation"), implementation) {
			continue
		}
		if on, ok := item["enable"].(bool); ok && !on {
			continue
		}
		raw, _ := item.FieldValue(fieldHost)
		got, _ := raw.(string)
		if normalizeArrHost(got) == want {
			out = append(out, item)
		}
	}
	return out
}

// normalizeArrHost is a download client's host as two are compared: the Arr stores
// what the operator typed, and "qbittorrent", "QBittorrent" and
// "http://qbittorrent/" are one address.
func normalizeArrHost(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")
	return strings.ToLower(strings.TrimRight(h, "/"))
}

// overlayOwned clones the live object and writes only the owned fields onto it, so
// every other field -- name, enable, priority, tags and anything a future schema
// adds -- is left as the user set it. Drift is reported for non-secret fields whose
// live value did not match.
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

// formatValue renders a decoded JSON scalar the way the receipt reads: whole numbers
// without a trailing ".0".
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
