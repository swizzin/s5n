// Package qbtconf seeds qBittorrent's WebUI credentials into qBittorrent.conf
// before the app's first start.
//
// qBittorrent 5 stores the WebUI password as a QSettings QByteArray:
//
//	[WebUI]
//	Username=admin
//	Password_PBKDF2="@ByteArray(<salt-b64>:<hash-b64>)"
//
// where hash = PBKDF2-HMAC-SHA512(password, salt, 100000 iterations, 64 bytes),
// salt is 16 random bytes, and Qt's INI writer wraps non-trivial QByteArray values
// in @ByteArray(...) with double quotes. The layout is verified against
// src/base/utils/password.cpp and src/base/preferences.cpp upstream; the formatting
// is verified against a live qBittorrent container (see hack/e2e-kind.sh).
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

// saveRoot is where qBittorrent puts torrents that carry no category. The
// chart mounts the release's data claim at /data; see PLAN.md's data
// contract. internal/apps.SaveRoot is the one place this path is defined.
const saveRoot = apps.SaveRoot

const (
	// Iterations and key length are fixed by qBittorrent; changing either makes
	// every stored login fail.
	Iterations = 100000
	KeyLength  = 64
	SaltLength = 16
)

// Hash derives the qBittorrent password hash in its on-disk form
// ("<salt-b64>:<hash-b64>") using a fresh random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	return HashWithSalt(password, salt)
}

// HashWithSalt is Hash with a caller-supplied salt, for tests and for re-deriving
// a hash to check whether the stored credential already matches.
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

// unquote reverses quote for the subset QSettings writes.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	inner := v[1 : len(v)-1]
	inner = strings.ReplaceAll(inner, `\"`, `"`)
	return strings.ReplaceAll(inner, `\\`, `\`)
}

// PasswordValue is the exact text to place after "Password_PBKDF2=" so that
// qBittorrent reads the hash back as a QByteArray.
func PasswordValue(hash string) string {
	return quote("@ByteArray(" + hash + ")")
}

// ParsePasswordValue extracts the hash from a stored Password_PBKDF2 value,
// accepting both the quoted @ByteArray form and a bare hash.
func ParsePasswordValue(value string) (string, error) {
	v := unquote(strings.TrimSpace(value))
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

// Document is a minimal line-preserving INI document. Everything the caller did
// not ask to change is written back byte for byte, so comments, ordering and
// keys written by qBittorrent itself survive.
type Document struct {
	prefix []string // lines before the first section header
	secs   []section
}

// Parse reads an INI document. Invalid lines are kept as-is rather than rejected:
// the file belongs to qBittorrent, and this tool only edits two keys.
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
			// A key: it belongs to the current section (or to the implicit global
			// section, which qBittorrent never writes).
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
	// Blank lines around section boundaries are render's business: trim them at
	// the edges of every section so Render can re-emit exactly one separator.
	// Interior blank lines (deliberate grouping) are preserved.
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

// Render serialises the document. Sections are separated by a blank line and the
// file ends with a newline, matching what qBittorrent writes itself.
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

// Seal writes the WebUI section qBittorrent should run with. The app's image only
// generates its config file when none exists, so the init container is the
// authority here: seeding the file first would otherwise leave the section with
// nothing but credentials and hand the rest to whatever defaults this build ships.
//
// It returns the new contents plus whether anything actually changed. Existing
// credentials for the same password are reused, so running it on every start is
// cheap and does not rewrite the file.
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
		// The data contract in one setting each. qBittorrent defaults its save path
		// to a directory on the config volume, and ignores a category's path unless
		// automatic torrent management is on — so a download from an Arr would land
		// outside /data and could never be hardlinked into the library.
		// qBittorrent 5 nests these under [Preferences] with QSettings-escaped keys,
		// not in a [Session] section of their own.
		{"Preferences", `Session\DefaultSavePath`, saveRoot},
		{"Preferences", `Session\TempPath`, saveRoot + "/temp"},
		{"Preferences", `Session\UseCategoryPathsInManualMode`, "true"},
		{"WebUI", "Address", "*"},
		{"WebUI", "Port", strconv.Itoa(port)},
		{"WebUI", "ServerDomains", "*"},
		// The release is only reachable through the Gateway (or directly, on the
		// LAN); a validated Host header would reject the proxy's.
		{"WebUI", "HostHeaderValidation", "false"},
		// No localhost bypass: the password is the only way in.
		{"WebUI", "LocalHostAuth", "false"},
		{"WebUI", "AuthSubnetWhitelistEnabled", "false"},
		// The API is cookie-authenticated; CSRF protection only breaks non-browser
		// clients, and nothing here is served to a browser we do not control.
		{"WebUI", "CSRFProtection", "false"},
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
