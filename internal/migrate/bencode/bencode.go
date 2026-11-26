// Package bencode is the codec for qBittorrent's bencode files: one resume file
// per torrent and its metadata. box rewrites the paths a resume file holds and
// must change nothing else -- libtorrent reads it back into live state, and a
// lost key is a torrent that rechecks from the start -- so a decoded value keeps
// the file's bytes and order and re-encodes byte for byte. Nothing here assumes
// text: a path is bytes, and invalid UTF-8 must survive the round trip.
package bencode

import (
	"bytes"
	"fmt"
	"strconv"
)

// Kind is what a value is.
type Kind int

const (
	// String is a byte string: a path, a name, a bitfield.
	String Kind = iota
	// Int is an integer.
	Int
	// List is an ordered list.
	List
	// Dict is a dictionary, held as fields so its order is not lost.
	Dict
)

// Value is one bencoded value. Only the field for its Kind is meaningful.
type Value struct {
	Kind  Kind
	Bytes []byte  // String: the bytes the length prefix announced
	Int   int64   // Int
	List  []Value // List
	Dict  []Field // Dict, in the order the file had them
}

// Field is one dictionary entry; the key is bytes for the same reason.
type Field struct {
	Key   []byte
	Value Value
}

// Lookup returns the value filed under key, or nil. Only a dictionary has keys.
func (v *Value) Lookup(key string) *Value {
	if v == nil || v.Kind != Dict {
		return nil
	}
	for i := range v.Dict {
		if string(v.Dict[i].Key) == key {
			return &v.Dict[i].Value
		}
	}
	return nil
}

// maxDepth bounds nesting: a resume file is a handful of levels deep, so the cap
// only fires on a file that is not one.
const maxDepth = 64

// Error is a byte sequence this codec refuses, at the byte it refused.
type Error struct {
	Offset int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("bencode: at byte %d: %s", e.Offset, e.Msg)
}

// Decode reads exactly one value; trailing bytes are an error, not an ignored
// tail. It refuses non-canonical encodings (leading zeros, keys out of order or
// repeated) because re-encoding must give the same bytes back: libtorrent writes
// none and reports one it reads as a soft error (bdecode_node::has_soft_error).
func Decode(data []byte) (Value, error) {
	d := decoder{data: data}
	v, err := d.value(0)
	if err != nil {
		return Value{}, err
	}
	if d.pos != len(data) {
		return Value{}, &Error{Offset: d.pos, Msg: "trailing data after the value"}
	}
	return v, nil
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) fail(offset int, format string, args ...any) error {
	return &Error{Offset: offset, Msg: fmt.Sprintf(format, args...)}
}

func (d *decoder) value(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, d.fail(d.pos, "nested deeper than %d levels", maxDepth)
	}
	if d.pos >= len(d.data) {
		return Value{}, d.fail(d.pos, "unexpected end of file")
	}
	switch c := d.data[d.pos]; {
	case c == 'i':
		return d.integer()
	case c == 'l':
		return d.list(depth)
	case c == 'd':
		return d.dict(depth)
	case c >= '0' && c <= '9':
		return d.string()
	default:
		return Value{}, d.fail(d.pos, "unexpected byte %q", c)
	}
}

func (d *decoder) integer() (Value, error) {
	start := d.pos
	d.pos++ // 'i'
	end := bytes.IndexByte(d.data[d.pos:], 'e')
	if end < 0 {
		return Value{}, d.fail(start, "unterminated integer")
	}
	body := d.data[d.pos : d.pos+end]
	d.pos += end + 1
	if len(body) == 0 {
		return Value{}, d.fail(start, "empty integer")
	}
	digits := body
	negative := body[0] == '-'
	if negative {
		digits = body[1:]
	}
	if len(digits) == 0 || !allDigits(digits) {
		return Value{}, d.fail(start, "integer %q is not a number", body)
	}
	if len(digits) > 1 && digits[0] == '0' {
		return Value{}, d.fail(start, "integer %q has a leading zero", body)
	}
	if negative && digits[0] == '0' {
		return Value{}, d.fail(start, "integer %q is negative zero", body)
	}
	n, err := strconv.ParseInt(string(body), 10, 64)
	if err != nil {
		return Value{}, d.fail(start, "integer %q does not fit 64 bits", body)
	}
	return Value{Kind: Int, Int: n}, nil
}

func (d *decoder) string() (Value, error) {
	start := d.pos
	colon := bytes.IndexByte(d.data[d.pos:], ':')
	if colon < 0 {
		return Value{}, d.fail(start, "unterminated string length")
	}
	length := d.data[d.pos : d.pos+colon]
	if !allDigits(length) {
		return Value{}, d.fail(start, "string length %q is not a number", length)
	}
	if len(length) > 1 && length[0] == '0' {
		return Value{}, d.fail(start, "string length %q has a leading zero", length)
	}
	n, err := strconv.Atoi(string(length))
	if err != nil || n > len(d.data) {
		return Value{}, d.fail(start, "string length %q is longer than the file", length)
	}
	d.pos += colon + 1
	if d.pos+n > len(d.data) {
		return Value{}, d.fail(start, "string of length %d runs past the end of the file", n)
	}
	s := d.data[d.pos : d.pos+n]
	d.pos += n
	return Value{Kind: String, Bytes: s}, nil
}

func (d *decoder) list(depth int) (Value, error) {
	start := d.pos
	d.pos++ // 'l'
	// Allocated even when empty so a round trip compares equal: nil and empty
	// encode the same.
	out := make([]Value, 0, 4)
	for {
		if d.pos >= len(d.data) {
			return Value{}, d.fail(start, "unterminated list")
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return Value{Kind: List, List: out}, nil
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		out = append(out, v)
	}
}

func (d *decoder) dict(depth int) (Value, error) {
	start := d.pos
	d.pos++ // 'd'
	out := make([]Field, 0, 8)
	for {
		if d.pos >= len(d.data) {
			return Value{}, d.fail(start, "unterminated dictionary")
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return Value{Kind: Dict, Dict: out}, nil
		}
		keyAt := d.pos
		if c := d.data[d.pos]; c < '0' || c > '9' {
			return Value{}, d.fail(keyAt, "dictionary key is not a string")
		}
		key, err := d.string()
		if err != nil {
			return Value{}, err
		}
		if n := len(out); n > 0 {
			switch prev := out[n-1].Key; {
			case bytes.Equal(prev, key.Bytes):
				return Value{}, d.fail(keyAt, "dictionary key %q appears twice", key.Bytes)
			case bytes.Compare(prev, key.Bytes) > 0:
				return Value{}, d.fail(keyAt, "dictionary key %q is out of order", key.Bytes)
			}
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		out = append(out, Field{Key: key.Bytes, Value: v})
	}
}

func allDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Encode writes a value; dictionary keys out of order are an error, since
// libtorrent never writes one.
func Encode(v Value) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, v, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v Value, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("bencode: nested deeper than %d levels", maxDepth)
	}
	switch v.Kind {
	case String:
		b.WriteString(strconv.Itoa(len(v.Bytes)))
		b.WriteByte(':')
		b.Write(v.Bytes)
	case Int:
		b.WriteByte('i')
		b.WriteString(strconv.FormatInt(v.Int, 10))
		b.WriteByte('e')
	case List:
		b.WriteByte('l')
		for _, e := range v.List {
			if err := encode(b, e, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte('e')
	case Dict:
		b.WriteByte('d')
		for i, f := range v.Dict {
			if i > 0 && bytes.Compare(v.Dict[i-1].Key, f.Key) >= 0 {
				return fmt.Errorf("bencode: dictionary key %q is out of order", f.Key)
			}
			b.WriteString(strconv.Itoa(len(f.Key)))
			b.WriteByte(':')
			b.Write(f.Key)
			if err := encode(b, f.Value, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte('e')
	default:
		return fmt.Errorf("bencode: value has no kind")
	}
	return nil
}
