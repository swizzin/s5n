package peerbanhelper

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// ymlFile is PBH's config.yml with the position of every scalar value in it, so
// a rewrite can replace one value and write every other byte back as it was.
// The file is the app's template with a bilingual comment above most settings,
// and the operator has edited it by hand; reformatting it would show in a diff
// as though migrate had changed settings it never looked at.
//
// This is not a YAML parser. It reads the block-mapping subset PBH's own writer
// produces -- nested `key: value` lines, with sequence items and flow values
// passed over -- which is all this package has to find: `server:`'s three keys
// and each `client.<id>.endpoint`.
type ymlFile struct {
	data   []byte
	values []ymlValue
	blocks []*ymlBlock
}

// ymlValue is one `key: value` line.
type ymlValue struct {
	path  []string
	start int    // the value token's first byte
	end   int    // and the byte after its last
	str   string // decoded
	quote byte   // '"' or '\'' when quoted, 0 when bare
}

// ymlBlock is one `key:` line whose value is a nested block.
type ymlBlock struct {
	path   []string
	indent string // the key line's own indentation
	// childIndent is the indentation its first child used, which is the shape
	// the app writes; a block with no child yet gets two spaces.
	childIndent string
	// insertAt is where another child goes: after the last line of the block.
	insertAt int
}

// ymlFrame is one open block while the file is read.
type ymlFrame struct {
	path   []string
	indent string
	block  *ymlBlock
}

// parseYAML records every mapping entry. A line it cannot read as one is left
// where it is: the settings this package owns are all plain mappings, and
// skipping a sequence item or a comment must never shift what the lines after it
// mean.
func parseYAML(data []byte) (*ymlFile, error) {
	f := &ymlFile{data: data}
	var stack []ymlFrame

	// inside marks every open block the current line belongs to and moves their
	// insertion point past it, so a key added later lands after the block's last
	// child. Blocks the line has left are popped first.
	inside := func(indent string, afterLine int) {
		for len(stack) > 0 && len(indent) <= len(stack[len(stack)-1].indent) {
			stack = stack[:len(stack)-1]
		}
		for i := range stack {
			stack[i].block.insertAt = afterLine
		}
	}

	for start := 0; start < len(data); {
		rel := bytes.IndexByte(data[start:], '\n')
		end := len(data)
		if rel >= 0 {
			end = start + rel
		}
		lineEnd := end
		if lineEnd > start && data[lineEnd-1] == '\r' {
			lineEnd--
		}
		text := string(data[start:lineEnd])
		indent, trimmed := splitIndent(text)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			// Nothing this package reads, and a blank line inside a block does
			// not end it.
		case strings.HasPrefix(trimmed, "- ") || trimmed == "-":
			inside(indent, end+1)
		default:
			colon := keyColon(trimmed)
			if colon < 1 {
				inside(indent, end+1)
				break
			}
			key := unquoteScalar(trimmed[:colon])
			inside(indent, end+1)
			path := append(parentPath(stack), key)
			rest := trimmed[colon+1:]
			if strings.TrimSpace(stripComment(rest)) == "" {
				b := &ymlBlock{path: path, indent: indent, insertAt: end + 1}
				f.blocks = append(f.blocks, b)
				stack = append(stack, ymlFrame{path: path, indent: indent, block: b})
				break
			}
			if len(stack) > 0 && stack[len(stack)-1].block.childIndent == "" {
				stack[len(stack)-1].block.childIndent = indent
			}
			v, err := f.parseValue(start, len(indent)+colon+1, text, path)
			if err != nil {
				return nil, err
			}
			f.values = append(f.values, v)
		}
		start = end + 1
	}
	return f, nil
}

// parentPath is the path of the block the current line is inside.
func parentPath(stack []ymlFrame) []string {
	if len(stack) == 0 {
		return nil
	}
	return append([]string(nil), stack[len(stack)-1].path...)
}

// parseValue reads the token after one `key:`. from is the byte after the
// colon.
func (f *ymlFile) parseValue(lineStart, from int, text string, path []string) (ymlValue, error) {
	for from < len(text) && (text[from] == ' ' || text[from] == '\t') {
		from++
	}
	v := ymlValue{path: path, start: lineStart + from}
	if from >= len(text) {
		v.end = v.start
		return v, nil
	}
	if q := text[from]; q == '"' || q == '\'' {
		i := from + 1
		for ; i < len(text); i++ {
			if q == '"' && text[i] == '\\' {
				i++
				continue
			}
			if text[i] == q {
				if q == '\'' && i+1 < len(text) && text[i+1] == '\'' {
					i++
					continue
				}
				break
			}
		}
		if i >= len(text) {
			return v, fmt.Errorf("line %d: %s has no closing quote", strings.Count(string(f.data[:lineStart]), "\n")+1, path[len(path)-1])
		}
		v.quote = q
		v.str = unquoteScalar(text[from : i+1])
		v.end = lineStart + i + 1
		return v, nil
	}
	end := from
	for ; end < len(text); end++ {
		if text[end] == '#' && (end == from || text[end-1] == ' ' || text[end-1] == '\t') {
			break
		}
	}
	for end > from && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	v.str = text[from:end]
	v.end = lineStart + end
	return v, nil
}

// get is a value by its path. The last one wins, which is how YAML resolves a
// duplicate key.
func (f *ymlFile) get(path ...string) (ymlValue, bool) {
	for i := len(f.values) - 1; i >= 0; i-- {
		if samePath(f.values[i].path, path) {
			return f.values[i], true
		}
	}
	return ymlValue{}, false
}

// str is a value's decoded text, "" when the key is not there.
func (f *ymlFile) str(path ...string) string {
	v, _ := f.get(path...)
	return v.str
}

// blockOf is the block one path names, for adding a key the file does not have.
func (f *ymlFile) blockOf(path ...string) (*ymlBlock, bool) {
	for i := len(f.blocks) - 1; i >= 0; i-- {
		if samePath(f.blocks[i].path, path) {
			return f.blocks[i], true
		}
	}
	return nil, false
}

// ymlEdit is one value replaced, or one key added at its block's end.
type ymlEdit struct {
	start, end int
	text       string
}

// set is the edit that makes one path hold value, keeping the quoting the app
// gave the line. A key the file does not have is added as the last child of its
// block, in the block's own indentation; ok is false when that block is absent
// too, which is not a file this package can rewrite.
func (f *ymlFile) set(value string, path ...string) (ymlEdit, bool) {
	if v, ok := f.get(path...); ok {
		return ymlEdit{start: v.start, end: v.end, text: renderScalar(value, v.quote)}, true
	}
	if len(path) < 2 {
		return ymlEdit{}, false
	}
	b, ok := f.blockOf(path[:len(path)-1]...)
	if !ok {
		return ymlEdit{}, false
	}
	indent := b.childIndent
	if indent == "" {
		indent = b.indent + "  "
	}
	line := indent + path[len(path)-1] + ": " + renderScalar(value, 0) + "\n"
	if b.insertAt > 0 && f.data[b.insertAt-1] != '\n' {
		line = "\n" + line
	}
	return ymlEdit{start: b.insertAt, end: b.insertAt, text: line}, true
}

// apply writes the edits in, so every byte between them is copied as it was
// read.
func (f *ymlFile) apply(edits []ymlEdit) []byte {
	sorted := append([]ymlEdit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	var out bytes.Buffer
	last := 0
	for _, e := range sorted {
		if e.start < last {
			// Two edits never overlap: each is a value's own span, or an
			// insertion between two.
			continue
		}
		out.Write(f.data[last:e.start])
		out.WriteString(e.text)
		last = e.end
	}
	out.Write(f.data[last:])
	return out.Bytes()
}

// splitIndent separates a line's indentation from its content.
func splitIndent(line string) (string, string) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i], line[i:]
}

// keyColon is the colon that makes a line a mapping entry: the first one
// followed by a space, a tab or the end of the line. That is YAML's own rule for
// a block key, and it keeps the colon inside a URL or a quoted key from being
// read as one.
func keyColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		if i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t' {
			return i
		}
	}
	return -1
}

// stripComment is the line's content without a trailing comment, for telling a
// block header from a scalar. A '#' inside quotes is part of the value.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch {
		case quote == '"' && s[i] == '\\':
			i++
		case quote != 0 && s[i] == quote:
			quote = 0
		case quote == 0 && (s[i] == '"' || s[i] == '\''):
			quote = s[i]
		case quote == 0 && s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// unquoteScalar is a YAML scalar's value. Both quote styles are read the way
// PBH's own writer produces them: a double quote with backslash escapes, a
// single quote with a doubled one standing for itself.
func unquoteScalar(s string) string {
	if len(s) < 2 {
		return s
	}
	switch s[0] {
	case '\'':
		if s[len(s)-1] != '\'' {
			return s
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	case '"':
		if s[len(s)-1] != '"' {
			return s
		}
		var b strings.Builder
		inner := s[1 : len(s)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] != '\\' || i+1 == len(inner) {
				b.WriteByte(inner[i])
				continue
			}
			i++
			switch inner[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(inner[i])
			}
		}
		return b.String()
	}
	return s
}

// renderScalar writes a value, keeping the quote the line used. An unquoted
// value that would not read back as the same string gets double quotes.
func renderScalar(value string, quote byte) string {
	if quote == '\'' && !strings.Contains(value, "'") {
		return "'" + value + "'"
	}
	if quote == '"' || needsQuote(value) {
		return `"` + escapeDouble(value) + `"`
	}
	return value
}

// needsQuote reports whether a bare scalar would be read as something else:
// an empty value, a number, a boolean, a mapping, a sequence, or a comment.
func needsQuote(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return true
	}
	if strings.Contains(value, ": ") || strings.Contains(value, " #") {
		return true
	}
	switch value[0] {
	case '{', '[', '&', '*', '!', '|', '>', '@', '`', '"', '\'', '#', ',', '?', ':', '-', '%':
		return true
	}
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	return false
}

// escapeDouble is a YAML double-quoted scalar's escaping, over what a URL, a
// host or a key can hold.
func escapeDouble(s string) string {
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

func samePath(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
