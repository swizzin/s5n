package sabconf

import (
	"bytes"
	"fmt"
	"strings"
)

// line is one line of the file: what it said, its section, and where its value
// sits when it is a key line.
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
// header nor `key = value` is an error: ConfigObj refuses it, and a file this
// half-understood would be one the app rejects.
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
		if err := f.readLine(strings.TrimSuffix(raw, "\r"), eol, n, &section, &parent); err != nil {
			return nil, err
		}
		if rel < 0 {
			return f, nil
		}
		start = end + 1
	}
}

// readLine appends one line, carrying the current section and its parent
// forward.
func (f *File) readLine(raw, eol string, n int, section, parent *string) error {
	l := line{raw: raw, eol: eol}
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
		l.section = *section
	case strings.HasPrefix(trimmed, "["):
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]"))
		if name == "" || !strings.HasSuffix(trimmed, "]") {
			return fmt.Errorf("line %d: section header without a name and a closing ]", n)
		}
		switch {
		case strings.HasPrefix(name, "["):
			// ConfigObj's nested section: SABnzbd writes categories and servers
			// as [[name]] under the last [section].
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]"))
			if *parent == "" || inner == "" {
				return fmt.Errorf("line %d: nested section %q has no parent section", n, trimmed)
			}
			l.section = *parent + "," + inner
			*section = l.section
		default:
			l.section = name
			*section, *parent = name, name
		}
	default:
		if err := l.readKey(raw, n, *section); err != nil {
			return err
		}
	}
	f.lines = append(f.lines, l)
	return nil
}

// readKey fills a key line's value and the offsets an edit keeps its spelling
// with.
func (l *line) readKey(raw string, n int, section string) error {
	eq := strings.Index(raw, "=")
	if eq < 0 {
		return fmt.Errorf("line %d: %q is not a key = value line", n, strings.TrimSpace(raw))
	}
	name := strings.TrimSpace(raw[:eq])
	if name == "" {
		return fmt.Errorf("line %d: a value with no key", n)
	}
	l.section, l.key = section, name
	rest := raw[eq+1:]
	lead := len(rest) - len(strings.TrimLeft(rest, " \t"))
	l.prefix = raw[:eq+1] + rest[:lead]
	value := strings.TrimRight(rest[lead:], " \t")
	l.suffix = rest[lead+len(value):]
	for _, q := range []string{`"`, `'`} {
		if len(value) >= 2*len(q) && strings.HasPrefix(value, q) && strings.HasSuffix(value, q) {
			l.quote = q
			value = value[len(q) : len(value)-len(q)]
			break
		}
	}
	l.value = value
	return nil
}

// HasSection reports whether the file declares a section. A duplicate is
// refused by Set when that section is written, because ConfigObj merges them.
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

// Get returns one key's value. The section is matched exactly (the file is all
// lower case and ConfigObj is case-sensitive); the key is matched without case
// so a hand-edited spelling is still read.
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

// Set writes one key in one section. The value's line is edited in place, so
// comments, ordering and spacing survive; a missing key is inserted inside its
// section.
func (f *File) Set(section, key, value string) error {
	if err := f.checkSection(section); err != nil {
		return err
	}
	found, err := f.findKey(section, key)
	if err != nil {
		return err
	}
	if len(found) == 1 {
		l := &f.lines[found[0]]
		l.value = value
		// The line's own spelling stays: the key, the spaces around '=' and any
		// trailing space belong to the file.
		l.raw = l.prefix + value + l.suffix
		return nil
	}
	f.insert(section, key, value)
	return nil
}

// checkSection refuses a section declared twice: ConfigObj merges the two, so
// this cannot tell which value the app reads.
func (f *File) checkSection(section string) error {
	first, seen := 0, false
	for i, l := range f.lines {
		if l.raw == "" || !strings.HasPrefix(strings.TrimSpace(l.raw), "[") || l.section != section {
			continue
		}
		if seen {
			return fmt.Errorf("[%s] is declared twice (lines %d and %d); ConfigObj would merge them, so this cannot tell which value the app reads", section, first, i+1)
		}
		first, seen = i+1, true
	}
	return nil
}

// findKey is every line of one section that holds one key, without case.
func (f *File) findKey(section, key string) ([]int, error) {
	var found []int
	for i := range f.lines {
		l := &f.lines[i]
		if l.section == section && l.key != "" && strings.EqualFold(l.key, key) {
			found = append(found, i)
		}
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("%s is set twice in [%s] (lines %d and %d); the app reads one of them", key, section, found[0]+1, found[1]+1)
	}
	return found, nil
}

// insert adds a key after the section's last key line, or after its header when
// it has none; a trailing blank or comment stays after the new key, where the
// app's own writer puts one.
func (f *File) insert(section, key, value string) {
	at, header, inSection := -1, -1, false
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
		// No section at all: an empty file is the only caller that reaches this.
		if len(f.lines) > 0 && f.lines[len(f.lines)-1].eol == "" {
			f.lines[len(f.lines)-1].eol = "\n"
		}
		f.lines = append(f.lines,
			line{raw: "[" + section + "]", eol: "\n"},
			line{raw: key + " = " + value, eol: "\n", section: section, key: key})
		return
	}
	if f.lines[at].eol == "" {
		f.lines[at].eol = "\n"
	}
	newLine := line{raw: key + " = " + value, eol: "\n", section: section, key: key}
	f.lines = append(f.lines, line{})
	copy(f.lines[at+2:], f.lines[at+1:])
	f.lines[at+1] = newLine
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
