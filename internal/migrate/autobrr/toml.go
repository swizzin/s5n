package autobrr

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// tomlFile is a TOML file's top-level scalars with where each sits. It is not a
// parser: app configs put settings one per line at the top, and this reads
// exactly that so a rewrite leaves every other byte as it was.
type tomlFile struct {
	data     []byte
	values   []tomlValue // top-level keys, in the order they appear
	insertAt int         // where a key that is not there goes: before the first table
}

// tomlValue is one top-level key.
type tomlValue struct {
	key        string
	start, end int    // span of the value token
	raw        string // the token as written
	str        string // the string value for a quoted token, raw otherwise
	quote      byte   // '"' or '\'' for a quoted token, 0 for a bare one
}

func parseTOML(data []byte) (*tomlFile, error) {
	f := &tomlFile{data: data, insertAt: len(data)}
	table := false
	for start := 0; ; {
		rel := bytes.IndexByte(data[start:], '\n')
		end := len(data)
		if rel >= 0 {
			end = start + rel
		}
		lineEnd := end
		if lineEnd > start && data[lineEnd-1] == '\r' {
			lineEnd--
		}
		text := data[start:lineEnd]
		trimmed := bytes.TrimSpace(text)
		switch {
		case !table && len(trimmed) > 0 && trimmed[0] == '[':
			// Root settings end at the first table header.
			f.insertAt = start
			table = true
		case !table && len(trimmed) > 0 && trimmed[0] != '#':
			if err := f.parseLine(start, text, lineEnd); err != nil {
				return nil, err
			}
		}
		if rel < 0 {
			return f, nil
		}
		start = end + 1
	}
}

func (f *tomlFile) parseLine(start int, text []byte, lineEnd int) error {
	eq := bytes.IndexByte(text, '=')
	if eq < 0 {
		// A continuation line: not migrate's.
		return nil
	}
	key := strings.TrimSpace(string(text[:eq]))
	if key == "" || strings.ContainsAny(key, " \t\"'[].") {
		return nil
	}
	vstart := start + eq + 1
	for vstart < lineEnd && (f.data[vstart] == ' ' || f.data[vstart] == '\t') {
		vstart++
	}
	vend := lineEnd
	v := tomlValue{key: key, start: vstart}
	if vstart < lineEnd && (f.data[vstart] == '"' || f.data[vstart] == '\'') {
		q := f.data[vstart]
		i := vstart + 1
		for ; i < lineEnd; i++ {
			if q == '"' && f.data[i] == '\\' {
				i++
				continue
			}
			if f.data[i] == q {
				break
			}
		}
		if i >= lineEnd {
			return fmt.Errorf("line with %s: string value has no closing quote", key)
		}
		vend = i + 1
		v.quote = q
		v.str = unescape(string(f.data[vstart+1:i]), q)
	} else {
		if i := bytes.IndexByte(f.data[vstart:lineEnd], '#'); i >= 0 {
			vend = vstart + i
		}
		for vend > vstart && (f.data[vend-1] == ' ' || f.data[vend-1] == '\t') {
			vend--
		}
		v.str = string(f.data[vstart:vend])
	}
	v.end, v.raw = vend, string(f.data[v.start:vend])
	f.values = append(f.values, v)
	return nil
}

// get is the last line holding key, as TOML resolves a duplicate.
func (f *tomlFile) get(key string) (tomlValue, bool) {
	for i := len(f.values) - 1; i >= 0; i-- {
		if f.values[i].key == key {
			return f.values[i], true
		}
	}
	return tomlValue{}, false
}

// tomlEdit is one key's new value: a string or a bare literal.
type tomlEdit struct {
	key   string
	value string
	isStr bool
	quote byte // the quote to write for a string; '"' when unknown
}

func (v tomlValue) setString(s string) tomlEdit {
	q := v.quote
	if q != '\'' || strings.ContainsRune(s, '\'') {
		q = '"'
	}
	return tomlEdit{key: v.key, value: s, isStr: true, quote: q}
}

func (v tomlValue) setLiteral(s string) tomlEdit {
	return tomlEdit{key: v.key, value: s}
}

// insertString adds a missing key with baseUrl's own double quotes.
func insertString(key, value string) tomlEdit {
	return tomlEdit{key: key, value: value, isStr: true, quote: '"'}
}

// apply replaces the edited values, adds missing keys, and copies every other byte
// as read.
func (f *tomlFile) apply(edits []tomlEdit) []byte {
	byKey := make(map[string]tomlEdit, len(edits))
	for _, e := range edits {
		byKey[e.key] = e
	}
	type span struct {
		start, end int
		text       string
	}
	var spans []span
	for _, v := range f.values {
		e, ok := byKey[v.key]
		if !ok {
			continue
		}
		delete(byKey, v.key)
		spans = append(spans, span{v.start, v.end, e.render()})
	}
	if len(byKey) > 0 {
		// Missing keys go at the end of the root table, so the app reads settings
		// rather than a table's.
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var add strings.Builder
		for _, k := range keys {
			add.WriteString(k + " = " + byKey[k].render() + "\n")
		}
		// The offset is into the bytes read, which the splice also works on, so
		// replacements before it need no adjustment.
		at := f.insertAt
		prefix := ""
		if at >= len(f.data) && len(f.data) > 0 && f.data[len(f.data)-1] != '\n' {
			prefix = "\n"
		}
		spans = append(spans, span{at, at, prefix + add.String()})
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	var out bytes.Buffer
	last := 0
	for _, s := range spans {
		out.Write(f.data[last:s.start])
		out.WriteString(s.text)
		last = s.end
	}
	out.Write(f.data[last:])
	return out.Bytes()
}

func (e tomlEdit) render() string {
	if !e.isStr {
		return e.value
	}
	if e.quote == '\'' {
		return "'" + e.value + "'"
	}
	return `"` + escape(e.value) + `"`
}

// escape is a TOML basic string's escaping for what a path or URL holds.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unescape(s string, quote byte) string {
	if quote == '\'' {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
