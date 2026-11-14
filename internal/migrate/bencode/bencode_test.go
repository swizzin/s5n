package bencode

import (
	"bytes"
	"errors"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bstr renders a string the way libtorrent's writer does, so the fixtures below
// can be written readably and still be the bytes a resume file holds.
func bstr(s string) string { return strconv.Itoa(len(s)) + ":" + s }

// resumeShaped is a file with the shape a resume file has: a dictionary of
// integers, a binary hash, strings, a nested dictionary and a list. The paths
// and the hash are invented.
func resumeShaped() []byte {
	hash := strings.Repeat("a1", 10)
	return []byte("d" +
		bstr("active_time") + "i60e" +
		bstr("completed_time") + "i1700000000e" +
		bstr("file-format") + bstr("libtorrent resume file") +
		bstr("file-version") + "i1e" +
		bstr("info") + "d" + bstr("name") + bstr("Some.Film.2024.1080p") + "e" +
		bstr("info-hash") + bstr(hash) +
		bstr("mapped_files") + "l" + bstr("a/relative/name.mkv") + bstr("/downloads/Series/absolute.mkv") + "e" +
		bstr("pieces") + bstr("\xff\x00\xfe binary, not text") +
		bstr("qBt-category") + bstr("Movies") +
		bstr("qBt-savePath") + bstr("/downloads/Series") +
		bstr("save_path") + bstr("/downloads/Movies") +
		"e")
}

func TestRoundTripFixtures(t *testing.T) {
	fixtures := map[string][]byte{
		"a resume-shaped file":     resumeShaped(),
		"an empty dictionary":      []byte("de"),
		"an empty list":            []byte("le"),
		"an empty string":          []byte("0:"),
		"a lone integer":           []byte("i-42e"),
		"a nested list":            []byte("ll1:ai2eee"),
		"bytes that are not UTF-8": []byte("4:\xff\x00\xfex"),
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			v, err := Decode(fixture)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			out, err := Encode(v)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(out, fixture) {
				t.Fatalf("round trip = %q, want %q", out, fixture)
			}
		})
	}
}

func TestDecodeKeepsWhatTheFileSaid(t *testing.T) {
	v, err := Decode(resumeShaped())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if v.Kind != Dict {
		t.Fatalf("kind = %v, want Dict", v.Kind)
	}
	save := v.Lookup("save_path")
	if save == nil || string(save.Bytes) != "/downloads/Movies" {
		t.Fatalf("save_path = %+v, want /downloads/Movies", save)
	}
	if got := string(v.Lookup("pieces").Bytes); !strings.HasPrefix(got, "\xff\x00\xfe") {
		t.Fatalf("pieces = %q, want the bytes exactly as the file held them", got)
	}
	mapped := v.Lookup("mapped_files")
	if mapped == nil || mapped.Kind != List || len(mapped.List) != 2 {
		t.Fatalf("mapped_files = %+v, want the two entries", mapped)
	}
	// The dictionary's own order is the file's, not sorted into a map.
	if got := string(v.Dict[0].Key); got != "active_time" {
		t.Fatalf("first key = %q, want the file's first key", got)
	}
}

func TestLookup(t *testing.T) {
	v, err := Decode([]byte("d1:ai1e1:bl1:cee"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := v.Lookup("a"); got == nil || got.Int != 1 {
		t.Errorf("Lookup(a) = %+v, want the integer", got)
	}
	if got := v.Lookup("missing"); got != nil {
		t.Errorf("Lookup(missing) = %+v, want nil", got)
	}
	list := v.Lookup("b")
	if got := list.Lookup("c"); got != nil {
		t.Errorf("Lookup on a list = %+v, want nil", got)
	}
	var nilValue *Value
	if got := nilValue.Lookup("a"); got != nil {
		t.Errorf("Lookup on nil = %+v, want nil", got)
	}
}

func TestDecodeRefusals(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		offset int
		want   string
	}{
		{"an empty file", "", 0, "unexpected end of file"},
		{"a byte that starts nothing", "x", 0, "unexpected byte"},
		{"an unterminated integer", "i12", 0, "unterminated integer"},
		{"an empty integer", "ie", 0, "empty integer"},
		{"an integer with a leading zero", "i03e", 0, "leading zero"},
		{"negative zero", "i-0e", 0, "negative zero"},
		{"a signed integer", "i+5e", 0, "not a number"},
		{"a fractional integer", "i1.5e", 0, "not a number"},
		{"an integer wider than 64 bits", "i99999999999999999999e", 0, "does not fit 64 bits"},
		{"a string that runs past the end", "5:abc", 0, "runs past the end of the file"},
		{"a length with a leading zero", "03:abc", 0, "leading zero"},
		{"a length that is not a number", "1x:abc", 0, "is not a number"},
		{"a length wider than the file", "99999999999999999999:abc", 0, "longer than the file"},
		{"a string with no colon", "12abc", 0, "unterminated string length"},
		{"trailing data", "3:abcextra", 5, "trailing data"},
		{"an unterminated list", "l", 0, "unterminated list"},
		{"an unterminated dictionary", "d", 0, "unterminated dictionary"},
		{"a dictionary key that is not a string", "di1ei2ee", 1, "dictionary key is not a string"},
		{"a dictionary key with no value", "d1:ae", 4, "unexpected byte"},
		{"dictionary keys out of order", "d1:bi1e1:ai2ee", 7, "out of order"},
		{"the same dictionary key twice", "d1:ai1e1:ai2ee", 7, "appears twice"},
		{"nesting past the cap", strings.Repeat("l", maxDepth+2) + strings.Repeat("e", maxDepth+2), -1, "nested deeper"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.in))
			if err == nil {
				t.Fatalf("Decode(%q) accepted it", tt.in)
			}
			var berr *Error
			if !errors.As(err, &berr) {
				t.Fatalf("error is %T, want *bencode.Error: %v", err, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to say %q", err, tt.want)
			}
			if tt.offset >= 0 && berr.Offset != tt.offset {
				t.Fatalf("error = %q, offset %d, want %d", err, berr.Offset, tt.offset)
			}
		})
	}
}

func TestEncodeRefusesWhatLibtorrentCouldNotRead(t *testing.T) {
	unsorted := Value{Kind: Dict, Dict: []Field{
		{Key: []byte("b"), Value: Value{Kind: Int, Int: 1}},
		{Key: []byte("a"), Value: Value{Kind: Int, Int: 2}},
	}}
	if _, err := Encode(unsorted); err == nil {
		t.Error("Encode accepted dictionary keys out of order")
	}
	repeated := Value{Kind: Dict, Dict: []Field{
		{Key: []byte("a"), Value: Value{Kind: Int, Int: 1}},
		{Key: []byte("a"), Value: Value{Kind: Int, Int: 2}},
	}}
	if _, err := Encode(repeated); err == nil {
		t.Error("Encode accepted one key twice")
	}
	if _, err := Encode(Value{Kind: Kind(9)}); err == nil {
		t.Error("Encode accepted a value with no kind")
	}
	deep := Value{Kind: List}
	for i := 0; i < maxDepth+2; i++ {
		deep = Value{Kind: List, List: []Value{deep}}
	}
	if _, err := Encode(deep); err == nil {
		t.Error("Encode accepted more nesting than it reads")
	}
}

// TestRoundTripRandomValues proves the codec on shapes no fixture wrote: any
// value it can hold, including arbitrary bytes and deep nesting, comes back
// from its own output unchanged.
func TestRoundTripRandomValues(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		want := randomValue(r, 0)
		encoded, err := Encode(want)
		if err != nil {
			t.Fatalf("Encode(%+v): %v", want, err)
		}
		got, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode(%q): %v", encoded, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip of %q changed the value:\n got %+v\nwant %+v", encoded, got, want)
		}
		again, err := Encode(got)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !bytes.Equal(again, encoded) {
			t.Fatalf("re-encode = %q, want %q", again, encoded)
		}
	}
}

func randomValue(r *rand.Rand, depth int) Value {
	kind := r.Intn(4)
	if depth >= 4 {
		kind = r.Intn(2) // only leaves: strings and integers
	}
	switch kind {
	case 0:
		b := make([]byte, r.Intn(8))
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		return Value{Kind: String, Bytes: b}
	case 1:
		return Value{Kind: Int, Int: r.Int63n(1<<41) - (1 << 40)}
	case 2:
		n := r.Intn(4)
		list := make([]Value, 0, n)
		for i := 0; i < n; i++ {
			list = append(list, randomValue(r, depth+1))
		}
		return Value{Kind: List, List: list}
	default:
		n := r.Intn(4)
		keys := make(map[string]bool, n)
		fields := make([]Field, 0, n)
		for len(fields) < n {
			key := make([]byte, 1+r.Intn(3))
			for i := range key {
				key[i] = byte('a' + r.Intn(26))
			}
			if keys[string(key)] {
				continue
			}
			keys[string(key)] = true
			fields = append(fields, Field{Key: key, Value: randomValue(r, depth+1)})
		}
		sort.Slice(fields, func(i, j int) bool { return bytes.Compare(fields[i].Key, fields[j].Key) < 0 })
		return Value{Kind: Dict, Dict: fields}
	}
}
