package qbit

import (
	"strings"

	"github.com/swizzin/s5n/internal/qbtconf"
)

// conf is qBittorrent.conf kept line by line, so every line this pass does not
// change is written back as the app wrote it: QSettings owns the file, and a
// reformatted line is a change migrate did not report. internal/qbtconf's INI
// document exists to seed the WebUI section and would change bytes here (it trims
// blank lines, cannot read a string list, and ignores legacy spellings).
type conf struct {
	lines []string
}

func parseConf(raw []byte) *conf {
	return &conf{lines: strings.Split(string(raw), "\n")}
}

func (c *conf) bytes() []byte { return []byte(strings.Join(c.lines, "\n")) }

// section is the half-open line range inside one [section]; QSettings never
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

// set replaces a key in place, or adds it (or its section) when absent.
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
	// Insert before the blank lines closing the section, keeping the app's shape.
	at := end
	for at > start && strings.TrimSpace(c.lines[at-1]) == "" {
		at--
	}
	c.lines = append(c.lines[:at], append([]string{key + "=" + value}, c.lines[at:]...)...)
}

// quote renders a path as QSettings would: bare when safe, quoted and escaped when
// it holds whitespace or list syntax (qsettings.cpp, iniEscapedString).
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

// splitList reads a QSettings string list, joined with ", " per element.
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
		out = append(out, qbtconf.Unquote(part))
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
