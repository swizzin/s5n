// Package qbtconf seeds qBittorrent's WebUI section into qBittorrent.conf
// before the app's first start: credentials, the data contract's save paths and
// the private ranges its login skips.
//
// qBittorrent 5 stores the password as a QSettings QByteArray holding
// PBKDF2-HMAC-SHA512(password, 16-byte salt, 100000 iterations, 64 bytes):
// `Password_PBKDF2="@ByteArray(<salt-b64>:<hash-b64>)"`. The layout is verified
// against src/base/utils/password.cpp and src/base/preferences.cpp upstream.
package qbtconf

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/apps"
)

// saveRoot is where uncategorised torrents go; internal/apps.SaveRoot is the
// one place the path is defined.
const saveRoot = apps.SaveRoot

const (
	// Iterations and key length are fixed by qBittorrent; changing either makes
	// every stored login fail.
	Iterations = 100000
	KeyLength  = 64
	SaltLength = 16
)

// AuthBypassSubnets is the private space qBittorrent's WebUI admits without a
// login: it cannot switch its login off, so what reaches it is Envoy (after the
// edge's login, which gives qBittorrent's paths no bypass) or a pod the
// release's NetworkPolicy lets in. That is the boundary the Arrs sit behind.
var authBypassSubnets = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"fc00::/7",
}

// subnetListValue renders authBypassSubnets as QSettings writes a QStringList:
// joined with ", ", unquoted, because Qt quotes only an element containing ';',
// ',' or '=' (qsettings.cpp, iniEscapedStringList). Quoting it whole would read
// back as one bogus subnet.
func subnetListValue() string {
	return strings.Join(authBypassSubnets, ", ")
}

// Hash derives the qBittorrent password hash in its on-disk form
// ("<salt-b64>:<hash-b64>") using a fresh random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	return HashWithSalt(password, salt)
}

// HashWithSalt is Hash with a caller-supplied salt.
func HashWithSalt(password string, salt []byte) (string, error) {
	key, err := pbkdf2.Key(sha512.New, password, salt, Iterations, KeyLength)
	if err != nil {
		return "", fmt.Errorf("pbkdf2: %w", err)
	}
	return base64.StdEncoding.EncodeToString(salt) + ":" + base64.StdEncoding.EncodeToString(key), nil
}

// Verify reports whether password produces the given hash. Malformed hashes are
// never valid.
func Verify(hash, password string) bool {
	saltB64, keyB64, ok := strings.Cut(hash, ":")
	if !ok {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) != SaltLength {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(want) != KeyLength {
		return false
	}
	got, err := pbkdf2.Key(sha512.New, password, salt, Iterations, KeyLength)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// quote renders a value for a QSettings INI file: plain when it is safe unquoted,
// otherwise wrapped in double quotes with embedded quotes and backslashes escaped.
func quote(v string) string {
	if v != "" && !strings.ContainsAny(v, "\"'\\ \t\n,;@[]") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// Unquote reads a QSettings INI value as the app wrote it: Qt quotes only what
// it has to. Every reader of a stored path or list goes through it, so
// discovery and the rewriter cannot disagree.
func Unquote(raw string) string {
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

// PasswordValue is the exact text after "Password_PBKDF2=" so that
// qBittorrent reads the hash back as a QByteArray.
func PasswordValue(hash string) string {
	return quote("@ByteArray(" + hash + ")")
}

// ParsePasswordValue extracts the hash from a stored Password_PBKDF2 value,
// accepting the quoted @ByteArray form and a bare hash.
func ParsePasswordValue(value string) (string, error) {
	v := Unquote(strings.TrimSpace(value))
	if strings.HasPrefix(v, "@ByteArray(") {
		if !strings.HasSuffix(v, ")") {
			return "", fmt.Errorf("malformed @ByteArray value %q", v)
		}
		v = strings.TrimSuffix(strings.TrimPrefix(v, "@ByteArray("), ")")
	}
	if _, _, ok := strings.Cut(v, ":"); !ok {
		return "", fmt.Errorf("value %q is not a salt:hash pair", v)
	}
	return v, nil
}

// section is one INI section with its lines preserved verbatim.
type section struct {
	name  string
	lines []string
}

// Document is a line-preserving INI document: everything the caller did not
// change is written back byte for byte, so comments, ordering and keys written
// by qBittorrent itself survive.
type Document struct {
	prefix []string // lines before the first section header
	secs   []section
}

// Parse reads an INI document. Invalid lines are kept as-is: the file belongs
// to qBittorrent, and this tool only edits a few keys.
func Parse(data []byte) *Document {
	doc := &Document{}
	var cur *section
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			doc.secs = append(doc.secs, section{name: strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")})
			cur = &doc.secs[len(doc.secs)-1]
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			if cur == nil {
				doc.prefix = append(doc.prefix, line)
			} else {
				cur.lines = append(cur.lines, line)
			}
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(key) != "" {
			if cur == nil {
				doc.secs = append(doc.secs, section{})
				cur = &doc.secs[len(doc.secs)-1]
			}
			cur.lines = append(cur.lines, line)
			continue
		}
		if cur == nil {
			doc.prefix = append(doc.prefix, line)
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	// Blank lines at section edges are trimmed so Render can emit one separator;
	// interior ones are kept.
	for i := range doc.secs {
		lines := doc.secs[i].lines
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		doc.secs[i].lines = lines
	}
	for len(doc.prefix) > 0 && strings.TrimSpace(doc.prefix[len(doc.prefix)-1]) == "" {
		doc.prefix = doc.prefix[:len(doc.prefix)-1]
	}
	return doc
}

func (d *Document) find(name string) *section {
	for i := range d.secs {
		if d.secs[i].name == name {
			return &d.secs[i]
		}
	}
	return nil
}

// Get returns the raw value of a key in a section.
func (d *Document) Get(sectionName, key string) (string, bool) {
	s := d.find(sectionName)
	if s == nil {
		return "", false
	}
	for _, line := range s.lines {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// Set writes key = value in a section, replacing an existing key in place or
// appending it, creating the section when needed.
func (d *Document) Set(sectionName, key, value string) {
	s := d.find(sectionName)
	if s == nil {
		d.secs = append(d.secs, section{name: sectionName})
		s = &d.secs[len(d.secs)-1]
	}
	for i, line := range s.lines {
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			s.lines[i] = key + "=" + value
			return
		}
	}
	s.lines = append(s.lines, key+"="+value)
}

// Render serialises the document with one blank line between sections and a
// final newline, matching what qBittorrent writes itself.
func (d *Document) Render() []byte {
	var b strings.Builder
	for _, line := range d.prefix {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if len(d.prefix) > 0 && len(d.secs) > 0 {
		b.WriteByte('\n')
	}
	for i, s := range d.secs {
		if i > 0 {
			b.WriteByte('\n')
		}
		if s.name != "" {
			b.WriteString("[" + s.name + "]\n")
		}
		for _, line := range s.lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// Seal writes the WebUI section qBittorrent should run with. The image only
// generates its config when none exists, so the init container is the authority:
// a file seeded with nothing but credentials would leave the rest to the build's
// defaults. A stored credential for the same password is reused, so a start
// rewrites nothing.
func Seal(existing []byte, username, password string, port int) (out []byte, changed bool, err error) {
	doc := Parse(existing)

	storedHash := ""
	if v, ok := doc.Get("WebUI", "Password_PBKDF2"); ok {
		if h, perr := ParsePasswordValue(v); perr == nil && Verify(h, password) {
			storedHash = h
		}
	}

	desired := []struct{ section, key, value string }{
		{"WebUI", "Username", quote(username)},
		// The data contract in one setting each: qBittorrent defaults its save
		// path onto the config volume and ignores a category's path unless
		// manual-mode category paths are on, so a download would never be
		// hardlinkable. qBittorrent 5 nests these under [Preferences].
		{"Preferences", `Session\DefaultSavePath`, saveRoot},
		{"Preferences", `Session\TempPath`, saveRoot + "/temp"},
		{"Preferences", `Session\UseCategoryPathsInManualMode`, "true"},
		{"WebUI", "Address", "*"},
		{"WebUI", "Port", strconv.Itoa(port)},
		{"WebUI", "ServerDomains", "*"},
		// The WebUI answers Envoy, which forwards the request's own Host; host
		// validation here would reject a caller the subnet whitelist admits.
		{"WebUI", "HostHeaderValidation", "false"},
		// qBittorrent can't switch its login off, so it skips it instead: for
		// loopback (port-forward) and for the private ranges pods and the Gateway
		// sit in. See authBypassSubnets.
		{"WebUI", "LocalHostAuth", "false"},
		{"WebUI", "AuthSubnetWhitelistEnabled", "true"},
		{"WebUI", "AuthSubnetWhitelist", subnetListValue()},
		// A same-site page holding the edge's cookie could POST here, the WebUI's own
		// login being skipped for the Gateway's address; a client with neither Origin
		// nor Referer passes (src/webui/webapplication.cpp:969 at release-5.3.1).
		{"WebUI", "CSRFProtection", "true"},
		{"LegalNotice", "Accepted", "true"},
	}
	if storedHash != "" {
		desired = append(desired, struct{ section, key, value string }{
			"WebUI", "Password_PBKDF2", PasswordValue(storedHash),
		})
	}

	unchanged := storedHash != ""
	for _, d := range desired {
		if !unchanged {
			break
		}
		if v, ok := doc.Get(d.section, d.key); !ok || v != d.value {
			unchanged = false
		}
	}
	if unchanged {
		return existing, false, nil
	}

	if storedHash == "" {
		hash, herr := Hash(password)
		if herr != nil {
			return nil, false, herr
		}
		desired = append(desired, struct{ section, key, value string }{
			"WebUI", "Password_PBKDF2", PasswordValue(hash),
		})
	}
	for _, d := range desired {
		doc.Set(d.section, d.key, d.value)
	}
	return doc.Render(), true, nil
}

// FormatIterations exists so callers can log the cost; it is not configurable.
func FormatIterations() string { return strconv.Itoa(Iterations) }
