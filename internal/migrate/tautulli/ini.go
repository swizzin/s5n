package tautulli

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// iniLine is one line of config.ini that holds a key: its section, its name,
// and where its value sits in the file, so a rewrite replaces the value and
// nothing else.
type iniLine struct {
	n          int
	key        string
	section    string
	valueStart int
	valueEnd   int
	value      string
	quote      string // "'''", `"""`, "'", `"` or ""
}

// iniFile is a parsed config.ini: the bytes it was read from and the key lines
// in it. Parsing is line-based on purpose -- ConfigObj's format is one
// `key = value` per line, and anything fancier (a section header, a comment)
// only has to be recognised, never rewritten.
type iniFile struct {
	data  []byte
	lines []iniLine
}

func parseINI(data []byte) (*iniFile, error) {
	f := &iniFile{data: data}
	section := ""
	for start, n := 0, 0; ; n++ {
		rel := bytes.IndexByte(data[start:], '\n')
		end := len(data)
		if rel >= 0 {
			end = start + rel
		}
		contentEnd := end
		if contentEnd > start && data[contentEnd-1] == '\r' {
			contentEnd--
		}
		if err := f.parseLine(n, start, contentEnd, &section); err != nil {
			return nil, err
		}
		if rel < 0 {
			return f, nil
		}
		start = end + 1
	}
}

func (f *iniFile) parseLine(n, start, end int, section *string) error {
	text := f.data[start:end]
	trimmed := bytes.TrimSpace(text)
	if len(trimmed) == 0 || trimmed[0] == '#' || trimmed[0] == ';' {
		return nil
	}
	if trimmed[0] == '[' {
		if trimmed[len(trimmed)-1] != ']' {
			return fmt.Errorf("line %d: section header without a closing ]", n+1)
		}
		*section = strings.TrimSpace(string(trimmed[1 : len(trimmed)-1]))
		return nil
	}
	eq := bytes.IndexByte(text, '=')
	if eq < 0 {
		// ConfigObj refuses a line it cannot read; a file migrate copies
		// should be refused the same way rather than half-understood.
		return fmt.Errorf("line %d: %q is not a key = value line", n+1, string(trimmed))
	}
	name := strings.TrimSpace(string(text[:eq]))
	if name == "" {
		return fmt.Errorf("line %d: a value with no key", n+1)
	}
	vstart := start + eq + 1
	for vstart < end && (f.data[vstart] == ' ' || f.data[vstart] == '\t') {
		vstart++
	}
	vend := end
	for vend > vstart && (f.data[vend-1] == ' ' || f.data[vend-1] == '\t') {
		vend--
	}
	raw := string(f.data[vstart:vend])
	line := iniLine{n: n, key: name, section: *section, valueStart: vstart, valueEnd: vend, value: raw}
	// ConfigObj quotes a string only when it must (leading or trailing space,
	// a comma that would make a list); the quotes are not part of the value.
	for _, q := range []string{"'''", `"""`, "'", `"`} {
		if len(raw) >= 2*len(q) && strings.HasPrefix(raw, q) && strings.HasSuffix(raw, q) {
			line.quote = q
			line.value = raw[len(q) : len(raw)-len(q)]
			break
		}
	}
	f.lines = append(f.lines, line)
	return nil
}

// get is the first line holding section/key. ConfigObj would keep the last
// duplicate; a file migrate copies should not have one, and editing the first
// is the one a human reading top-down sees.
//
// The name is matched without caring about case, the way Tautulli's setting
// names are: Config folds every setting to `ini_key = name.lower()`
// (plexpy/config.py:473) and reads it back under that lower-case name, so a
// config.ini it wrote is all lower case -- the spelling this package's names
// use. An exact match wins over a fold, because ConfigObj's keys are
// case-sensitive: in a file holding both spellings, the lower-case line is the
// setting Tautulli reads and the other one is a key it keeps without ever
// using, and a rewrite of the unused one would leave the real setting alone.
// The section is matched exactly: Tautulli's sections are the literals
// _CONFIG_DEFINITIONS gives (_define passes `section` through untouched), so a
// `[GENERAL]` block is a different section to it and to ConfigObj.
func (f *iniFile) get(section, name string) (iniLine, bool) {
	var folded *iniLine
	for i, l := range f.lines {
		if l.section != section {
			continue
		}
		if l.key == name {
			return l, true
		}
		if folded == nil && strings.EqualFold(l.key, name) {
			folded = &f.lines[i]
		}
	}
	if folded != nil {
		return *folded, true
	}
	return iniLine{}, false
}

// value is the string value of a key, "" when it is absent.
func (f *iniFile) value(section, name string) string {
	l, _ := f.get(section, name)
	return l.value
}

// rewrite applies the edits and returns the whole file: every byte outside an
// edited value is the byte that was read.
func (f *iniFile) rewrite(edits []edit) []byte {
	byN := make(map[int]iniLine, len(f.lines))
	for _, l := range f.lines {
		byN[l.n] = l
	}
	byLine := make(map[int]edit, len(edits))
	for _, e := range edits {
		byLine[e.line] = e
	}
	order := make([]int, 0, len(byLine))
	for n := range byLine {
		order = append(order, n)
	}
	sort.Ints(order)

	var out bytes.Buffer
	last := 0
	for _, n := range order {
		l := byN[n]
		e := byLine[n]
		out.Write(f.data[last:l.valueStart])
		out.WriteString(e.quote)
		out.WriteString(e.value)
		out.WriteString(e.quote)
		last = l.valueEnd
	}
	out.Write(f.data[last:])
	return out.Bytes()
}
