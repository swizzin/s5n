// Package bazconf seeds the two settings Bazarr needs before it starts: the API
// key the wiring Job authenticates with and the URL prefix the chart serves it
// under.
//
// Neither can be set through the API: Bazarr loads config/config.yaml
// (bazarr/app/config.py:519 at v1.6.2) and rewrites it whole on every start
// (config.py:558-574), and its blueprints are registered under general.base_url
// once, at import (bazarr/app/server.py:21-22). So the file is spliced by
// section, and only the two keys this package owns are touched.
package bazconf

import (
	"fmt"
	"strings"
)

// The keys this package owns and the sections they live in, both names Bazarr's
// own validators (config.py:92 and config.py:185).
const (
	sectionGeneral = "general"
	sectionAuth    = "auth"
	keyBaseURL     = "base_url"
	keyAPIKey      = "apikey"
)

// Seal ensures config.yaml serves at baseURL and holds apiKey, and reports
// whether anything changed.
//
// A file it cannot edit safely -- a flow mapping (`general: {...}`), a section
// declared twice -- is an error, not a guess: the app rewrites the file from
// memory on the next start, and an init container that fails is a pod that says
// so.
func Seal(existing []byte, apiKey, baseURL string) (out []byte, changed bool, err error) {
	doc, err := parse(existing)
	if err != nil {
		return existing, false, err
	}
	want := []struct{ section, key, value string }{
		{sectionGeneral, keyBaseURL, baseURL},
		{sectionAuth, keyAPIKey, apiKey},
	}
	differs := false
	for _, w := range want {
		if got, ok := doc.get(w.section, w.key); !ok || got != w.value {
			differs = true
			break
		}
	}
	if !differs {
		return existing, false, nil
	}
	for _, w := range want {
		doc.set(w.section, w.key, w.value)
	}
	return doc.render(), true, nil
}

// section is one top-level mapping: its key line and the lines under it.
type section struct {
	name string
	line string   // the "name:" line as it was
	body []string // the indented lines, exactly as they were
}

type document struct {
	prefix []string // lines before the first top-level key
	secs   []section
	last   string // the file's own newline state, so a render keeps it
}

// parse splits a YAML file into top-level sections, reading no value it does
// not own.
func parse(data []byte) (*document, error) {
	doc := &document{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	doc.last = "\n"
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else {
		doc.last = ""
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// A document marker belongs to the file's prologue. A second one is a
		// second document, and the app's loader reads only the first.
		if trimmed == "---" {
			if len(doc.secs) > 0 {
				return nil, fmt.Errorf("holds more than one YAML document (`---` after %q); the app reads only one", doc.secs[0].name)
			}
			doc.prefix = append(doc.prefix, line)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if len(doc.secs) == 0 {
				doc.prefix = append(doc.prefix, line)
			} else {
				doc.secs[len(doc.secs)-1].body = append(doc.secs[len(doc.secs)-1].body, line)
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			if len(doc.secs) == 0 {
				return nil, fmt.Errorf("starts with an indented line or a list item, which is not a YAML mapping of sections")
			}
			doc.secs[len(doc.secs)-1].body = append(doc.secs[len(doc.secs)-1].body, line)
			continue
		}
		name, ok := topLevelKey(line)
		if !ok {
			return nil, fmt.Errorf("line %q is not a top-level `key:` mapping", line)
		}
		doc.secs = append(doc.secs, section{name: name, line: line})
	}
	seen := map[string]bool{}
	for _, s := range doc.secs {
		if seen[s.name] {
			return nil, fmt.Errorf("declares %q twice; the app reads only one of them", s.name)
		}
		seen[s.name] = true
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.line), s.name+":")); rest != "" {
			return nil, fmt.Errorf("%q is written inline (%q); this can only edit a section's keys, one per line", s.name, rest)
		}
	}
	return doc, nil
}

// topLevelKey reads a top-level `name:` line, possibly quoted.
func topLevelKey(line string) (name string, ok bool) {
	s := strings.TrimRight(line, " \t")
	if len(s) > 0 && (s[0] == '\'' || s[0] == '"') {
		end := strings.IndexByte(s[1:], s[0])
		if end < 0 {
			return "", false
		}
		name = s[1 : 1+end]
		if !strings.HasPrefix(s[2+end:], ":") {
			return "", false
		}
		return name, true
	}
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", false
	}
	name = s[:i]
	for _, r := range name {
		if !keyChar(r) {
			return "", false
		}
	}
	return name, true
}

// keyChar is what a plain YAML mapping key is made of.
func keyChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.':
		return true
	}
	return false
}

func (d *document) find(name string) *section {
	for i := range d.secs {
		if d.secs[i].name == name {
			return &d.secs[i]
		}
	}
	return nil
}

// get reads one key's scalar value. ok is false when the key is absent.
func (d *document) get(sectionName, key string) (value string, ok bool) {
	s := d.find(sectionName)
	if s == nil {
		return "", false
	}
	indent := childIndent(s)
	for _, line := range s.body {
		if indent != "" && !strings.HasPrefix(line, indent) {
			continue
		}
		k, v, found := splitKey(strings.TrimPrefix(line, indent))
		if !found || k != key {
			continue
		}
		return scalar(v), true
	}
	return "", false
}

// set writes one key, under its section, leaving every other line in place.
func (d *document) set(sectionName, key, value string) {
	s := d.find(sectionName)
	if s == nil {
		d.secs = append(d.secs, section{
			name: sectionName,
			line: sectionName + ":",
			body: []string{"  " + key + ": " + quote(value)},
		})
		return
	}
	indent := childIndent(s)
	if indent == "" {
		indent = "  "
	}
	for i, line := range s.body {
		if !strings.HasPrefix(line, indent) {
			continue
		}
		k, _, ok := splitKey(strings.TrimPrefix(line, indent))
		if ok && k == key {
			s.body[i] = indent + key + ": " + quote(value)
			return
		}
	}
	// The section exists without the key: it goes last in the section.
	s.body = append(s.body, indent+key+": "+quote(value))
}

// childIndent is the indentation of a section's first child line; an empty
// section has none.
func childIndent(s *section) string {
	for _, line := range s.body {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	return ""
}

// splitKey splits "key: value" at the first colon followed by a space or the
// end of the line, where YAML ends a plain key.
func splitKey(line string) (key, value string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		if i+1 < len(line) && line[i+1] != ' ' && line[i+1] != '\t' {
			continue
		}
		key = strings.TrimSpace(line[:i])
		value = strings.TrimSpace(line[i+1:])
		return key, value, key != ""
	}
	return "", "", false
}

// scalar is a YAML scalar's value, unquoted, with a plain value's comment
// dropped. Only the two shapes this file uses are handled.
func scalar(v string) string {
	if v == "" {
		return ""
	}
	switch v[0] {
	case '"':
		if end := lastQuote(v, '"'); end > 0 {
			v = v[1:end]
			var b strings.Builder
			for i := 0; i < len(v); i++ {
				if v[i] == '\\' && i+1 < len(v) {
					i++
					switch v[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(v[i])
					}
					continue
				}
				b.WriteByte(v[i])
			}
			return b.String()
		}
	case '\'':
		if end := lastQuote(v, '\''); end > 0 {
			return strings.ReplaceAll(v[1:end], "''", "'")
		}
	case '#':
		return ""
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	if v == "~" || v == "null" {
		return ""
	}
	return strings.TrimSpace(v)
}

// lastQuote is the index of the closing quote, counting a doubled single quote
// as content.
func lastQuote(v string, quote byte) int {
	for i := len(v) - 1; i > 0; i-- {
		if v[i] != quote {
			continue
		}
		if quote == '\'' {
			// In a single-quoted scalar '' is an escaped quote, not the end.
			n := 0
			for j := i - 1; j >= 1 && v[j] == '\''; j-- {
				n++
			}
			if n%2 == 1 {
				continue
			}
		}
		return i
	}
	return -1
}

// quote writes v as a double-quoted YAML scalar, so a `#` or `:` in a path or a
// key is not read as a comment or a mapping.
func quote(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(v[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (d *document) render() []byte {
	var b strings.Builder
	for _, line := range d.prefix {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, s := range d.secs {
		b.WriteString(s.line)
		b.WriteByte('\n')
		for _, line := range s.body {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	out := b.String()
	if d.last == "" {
		return []byte(strings.TrimSuffix(out, "\n"))
	}
	return []byte(out)
}
