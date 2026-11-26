// Package ttlconf seeds Tautulli's config.ini before the app's first start: the
// wiring Job has no mount into /config and can never read back a key Tautulli
// generated, so the init container writes a known one first.
//
// The app's own login is sealed off on every start (docs/box.md "Tautulli"), and
// http_root is set to the chart's prefix: Tautulli builds absolute redirects from
// it (plexpy/webserve.py: index raises HTTPRedirect(HTTP_ROOT + "home"),
// webstart.py:215-218 at v2.18.2), so without it the browser lands on the box's
// own /home.
package ttlconf

import "strings"

// The keys this package owns, spelled as Tautulli writes them: Config._define
// folds every name to ini_key = name.lower() (plexpy/config.py:473), and ConfigObj
// is case-sensitive. The section is the one _CONFIG_DEFINITIONS gives the
// setting; a key in the wrong one is a setting the app never reads.
const (
	sectionGeneral  = "General"
	sectionAdvanced = "Advanced"
	keyAPIKey       = "api_key"
	keyAPIEnabled   = "api_enabled"
	keyHTTPRoot     = "http_root"
	keyHTTPUser     = "http_username"
	keyHTTPPass     = "http_password"
	keyHTTPHashPW   = "http_hash_password"
	keyHTTPHashed   = "http_hashed_password"
	keyHTTPBasic    = "http_basic_auth"
	keyFirstRun     = "first_run_complete"
	keyPublicNoPW   = "allow_public_no_password"
)

// empty is an empty string as ConfigObj writes it: the quotes say "no value"
// rather than "a key with nothing after it".
const empty = `""`

// Seal ensures config.ini holds apiKey, serves at httpRoot and has its own web
// login off. It is line-preserving: everything it does not own is the app's. A
// second start over its own output writes nothing. Why each key, with Tautulli's
// sources: docs/box.md "Tautulli".
func Seal(existing []byte, apiKey, httpRoot string) (out []byte, changed bool) {
	doc := parse(existing)
	want := []struct{ section, key, value string }{
		{sectionGeneral, keyAPIKey, apiKey},
		{sectionGeneral, keyAPIEnabled, "1"},
		{sectionGeneral, keyHTTPRoot, httpRoot},
		{sectionGeneral, keyHTTPUser, empty},
		{sectionGeneral, keyHTTPPass, empty},
		{sectionGeneral, keyHTTPHashPW, "0"},
		{sectionGeneral, keyHTTPHashed, "0"},
		{sectionGeneral, keyHTTPBasic, "0"},
		// The two switches 2.19 put in front of an empty password; see
		// docs/box.md "Tautulli".
		{sectionGeneral, keyFirstRun, "1"},
		{sectionAdvanced, keyPublicNoPW, "1"},
	}
	differs := false
	for _, w := range want {
		if doc.get(w.section, w.key) != w.value {
			differs = true
			break
		}
	}
	if !differs {
		return existing, false
	}
	for _, w := range want {
		doc.set(w.section, w.key, w.value)
	}
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
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, line := range lines {
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
		// A file that ends in a newline splits into one last empty piece, which is
		// the newline rather than a line: keeping it adds a blank line on every
		// rewrite.
		if i == len(lines)-1 && line == "" {
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
