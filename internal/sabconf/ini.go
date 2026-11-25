package sabconf

// ini.go is the INI half of this package: a ConfigObj file split into its
// lines without interpreting anything, so a value this package does not own is
// written back byte for byte. It is deliberately small -- SABnzbd's file is one
// `key = value` per line, a `[section]` header, and the app rewrites the whole
// file in its own formatting on the first save anyway.

import (
	"bytes"
	"fmt"
	"strings"
)

// line is one line of the file: what it said, which section it belongs to, and
// where its value sits when it is a key line.
type line struct {
	raw     string // the line without its newline
	eol     string // "\n" or "\r\n" as it was, "" for a last line without one
	section string // "" at the top of the file
	key     string // "" for a blank line, a comment or a section header
	prefix  string // everything up to the value, for a key line
	value   string // the value without quotes, for a key line
	suffix  string // trailing spaces after the value
	quote   string // ", ', nothing
}

// File is a parsed sabnzbd.ini.
type File struct {
	lines []line
}

// Parse splits an INI file. A line that is neither blank, a comment, a section
// header nor a `key = value` is an error: ConfigObj refuses it, and a caller
// that half-understood it would write a file the app then rejects.
func Parse(data []byte) (*File, error) {
	f := &File{}
	if len(data) == 0 {
		return f, nil
	}
	section, parent := "", ""
	for start, n := 0, 1; ; n++ {
		rel := bytes.IndexByte(data[start:], '\n')
		end := len(data)
		if rel >= 0 {
			end = start + rel
		}
		raw := string(data[start:end])
		eol := "\n"
		if rel < 0 {
			eol = ""
		}
		raw = strings.TrimSuffix(raw, "\r")
		l := line{raw: raw, eol: eol}
		trimmed := strings.TrimSpace(raw)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
			l.section = section
		case strings.HasPrefix(trimmed, "["):
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("line %d: section header without a closing ]", n)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			switch {
			case strings.HasPrefix(name, "["):
				// ConfigObj's nested section: [[name]] under the last [section].
				// SABnzbd writes categories and servers this way.
				inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]"))
				if parent == "" || inner == "" {
					return nil, fmt.Errorf("line %d: nested section %q has no parent section", n, trimmed)
				}
				l.section = parent + "," + inner
				section = l.section
			default:
				l.section = name
				section, parent = name, name
			}
		default:
			eq := strings.Index(raw, "=")
			if eq < 0 {
				return nil, fmt.Errorf("line %d: %q is not a key = value line", n, trimmed)
			}
			name := strings.TrimSpace(raw[:eq])
			if name == "" {
				return nil, fmt.Errorf("line %d: a value with no key", n)
			}
			l.section = section
			l.key = name
			rest := raw[eq+1:]
			lead := len(rest) - len(strings.TrimLeft(rest, " \t"))
			l.prefix = raw[:eq+1] + rest[:lead]
			value := rest[lead:]
			value = strings.TrimRight(value, " \t")
			l.suffix = rest[lead+len(value):]
			for _, q := range []string{`"`, `'`} {
				if len(value) >= 2*len(q) && strings.HasPrefix(value, q) && strings.HasSuffix(value, q) {
					l.quote = q
					value = value[len(q) : len(value)-len(q)]
					break
				}
			}
			l.value = value
		}
		f.lines = append(f.lines, l)
		if rel < 0 {
			return f, nil
		}
		start = end + 1
	}
}

// HasSection reports whether the file declares a section. A duplicate
// declaration is refused by Set when that section is written, because ConfigObj
// merges them and this package cannot say which of two values the app reads.
func (f *File) HasSection(section string) bool {
	for _, l := range f.lines {
		if l.raw != "" && strings.HasPrefix(strings.TrimSpace(l.raw), "[") && l.section == section {
			return true
		}
	}
	return false
}

// Children lists the nested sections under one top-level section, in file
// order: SABnzbd writes each category as [[name]] under [categories].
func (f *File) Children(parent string) []string {
	var out []string
	for _, l := range f.lines {
		if l.raw == "" || !strings.HasPrefix(strings.TrimSpace(l.raw), "[[") {
			continue
		}
		if p, child, ok := strings.Cut(l.section, ","); ok && p == parent {
			out = append(out, child)
		}
	}
	return out
}

// Keys lists the keys of one section, in file order.
func (f *File) Keys(section string) []string {
	var out []string
	for _, l := range f.lines {
		if l.section == section && l.key != "" {
			out = append(out, l.key)
		}
	}
	return out
}

// Get returns one key's value. The section is matched exactly (SABnzbd's file
// is all lower case and ConfigObj is case-sensitive); the key is matched
// without case so a hand-edited spelling is still read.
func (f *File) Get(section, key string) (string, bool) {
	if l, ok := f.find(section, key); ok {
		return l.value, true
	}
	return "", false
}

func (f *File) find(section, key string) (line, bool) {
	var folded *line
	for i := range f.lines {
		l := &f.lines[i]
		if l.section != section || l.key == "" {
			continue
		}
		if l.key == key {
			return *l, true
		}
		if folded == nil && strings.EqualFold(l.key, key) {
			folded = l
		}
	}
	if folded != nil {
		return *folded, true
	}
	return line{}, false
}

// Set writes one key in one section. The value's own line is edited in place,
// so comments, ordering and spacing survive; a key the section does not have is
// inserted after the section's last key line, inside that section.
func (f *File) Set(section, key, value string) error {
	// A section declared twice is a file ConfigObj merges: refuse before
	// editing either copy.
	seen := false
	for i, l := range f.lines {
		if l.raw == "" || !strings.HasPrefix(strings.TrimSpace(l.raw), "[") {
			continue
		}
		if l.section != section {
			continue
		}
		if seen {
			return fmt.Errorf("[%s] is declared twice (lines %d and %d); ConfigObj would merge them, so this cannot tell which value the app reads", section, f.sectionLine(section), i+1)
		}
		seen = true
	}
	var found []int
	for i := range f.lines {
		l := &f.lines[i]
		if l.section == section && l.key != "" && strings.EqualFold(l.key, key) {
			found = append(found, i)
		}
	}
	if len(found) > 1 {
		return fmt.Errorf("%s is set twice in [%s] (lines %d and %d); the app reads one of them", key, section, found[0]+1, found[1]+1)
	}
	if len(found) == 1 {
		l := &f.lines[found[0]]
		l.value = value
		// The line's own spelling stays: the key, the spaces around '=' and
		// any trailing space are the file's, and only the value changes.
		l.raw = l.prefix + value + l.suffix
		return nil
	}
	// Insert after the last key line of the section, or right after its header
	// when it has none. A trailing blank line or comment stays after the new
	// key, which is where the app's own writer puts one.
	at := -1
	header := -1
	inSection := false
	for i, l := range f.lines {
		trimmed := strings.TrimSpace(l.raw)
		if strings.HasPrefix(trimmed, "[") {
			inSection = l.section == section
			if inSection {
				header = i
			}
			continue
		}
		if inSection && l.key != "" {
			at = i
		}
	}
	if at == -1 {
		at = header
	}
	if at == -1 {
		// No section at all: append one. An empty file is the only caller that
		// reaches this, and the section header is all it needs.
		if len(f.lines) > 0 {
			last := &f.lines[len(f.lines)-1]
			if last.eol == "" {
				last.eol = "\n"
			}
		}
		f.lines = append(f.lines, line{raw: "[" + section + "]", eol: "\n"})
		f.lines = append(f.lines, line{raw: key + " = " + value, eol: "\n", section: section, key: key})
		return nil
	}
	if at >= 0 {
		l := &f.lines[at]
		if l.eol == "" {
			l.eol = "\n"
		}
	}
	newLine := line{raw: key + " = " + value, eol: "\n", section: section, key: key}
	f.lines = append(f.lines, line{})
	copy(f.lines[at+2:], f.lines[at+1:])
	f.lines[at+1] = newLine
	return nil
}

func (f *File) sectionLine(section string) int {
	for i, l := range f.lines {
		if l.section == section && l.raw != "" && strings.HasPrefix(strings.TrimSpace(l.raw), "[") {
			return i + 1
		}
	}
	return 0
}

// Bytes renders the file.
func (f *File) Bytes() []byte {
	var b strings.Builder
	for _, l := range f.lines {
		b.WriteString(l.raw)
		b.WriteString(l.eol)
	}
	return []byte(b.String())
}
