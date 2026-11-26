package arr

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// configXML rewrites the path and listen elements, leaving every other byte as it
// was, because the app reads this file at startup and writes it back. It is one
// flat list under <Config> (ConfigFileProvider.cs:124-140 at v4.0.14.2938), so a
// full XML parse would only add a reformatting risk.
func (a *analysis) configXML() error {
	ep, ok := a.in.Targets[a.app]
	if !ok {
		return fmt.Errorf("the plan has no endpoint for %s, so its port and URL base cannot be set", a.app)
	}
	file := filepath.Join(a.dir, "config.xml")
	info, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("read config.xml: %w", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read config.xml: %w", err)
	}

	type element struct{ name, value, what string }
	var edits []element
	// The chart's port, bind address and URL base are what the app must listen on
	// (docs/box.md, "Ports go to the chart's defaults"). TLS ends at the gateway and
	// probes speak plain HTTP, so an HTTPS source is turned back to HTTP; its
	// certificate paths are still rewritten below.
	for _, e := range []element{
		{"BindAddress", "*", "bind address"},
		{"Port", strconv.Itoa(ep.Port), "port"},
		{"UrlBase", ep.URLBase, "url base"},
		{"EnableSsl", "False", "tls"},
	} {
		want := xmlEscape(e.value)
		if stored, ok := elementText(raw, e.name); ok && stored == want {
			continue
		}
		edits = append(edits, element{e.name, want, e.what})
	}
	// An empty SslCertPath means "no certificate" and stays empty: writing a path
	// would only add a startup read of a missing file. A non-empty one is read at
	// startup, so an uncovered path is a gap.
	for _, name := range a.configPathElements() {
		stored, ok := elementText(raw, name)
		if !ok || stored == "" {
			continue
		}
		value := xmlUnescape(stored)
		out, covered := a.in.Plan.Rewrite(a.app, value)
		if !covered {
			a.gap(a.app, "config.xml", name, value)
			continue
		}
		if out == value {
			continue
		}
		edits = append(edits, element{name, xmlEscape(out), "path"})
	}
	if len(edits) == 0 {
		return nil
	}
	out := raw
	for _, e := range edits {
		out, err = setElement(out, e.name, e.value)
		if err != nil {
			return err
		}
		a.add("config.xml", e.name, e.what)
	}
	a.conf, a.confMode = out, info.Mode().Perm()
	return nil
}

// configPathElements are the path elements; Prowlarr has no SslKeyPath
// (ConfigFileProvider.cs:50-58 at v2.7.0.5723).
func (a *analysis) configPathElements() []string {
	if a.kind == kindProwlarr {
		return []string{"SslCertPath", "UpdateScriptPath"}
	}
	return []string{"SslCertPath", "SslKeyPath", "UpdateScriptPath"}
}

// elementText returns the text between <name> and </name>, as it sits in the
// file. A self-closing <name/> reads as empty.
func elementText(raw []byte, name string) (string, bool) {
	open := []byte("<" + name + ">")
	if i := bytes.Index(raw, open); i >= 0 {
		rest := raw[i+len(open):]
		if j := bytes.Index(rest, []byte("</"+name+">")); j >= 0 {
			return string(rest[:j]), true
		}
		return "", false
	}
	for _, empty := range [][]byte{[]byte("<" + name + "/>"), []byte("<" + name + " />")} {
		if bytes.Contains(raw, empty) {
			return "", true
		}
	}
	return "", false
}

// setElement replaces an element's text, or adds it before the root closes: an
// older source can lack UrlBase or UpdateScriptPath, and its default is not the
// release's value.
func setElement(raw []byte, name, value string) ([]byte, error) {
	open := []byte("<" + name + ">")
	if i := bytes.Index(raw, open); i >= 0 {
		start := i + len(open)
		j := bytes.Index(raw[start:], []byte("</"+name+">"))
		if j < 0 {
			return nil, fmt.Errorf("config.xml: <%s> has no closing tag", name)
		}
		out := make([]byte, 0, len(raw)+len(value))
		out = append(out, raw[:start]...)
		out = append(out, value...)
		out = append(out, raw[start+j:]...)
		return out, nil
	}
	for _, empty := range [][]byte{[]byte("<" + name + "/>"), []byte("<" + name + " />")} {
		if i := bytes.Index(raw, empty); i >= 0 {
			replacement := "<" + name + ">" + value + "</" + name + ">"
			out := make([]byte, 0, len(raw)+len(replacement))
			out = append(out, raw[:i]...)
			out = append(out, replacement...)
			out = append(out, raw[i+len(empty):]...)
			return out, nil
		}
	}
	closing := bytes.LastIndex(raw, []byte("</Config>"))
	if closing < 0 {
		return nil, fmt.Errorf("config.xml: has no <Config> root")
	}
	at := closing
	for at > 0 && (raw[at-1] == ' ' || raw[at-1] == '\t' || raw[at-1] == '\n' || raw[at-1] == '\r') {
		at--
	}
	insert := "<" + name + ">" + value + "</" + name + ">"
	if bytes.Contains(raw, []byte("\n")) {
		insert = "\n" + childIndent(raw) + insert
	}
	out := make([]byte, 0, len(raw)+len(insert))
	out = append(out, raw[:at]...)
	out = append(out, insert...)
	out = append(out, raw[at:]...)
	return out, nil
}

// childIndent is the file's own element indentation; no newline means none.
func childIndent(raw []byte) string {
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 {
		return ""
	}
	rest := raw[nl+1:]
	n := 0
	for n < len(rest) && (rest[n] == ' ' || rest[n] == '\t') {
		n++
	}
	return string(rest[:n])
}

// xmlEscape and xmlUnescape cover what the app's own writer emit and read: the
// three characters that can appear in a path.
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

var xmlUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

func xmlEscape(s string) string   { return xmlEscaper.Replace(s) }
func xmlUnescape(s string) string { return xmlUnescaper.Replace(s) }
