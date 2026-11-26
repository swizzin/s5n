package seerr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// jsonFile is a JSON document with the position of every value, so a rewrite
// replaces one value and writes every other byte back as it was. A decoder round
// trip would reorder keys and reformat the settings file Seerr rewrites with
// two-space indentation (server/lib/settings/index.ts at v3.5.1, write()).
type jsonFile struct {
	data  []byte
	nodes []*jsonNode
}

// jsonNode is one value and where its token sits.
type jsonNode struct {
	path       []string
	kind       byte // 's' string, 'n' number, 'b' bool, 'z' null, 'o' object, 'a' array
	start, end int  // the token's span
	raw        string
	str        string   // decoded value for a string
	members    []string // an object's keys, in order
	lastEnd    int      // end of the last member's value, for inserting one more
}

func parseJSON(data []byte) (*jsonFile, error) {
	f := &jsonFile{data: data}
	p := &parser{data: data}
	if err := p.value(nil); err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("trailing data at byte %d", p.pos)
	}
	f.nodes = p.nodes
	return f, nil
}

type parser struct {
	data  []byte
	pos   int
	nodes []*jsonNode
}

func (p *parser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(path []string) error {
	p.space()
	if p.pos >= len(p.data) {
		return fmt.Errorf("value expected at end of file")
	}
	switch c := p.data[p.pos]; c {
	case '{':
		return p.object(path)
	case '[':
		return p.array(path)
	case '"':
		n, err := p.str(path)
		if err != nil {
			return err
		}
		p.nodes = append(p.nodes, n)
		return nil
	case 't', 'f', 'n':
		return p.literal(path)
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			start := p.pos
			for p.pos < len(p.data) && strings.IndexByte("+-.eE0123456789", p.data[p.pos]) >= 0 {
				p.pos++
			}
			p.nodes = append(p.nodes, &jsonNode{path: path, kind: 'n', start: start, end: p.pos,
				raw: string(p.data[start:p.pos])})
			return nil
		}
		return fmt.Errorf("unexpected %q at byte %d", c, p.pos)
	}
}

func (p *parser) object(path []string) error {
	n := &jsonNode{path: path, kind: 'o', start: p.pos}
	p.nodes = append(p.nodes, n)
	p.pos++ // '{'
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		n.lastEnd = p.pos
		n.end = p.pos + 1
		p.pos++
		return nil
	}
	for {
		p.space()
		key, err := p.str(nil)
		if err != nil {
			return err
		}
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return fmt.Errorf("':' expected at byte %d", p.pos)
		}
		p.pos++
		memberPath := append(append([]string(nil), path...), key.str)
		if err := p.value(memberPath); err != nil {
			return err
		}
		n.members = append(n.members, key.str)
		n.lastEnd = p.pos
		p.space()
		if p.pos >= len(p.data) {
			return fmt.Errorf("object has no closing brace")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			n.end = p.pos + 1
			p.pos++
			return nil
		default:
			return fmt.Errorf("',' or '}' expected at byte %d", p.pos)
		}
	}
}

func (p *parser) array(path []string) error {
	n := &jsonNode{path: path, kind: 'a', start: p.pos}
	p.nodes = append(p.nodes, n)
	p.pos++ // '['
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		n.lastEnd = p.pos
		n.end = p.pos + 1
		p.pos++
		return nil
	}
	for i := 0; ; i++ {
		item := append(append([]string(nil), path...), strconv.Itoa(i))
		if err := p.value(item); err != nil {
			return err
		}
		n.lastEnd = p.pos
		p.space()
		if p.pos >= len(p.data) {
			return fmt.Errorf("array has no closing bracket")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			n.end = p.pos + 1
			p.pos++
			return nil
		default:
			return fmt.Errorf("',' or ']' expected at byte %d", p.pos)
		}
	}
}

// str reads a string token. A key is read the same way and thrown away.
func (p *parser) str(path []string) (*jsonNode, error) {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return nil, fmt.Errorf("string expected at byte %d", p.pos)
	}
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case '\\':
			p.pos += 2
			continue
		case '"':
			p.pos++
			var s string
			if err := json.Unmarshal(p.data[start:p.pos], &s); err != nil {
				return nil, fmt.Errorf("string at byte %d: %w", start, err)
			}
			return &jsonNode{path: path, kind: 's', start: start, end: p.pos,
				raw: string(p.data[start:p.pos]), str: s}, nil
		}
		p.pos++
	}
	return nil, fmt.Errorf("string at byte %d has no closing quote", start)
}

func (p *parser) literal(path []string) error {
	for _, lit := range []struct {
		text string
		kind byte
	}{{"true", 'b'}, {"false", 'b'}, {"null", 'z'}} {
		if strings.HasPrefix(string(p.data[p.pos:]), lit.text) {
			start := p.pos
			p.pos += len(lit.text)
			p.nodes = append(p.nodes, &jsonNode{path: path, kind: lit.kind, start: start, end: p.pos,
				raw: lit.text})
			return nil
		}
	}
	return fmt.Errorf("unexpected %q at byte %d", p.data[p.pos], p.pos)
}

// find is the value at a JSON pointer ("/sonarr/0/hostname").
func (f *jsonFile) find(pointer string) (*jsonNode, bool) {
	want := pointerSegments(pointer)
	for _, n := range f.nodes {
		if len(n.path) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if n.path[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return n, true
		}
	}
	return nil, false
}

func pointerSegments(pointer string) []string {
	if pointer == "" || pointer == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		parts[i] = strings.ReplaceAll(p, "~0", "~")
	}
	return parts
}

// edit replaces one token, or adds a member to an object.
type edit struct {
	start, end int
	text       string
	where      string // JSON pointer, for the report
	what       string // "path", "host", "port", "tls", "url base"
}

// setString is an edit that replaces a value with a JSON string.
func (f *jsonFile) setString(n *jsonNode, s, what string) edit {
	enc, _ := json.Marshal(s)
	return edit{start: n.start, end: n.end, text: string(enc),
		where: "/" + strings.Join(n.path, "/"), what: what}
}

// setRaw is an edit that replaces a value with a number or a boolean.
func (f *jsonFile) setRaw(n *jsonNode, raw, what string) edit {
	return edit{start: n.start, end: n.end, text: raw,
		where: "/" + strings.Join(n.path, "/"), what: what}
}

// addMember inserts a member into an object lacking it, in its own indentation.
func (f *jsonFile) addMember(obj *jsonNode, key, value, what string) (edit, bool) {
	if obj == nil || obj.kind != 'o' {
		return edit{}, false
	}
	enc, _ := json.Marshal(key)
	where := "/" + strings.Join(append(append([]string(nil), obj.path...), key), "/")
	if len(obj.members) == 0 {
		return edit{start: obj.lastEnd, end: obj.lastEnd, text: string(enc) + ": " + value,
			where: where, what: what}, true
	}
	// After the last member, before the closing brace, which keeps its line.
	gap := string(f.data[obj.lastEnd:obj.end])
	if !strings.Contains(gap, "\n") {
		return edit{start: obj.lastEnd, end: obj.lastEnd,
			text: ", " + string(enc) + ": " + value, where: where, what: what}, true
	}
	indent := ""
	if start := bytesLastIndexByte(f.data[:obj.lastEnd], '\n'); start >= 0 {
		for _, c := range f.data[start+1 : obj.lastEnd] {
			if c != ' ' && c != '\t' {
				break
			}
			indent += string(c)
		}
	}
	return edit{start: obj.lastEnd, end: obj.lastEnd,
		text: ",\n" + indent + string(enc) + ": " + value, where: where, what: what}, true
}

func bytesLastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// apply writes the edits in order, copying everything between them byte for
// byte.
func (f *jsonFile) apply(edits []edit) []byte {
	sorted := append([]edit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	var out bytes.Buffer
	last := 0
	for _, e := range sorted {
		if e.start < last {
			// Two edits cannot overlap: a value's span, or an insertion after one.
			continue
		}
		out.Write(f.data[last:e.start])
		out.WriteString(e.text)
		last = e.end
	}
	out.Write(f.data[last:])
	return out.Bytes()
}
