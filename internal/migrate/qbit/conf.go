package qbit

import (
	"strings"
)

// conf is qBittorrent.conf kept line by line, so every line this pass does not
// change is written back exactly as the app wrote it. QSettings owns the file
// and reads it at startup; a reformatted line (a dropped blank one, a section
// in another order) is a change migrate did not report.
//
// internal/qbtconf has an INI document of its own, but it exists to seed the
// WebUI section qBittorrent's image leaves empty: it trims blank lines at every
// section's edges, it cannot read a string list or rewrite one key of a map,
// and it has no notion of the legacy spellings a conf upgraded in place still
// carries. Reusing it here would change bytes this pass never reported.
type conf struct {
	lines []string
}

func parseConf(raw []byte) *conf {
	return &conf{lines: strings.Split(string(raw), "\n")}
}

func (c *conf) bytes() []byte { return []byte(strings.Join(c.lines, "\n")) }

// section returns the half-open line range inside one [section]. QSettings
// writes a key "A/B" as section [A], key "B" with a backslash, and never
// repeats an element, so the first match is the only one.
func (c *conf) section(name string) (int, int, bool) {
	for i, line := range c.lines {
		if headerSection(line) != name || !isHeader(line) {
			continue
		}
		end := len(c.lines)
		for j := i + 1; j < len(c.lines); j++ {
			if isHeader(c.lines[j]) {
				end = j
				break
			}
		}
		return i + 1, end, true
	}
	return 0, 0, false
}

func isHeader(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")
}

func headerSection(line string) string {
	t := strings.TrimSpace(line)
	return strings.TrimSuffix(strings.TrimPrefix(t, "["), "]")
}

// get returns a key's raw value from one section.
func (c *conf) get(section, key string) (string, bool) {
	start, end, ok := c.section(section)
	if !ok {
		return "", false
	}
	for _, line := range c.lines[start:end] {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// set writes key = value into a section, replacing the key in place, adding it
// to the section, or adding the section when the file has none.
func (c *conf) set(section, key, value string) {
	start, end, ok := c.section(section)
	if !ok {
		if n := len(c.lines); n > 0 && strings.TrimSpace(c.lines[n-1]) != "" {
			c.lines = append(c.lines, "")
		}
		c.lines = append(c.lines, "["+section+"]", key+"="+value)
		return
	}
	for i := start; i < end; i++ {
		if k, _, ok := strings.Cut(c.lines[i], "="); ok && strings.TrimSpace(k) == key {
			c.lines[i] = key + "=" + value
			return
		}
	}
	// Insert before the blank lines that close the section, so the section
	// keeps the shape the app gave it.
	at := end
	for at > start && strings.TrimSpace(c.lines[at-1]) == "" {
		at--
	}
	c.lines = append(c.lines[:at], append([]string{key + "=" + value}, c.lines[at:]...)...)
}

// unquote reads a QSettings INI value: Qt quotes only what it has to, and
// escapes a backslash, a quote and a control character inside the quotes.
func unquote(raw string) string {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return raw
	}
	inner := raw[1 : len(raw)-1]
	var b strings.Builder
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

// quote renders a path back the way QSettings would: bare when it is safe,
// quoted (with its escapes) when it holds whitespace, a quote, a backslash or
// one of the characters that would otherwise read as list syntax
// (qBittorrent's qsettings.cpp, iniEscapedString).
func quote(v string) string {
	if v != "" && !strings.ContainsAny(v, "\"'\\ \t\n,;=@[]") {
		return v
	}
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
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(v[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// splitList reads a QSettings string list, which the writer joins with ", "
// and quotes element by element.
func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var parts []string
	start := 0
	quoted := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, strings.TrimSpace(raw[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(raw[start:]))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, unquote(part))
	}
	return out
}

// joinList renders a list the way QSettings does.
func joinList(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = quote(item)
	}
	return strings.Join(parts, ", ")
}
