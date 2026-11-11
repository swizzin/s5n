// Package ttlconf seeds Tautulli's api_key into config.ini before the app's
// first start, the same way internal/qbtconf seeds qBittorrent's WebUI
// password: the wiring Job has no mount into Tautulli's /config and so can
// never read back a key Tautulli generated for itself, so this package's
// init container writes a known one first.
//
// Tautulli's own code only generates a key when config.ini has none yet
// (verified against a live container; see hack/e2e-kind.sh), so seeding it
// once makes every later start a no-op.
package ttlconf

import "strings"

// Seal ensures config.ini's [General] section holds the given api_key and
// api_enabled = 1. It is line-preserving like qbtconf.Document, though
// Tautulli's config.ini needs far fewer keys touched: everything else is this
// app's own business. It returns the new contents plus whether anything
// changed.
func Seal(existing []byte, apiKey string) (out []byte, changed bool) {
	doc := parse(existing)
	wantAPIEnabled := "1"
	curKey := doc.get("General", "api_key")
	curEnabled := doc.get("General", "api_enabled")
	if curKey == apiKey && curEnabled == wantAPIEnabled {
		return existing, false
	}
	doc.set("General", "api_key", apiKey)
	doc.set("General", "api_enabled", wantAPIEnabled)
	return doc.render(), true
}

type section struct {
	name  string
	lines []string
}

type document struct {
	prefix []string
	secs   []section
}

func parse(data []byte) *document {
	doc := &document{}
	var cur *section
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			doc.secs = append(doc.secs, section{name: strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")})
			cur = &doc.secs[len(doc.secs)-1]
			continue
		}
		if cur == nil {
			if trimmed == "" {
				continue
			}
			doc.prefix = append(doc.prefix, line)
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	return doc
}

func (d *document) find(name string) *section {
	for i := range d.secs {
		if d.secs[i].name == name {
			return &d.secs[i]
		}
	}
	return nil
}

func (d *document) get(sectionName, key string) string {
	s := d.find(sectionName)
	if s == nil {
		return ""
	}
	for _, line := range s.lines {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (d *document) set(sectionName, key, value string) {
	s := d.find(sectionName)
	if s == nil {
		d.secs = append(d.secs, section{name: sectionName})
		s = &d.secs[len(d.secs)-1]
	}
	for i, line := range s.lines {
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			s.lines[i] = " " + key + " = " + value
			return
		}
	}
	s.lines = append(s.lines, " "+key+" = "+value)
}

func (d *document) render() []byte {
	var b strings.Builder
	for _, line := range d.prefix {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, s := range d.secs {
		b.WriteString("[" + s.name + "]\n")
		for _, line := range s.lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}
