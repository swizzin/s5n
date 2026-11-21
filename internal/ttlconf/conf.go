// Package ttlconf seeds Tautulli's config.ini before the app's first start, the
// same way internal/qbtconf seeds qBittorrent's WebUI password: the wiring Job
// has no mount into Tautulli's /config and so can never read back a key
// Tautulli generated for itself, so this package's init container writes a
// known one first.
//
// Tautulli's own code only generates a key when config.ini has none yet
// (verified against a live container; see hack/e2e-kind.sh), so seeding it
// once makes every later start a no-op for the key. The three settings below
// are re-sealed on every start, because they are box's and not the app's.
//
// # The URL root
//
// Tautulli answers with absolute redirects built from http_root (webserve.py:
// index raises HTTPRedirect(HTTP_ROOT + "home"), and webstart.py:215-218
// normalises whatever the file holds to "/<root>/"), so an app served under a
// prefix and told nothing about it sends the browser to /home and /auth/logout
// on the host instead of under the prefix. The chart serves it at /<instance>,
// so the file says that, and the route keeps the prefix instead of stripping it.
//
// # Its own login
//
// A non-empty http_password is the switch that turns Tautulli's web login on
// (webstart.py:195-213: with a password it registers the auth tool and logs
// "authentication is enabled"; without one it logs "authentication is disabled"
// and admits everyone). Behind box the person signs in at the edge once, so the
// app's own login is a second password nobody should be asked for: every start
// clears the pair and the two hash flags, which is what keeps a value the app or
// a person wrote back -- the settings page can set one -- from coming alive
// again on the next restart. http_basic_auth only chooses *how* the login is
// asked (basic instead of a form); it is cleared with the rest so the app never
// has half of a login configured.
package ttlconf

import "strings"

// The [General] keys this package owns, all spelled the way Tautulli writes and
// reads them: Config._define folds every setting to ini_key = name.lower()
// (plexpy/config.py:473), and ConfigObj itself is case-sensitive.
const (
	sectionGeneral = "General"
	keyAPIKey      = "api_key"
	keyAPIEnabled  = "api_enabled"
	keyHTTPRoot    = "http_root"
	keyHTTPUser    = "http_username"
	keyHTTPPass    = "http_password"
	keyHTTPHashPW  = "http_hash_password"
	keyHTTPHashed  = "http_hashed_password"
	keyHTTPBasic   = "http_basic_auth"
)

// empty is an empty string as ConfigObj writes it: the quotes are what tells
// the app "no value" rather than "this line is a key with nothing after it".
const empty = `""`

// Seal ensures config.ini holds the given api_key, serves at httpRoot, and has
// its own web login switched off. It is line-preserving like qbtconf.Document:
// everything this package does not own is this app's own business. It returns
// the new contents plus whether anything changed, so a second start writes
// nothing.
func Seal(existing []byte, apiKey, httpRoot string) (out []byte, changed bool) {
	doc := parse(existing)
	want := []struct{ key, value string }{
		{keyAPIKey, apiKey},
		{keyAPIEnabled, "1"},
		{keyHTTPRoot, httpRoot},
		{keyHTTPUser, empty},
		{keyHTTPPass, empty},
		{keyHTTPHashPW, "0"},
		{keyHTTPHashed, "0"},
		{keyHTTPBasic, "0"},
	}
	differs := false
	for _, w := range want {
		if doc.get(sectionGeneral, w.key) != w.value {
			differs = true
			break
		}
	}
	if !differs {
		return existing, false
	}
	for _, w := range want {
		doc.set(sectionGeneral, w.key, w.value)
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
		// A file that ends in a newline splits into one last empty piece, and
		// that piece is the newline rather than a line of its own: keeping it
		// as one makes every rewrite of the file add a blank line at the end.
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
