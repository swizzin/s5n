package plex

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// prefsPaths are the Preferences.xml attributes holding an absolute path, all
// strings in the 1.43.5 binary: transcoder temp and iTunes XML are documented
// paths, ButlerDatabaseBackupPath is the backup directory, customCertificatePath
// a PKCS#12 file.
var prefsPaths = []string{
	"TranscoderTempDirectory",
	"ButlerDatabaseBackupPath",
	"customCertificatePath",
	"iTunesLibraryXmlPath",
}

// prefsList is the one attribute holding a list: pre-roll videos.
const prefsList = "CinemaTrailersPrerollID"

// attr is one attribute and where its quoted value sits: an edit leaves the rest
// of the file exactly as Plex wrote it.
type attr struct {
	name       string
	start, end int // the value's span, quotes included
	quote      byte
	value      string // decoded
}

func (a attr) replace(value, key, what string) prefEdit {
	esc := &bytes.Buffer{}
	// A bytes.Buffer cannot fail; the error is for writers that can.
	_ = xml.EscapeText(esc, []byte(value))
	return prefEdit{
		start: a.start,
		end:   a.end,
		text:  string(a.quote) + esc.String() + string(a.quote),
		key:   key,
		what:  what,
	}
}

// prefEdit replaces one attribute's value.
type prefEdit struct {
	start, end int
	text       string
	key, what  string
}

// prefsFile is Preferences.xml with its attributes located.
type prefsFile struct {
	data  []byte
	attrs []attr
}

// parsePrefs finds the <Preferences> start tag's attributes. It is not an XML
// parser on purpose: a decoder would rewrite quoting and entity escaping.
func parsePrefs(data []byte) (*prefsFile, error) {
	i := bytes.Index(data, []byte("<Preferences"))
	if i < 0 {
		return nil, fmt.Errorf("has no <Preferences> element")
	}
	attrs, err := scanAttrs(data, i+len("<Preferences"))
	if err != nil {
		return nil, err
	}
	return &prefsFile{data: data, attrs: attrs}, nil
}

// scanAttrs reads attributes until the start tag ends.
func scanAttrs(data []byte, p int) ([]attr, error) {
	var attrs []attr
	for {
		for p < len(data) && isSpace(data[p]) {
			p++
		}
		if p >= len(data) {
			return nil, fmt.Errorf("the <Preferences> element is not closed")
		}
		switch data[p] {
		case '>':
			return attrs, nil
		case '/':
			if p+1 < len(data) && data[p+1] == '>' {
				return attrs, nil
			}
			return nil, fmt.Errorf("unexpected %q inside <Preferences>", data[p])
		}
		a, next, err := scanAttr(data, p)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, a)
		p = next
	}
}

// scanAttr is one name="value" and the offset just past its closing quote.
func scanAttr(data []byte, p int) (attr, int, error) {
	start := p
	for p < len(data) && !isSpace(data[p]) && data[p] != '=' && data[p] != '>' && data[p] != '/' {
		p++
	}
	name := string(data[start:p])
	for p < len(data) && isSpace(data[p]) {
		p++
	}
	if p >= len(data) || data[p] != '=' {
		return attr{}, 0, fmt.Errorf("attribute %q has no value", name)
	}
	p++
	for p < len(data) && isSpace(data[p]) {
		p++
	}
	if p >= len(data) || (data[p] != '"' && data[p] != '\'') {
		return attr{}, 0, fmt.Errorf("attribute %q is not quoted", name)
	}
	quote := data[p]
	open := p
	p++
	for p < len(data) && data[p] != quote {
		p++
	}
	if p >= len(data) {
		return attr{}, 0, fmt.Errorf("attribute %q has no closing quote", name)
	}
	return attr{
		name:  name,
		start: open,
		end:   p + 1,
		quote: quote,
		value: unescapeXML(string(data[open+1 : p])),
	}, p + 1, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// get is the attribute of that name.
func (f *prefsFile) get(name string) (attr, bool) {
	for _, a := range f.attrs {
		if a.name == name {
			return a, true
		}
	}
	return attr{}, false
}

// apply writes the edits back to front, so an earlier span stays put.
func (f *prefsFile) apply(edits []prefEdit) []byte {
	order := append([]prefEdit(nil), edits...)
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && order[j].start < order[j-1].start; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	out := append([]byte(nil), f.data...)
	for i := len(order) - 1; i >= 0; i-- {
		e := order[i]
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	return out
}

// unescapeXML decodes the entities Plex writes into an attribute.
func unescapeXML(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		rest := s[i:]
		switch {
		case strings.HasPrefix(rest, "&amp;"):
			b.WriteByte('&')
			i += len("&amp;") - 1
		case strings.HasPrefix(rest, "&lt;"):
			b.WriteByte('<')
			i += len("&lt;") - 1
		case strings.HasPrefix(rest, "&gt;"):
			b.WriteByte('>')
			i += len("&gt;") - 1
		case strings.HasPrefix(rest, "&quot;"):
			b.WriteByte('"')
			i += len("&quot;") - 1
		case strings.HasPrefix(rest, "&apos;"):
			b.WriteByte('\'')
			i += len("&apos;") - 1
		case strings.HasPrefix(rest, "&#"):
			end := strings.IndexByte(rest, ';')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			code, base := rest[2:end], 10
			if strings.HasPrefix(code, "x") || strings.HasPrefix(code, "X") {
				base, code = 16, code[1:]
			}
			n, err := strconv.ParseInt(code, base, 32)
			if err != nil {
				b.WriteByte(s[i])
				continue
			}
			b.WriteRune(rune(n))
			i += end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
