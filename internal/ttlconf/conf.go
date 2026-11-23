// Package ttlconf seeds Tautulli's config.ini before the app's first start, the
// same way internal/qbtconf seeds qBittorrent's WebUI password: the wiring Job
// has no mount into Tautulli's /config and so can never read back a key
// Tautulli generated for itself, so this package's init container writes a
// known one first.
//
// Tautulli's own code only generates a key when config.ini has none yet
// (verified against a live container; see hack/e2e-kind.sh), so seeding it
// once makes every later start a no-op for the key. The URL root and the app's
// own login are re-sealed on every start, because they are box's and not the
// app's.
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
// (webstart.py:195-213 at v2.18.2: with a password it registers the auth tool
// and logs "authentication is enabled"; without one it logs "authentication is
// disabled" and admits everyone). Behind box the person signs in at the edge
// once, so the app's own login is a second password nobody should be asked for:
// every start clears the pair and the two hash flags, which is what keeps a
// value the app or a person wrote back -- the settings page can set one -- from
// coming alive again on the next restart. http_basic_auth only chooses *how* the
// login is asked (basic instead of a form); it is cleared with the rest so the
// app never has half of a login configured.
//
// Tautulli 2.19 closed both holes an empty password left, and this package seals
// those too:
//
//   - A start where the setup wizard never finished mints a random password and
//     logs it ("Setup wizard login", plexpy/__init__.py:3121-3145 at v2.19.1,
//     generate_first_run_password). A box is not an unfinished install -- the
//     wiring Job is what configures Plex, through the app's API -- so
//     first_run_complete is set, which is also what makes that function return
//     without writing one, and what serves /home instead of the wizard.
//   - Without a password, 2.19 refuses every client outside the local network
//     *and* every request that arrived through a reverse proxy
//     (webstart.py:406-441, no_password_gate: the proxy's own address is local,
//     but Envoy sends X-Forwarded-For and this app's own proxy setting is off,
//     so the request is refused). allow_public_no_password is that gate's switch
//     and says what box means: the login in front of this app is the edge's.
//     It is load-bearing twice, because the same key is what keeps a config
//     upgrading to 2.19 from minting a password on the way (config.py:796-800).
package ttlconf

import "strings"

// The keys this package owns, all spelled the way Tautulli writes and reads
// them: Config._define folds every setting to ini_key = name.lower()
// (plexpy/config.py:473), and ConfigObj itself is case-sensitive. The section is
// the one _CONFIG_DEFINITIONS gives the setting (`GENERAL` for most of them,
// `Advanced` for allow_public_no_password): _define passes the section through
// untouched, so a key written in the wrong one is a setting the app never reads.
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
	want := []struct{ section, key, value string }{
		{sectionGeneral, keyAPIKey, apiKey},
		{sectionGeneral, keyAPIEnabled, "1"},
		{sectionGeneral, keyHTTPRoot, httpRoot},
		{sectionGeneral, keyHTTPUser, empty},
		{sectionGeneral, keyHTTPPass, empty},
		{sectionGeneral, keyHTTPHashPW, "0"},
		{sectionGeneral, keyHTTPHashed, "0"},
		{sectionGeneral, keyHTTPBasic, "0"},
		// The two switches 2.19 put in front of an empty password; see the
		// package comment. first_run_complete is what stops the app minting a
		// password for an install it thinks unfinished, and
		// allow_public_no_password is what keeps its local-network-only gate off
		// the Gateway's requests.
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
