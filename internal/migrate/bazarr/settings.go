package bazarr

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// The two Arrs a Bazarr instance reads. The constants are the app's own section
// names, which its API spells the same way.
const (
	arrSonarr = "sonarr"
	arrRadarr = "radarr"
)

// sides are the two sections, and the switch in general that turns each on
// (config.py:176-240 at v1.6.2: the sonarr.* and radarr.* validators and
// general.use_sonarr / general.use_radarr).
var sides = []struct{ kind, useKey string }{
	{arrSonarr, "use_sonarr"},
	{arrRadarr, "use_radarr"},
}

// pathMappingKeys are the two path-mapping lists, each with the Arr whose paths
// its `from` side names: series come from Sonarr, films from Radarr. A pair is
// [from, to], where `from` is the path the Arr reports and `to` the path this
// app sees (bazarr/app/config.py:95-96 and utilities/path_mappings.py's
// path_replace). The two sides are therefore two different apps' paths, and
// each is rewritten through its own app's table.
var pathMappingKeys = []struct{ key, arr string }{
	{"path_mappings", arrSonarr},
	{"path_mappings_movie", arrRadarr},
}

// settings reads config/config.yaml and rewrites the Arr addresses and the path
// mappings.
func (a *analysis) settings() error {
	path := filepath.Join(a.in.Dir, settingsFile)
	a.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			a.notes = append(a.notes, settingsFile+" is not in the staged config; nothing to rewrite in it")
			return nil
		}
		return fmt.Errorf("bazarr: %w", err)
	}
	a.doc = parseSettings(data)
	for _, s := range sides {
		if err := a.side(s.kind, s.useKey); err != nil {
			return err
		}
	}
	for _, m := range pathMappingKeys {
		if err := a.mappings(m.key, m.arr); err != nil {
			return err
		}
	}
	return nil
}

// side rewrites one Arr's address. A side the source switched off is left
// exactly as it is and said so: nothing dials it, so an address that no longer
// resolves is not a gap.
func (a *analysis) side(section, useKey string) error {
	host, haveHost := a.doc.scalar(section, "ip")
	if !haveHost || host == "" {
		return nil
	}
	where := section + ".ip"
	if on, _ := a.doc.scalar("general", useKey); !isTrue(on) {
		a.left = append(a.left, rewrite.Left{
			File: settingsFile, Where: where, What: host,
			Why:    "switched off, so nothing dials it",
			Detail: fmt.Sprintf("%s is on %s while general.%s says this side is off: the %s address is left as the source stored it.", section, host, useKey, section),
		})
		return nil
	}
	port := 0
	if raw, ok := a.doc.scalar(section, "port"); ok {
		if p, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			port = p
		}
	}
	base := "/"
	if raw, ok := a.doc.scalar(section, "base_url"); ok && raw != "" {
		base = raw
	}
	scheme := "http"
	if ssl, _ := a.doc.scalar(section, "ssl"); isTrue(ssl) {
		scheme = "https"
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if base != "/" {
		u.Path = strings.TrimRight(base, "/")
	}
	res := a.in.ResolveURL(u)
	switch res.Verdict {
	case plan.Rewrite:
		target, ok := a.in.Targets[res.App]
		if !ok {
			return fmt.Errorf("%s: the plan points %s at %s, which this box does not run", settingsFile, where, res.App)
		}
		a.setScalar(section, "ip", target.Host, true, where)
		a.setScalar(section, "port", strconv.Itoa(target.Port), false, section+".port")
		baseURL := target.URLBase
		if baseURL == "" {
			baseURL = "/"
		}
		a.setScalar(section, "base_url", baseURL, true, section+".base_url")
		// Every address inside the release is plain HTTP: the Gateway terminates
		// TLS, and a pod talking to another pod's Service would need a
		// certificate for a name nothing serves one for.
		a.setScalar(section, "ssl", "false", false, section+".ssl")
	case plan.Leave:
		// The plan says this address points outside the source, and the operator
		// owns that decision. Nothing to say.
	default:
		a.unclass = append(a.unclass, rewrite.Address{
			App: a.app, File: settingsFile, Where: where, Host: host, Port: port,
		})
	}
	return nil
}

// mappings rewrites one path-mapping list: the Arr's side through that Arr's
// table, this app's side through its own. A pair whose two sides now name the
// same path is dropped, because on the box the Arrs and this app both see
// /data, which is the one thing a mapping exists to bridge -- and a mapping
// left in place would map every path it now matches.
//
// The Arr's side is settled only when this box runs exactly one of it: with two
// Sonarrs the pair cannot say which one's path it names, so it is left and said
// so rather than rewritten at a guess.
func (a *analysis) mappings(key, arr string) error {
	pairs, present, err := a.doc.pairs("general", key)
	if err != nil {
		return fmt.Errorf("%s: general.%s: %w", settingsFile, key, err)
	}
	if !present || len(pairs) == 0 {
		return nil
	}
	where := "general." + key
	fromApp, sole := a.in.Sole(arr)
	if !sole {
		for _, pair := range pairs {
			a.left = append(a.left, rewrite.Left{
				File: settingsFile, Where: where, What: pair[0] + " -> " + pair[1],
				Why:    "this box does not run exactly one " + arr + ", so the mapping cannot say whose path it is",
				Detail: fmt.Sprintf("general.%s maps an Arr's path onto this app's. box runs %d %s instance(s), so it left the pair as the source wrote it; check it in the app if this Bazarr is wired to one of them.", key, len(a.in.Runs[arr]), arr),
			})
		}
		return nil
	}
	kept := make([][2]string, 0, len(pairs))
	changed := false
	for i, pair := range pairs {
		//nolint:gosec // the index is this loop's own count
		from, fromOK := a.in.Plan.Rewrite(fromApp, pair[0])
		if !fromOK {
			a.gap(fromApp, settingsFile, fmt.Sprintf("%s[%d][0]", where, i), pair[0])
			kept = append(kept, pair)
			continue
		}
		to, toOK := a.in.Plan.Rewrite(a.app, pair[1])
		if !toOK {
			a.gap(a.app, settingsFile, fmt.Sprintf("%s[%d][1]", where, i), pair[1])
			kept = append(kept, pair)
			continue
		}
		if from == to {
			a.add(settingsFile, where, "dropped")
			a.notes = append(a.notes, fmt.Sprintf(
				"%s pair %q -> %q: both sides now read %s, so it was dropped; one filesystem needs no mapping",
				where, pair[0], pair[1], from))
			changed = true
			continue
		}
		if from != pair[0] || to != pair[1] {
			a.add(settingsFile, where, "path")
			changed = true
		}
		kept = append(kept, [2]string{from, to})
	}
	if changed {
		a.doc.setPairs("general", key, kept)
		a.settingsChanged = true
	}
	return nil
}

// setScalar writes one scalar and counts it, when it differs from what the
// document holds. quoted is what keeps a path a string and a port a number:
// Bazarr's validators are typed (config.py), and a quoted port fails
// validation and is reset to the app's default.
func (a *analysis) setScalar(section, key, value string, quoted bool, where string) {
	current, ok := a.doc.scalar(section, key)
	if ok && current == value {
		return
	}
	a.doc.setScalar(section, key, value, quoted)
	a.settingsChanged = true
	a.add(settingsFile, where, "address")
}

// applySettings writes the settings file, if anything changed.
func (a *analysis) applySettings() error {
	if a.doc == nil || !a.settingsChanged {
		return nil
	}
	return atomicfile.Write(a.path, a.doc.bytes(), 0o600)
}

// isNoSuchTable reports SQLite's "no such table" for a source version that
// predates one of the tables.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// isTrue reads a YAML boolean the way Dynaconf does for the app's own switches.
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// settingsDoc is config/config.yaml as lines. Only the keys this package owns
// are ever looked at or replaced: every other line is the app's own and is
// written back exactly as it was, and the app rewrites the whole file in its
// own formatting on its first start anyway.
type settingsDoc struct {
	lines []string
	tail  bool // the file ended with a newline
}

func parseSettings(data []byte) *settingsDoc {
	doc := &settingsDoc{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	doc.lines = strings.Split(text, "\n")
	if doc.lines[len(doc.lines)-1] == "" {
		doc.lines = doc.lines[:len(doc.lines)-1]
		doc.tail = true
	}
	return doc
}

func (d *settingsDoc) bytes() []byte {
	out := strings.Join(d.lines, "\n")
	if d.tail {
		out += "\n"
	}
	return []byte(out)
}

// topLevel reports whether a line starts a top-level mapping key, and its name.
func topLevel(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
		return "", false
	}
	name, _, ok := strings.Cut(line, ":")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(name), true
}

// section is one top-level block: the index of its key line and the exclusive
// end of the block.
func (d *settingsDoc) section(name string) (start, end int, ok bool) {
	for i, line := range d.lines {
		if got, isKey := topLevel(line); !isKey || got != name {
			continue
		}
		end := len(d.lines)
		for j := i + 1; j < len(d.lines); j++ {
			if _, isKey := topLevel(d.lines[j]); isKey {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

// childIndent is the indentation a section's keys are written at: the first
// non-empty line under it, or two spaces for an empty one.
func (d *settingsDoc) childIndent(start, end int) string {
	for _, line := range d.lines[start+1 : end] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	return "  "
}

// keyIndex finds one key inside a section.
func (d *settingsDoc) keyIndex(section, key string) (index int, ok bool) {
	start, end, ok := d.section(section)
	if !ok {
		return 0, false
	}
	indent := d.childIndent(start, end)
	for i := start + 1; i < end; i++ {
		line := d.lines[i]
		if !strings.HasPrefix(line, indent) {
			continue
		}
		k, _, found := strings.Cut(strings.TrimPrefix(line, indent), ":")
		if found && strings.TrimSpace(k) == key {
			return i, true
		}
	}
	return 0, false
}

// scalar reads one key's value, unquoted.
func (d *settingsDoc) scalar(section, key string) (string, bool) {
	i, ok := d.keyIndex(section, key)
	if !ok {
		return "", false
	}
	_, raw, _ := strings.Cut(d.lines[i], ":")
	return unquoteScalar(raw), true
}

// setScalar replaces one key's value, keeping its indentation and position.
func (d *settingsDoc) setScalar(section, key, value string, quoted bool) {
	i, ok := d.keyIndex(section, key)
	if !ok {
		// The section exists but not the key, or neither: append the key to the
		// section (or the section to the file). Bazarr itself rewrites this file
		// on the next start, so where a key sits is a detail of this one write.
		line := key + ": " + scalarText(value, quoted)
		if s, e, has := d.section(section); has {
			indent := d.childIndent(s, e)
			d.lines = append(d.lines[:e], append([]string{indent + line}, d.lines[e:]...)...)
		} else {
			d.lines = append(d.lines, section+":", "  "+line)
		}
		return
	}
	indent := d.lines[i][:len(d.lines[i])-len(strings.TrimLeft(d.lines[i], " \t"))]
	d.lines[i] = indent + key + ": " + scalarText(value, quoted)
}

// pairs reads a list of [from, to] pairs. present is false when the key is
// absent; an empty `[]` is present and empty.
func (d *settingsDoc) pairs(section, key string) (pairs [][2]string, present bool, err error) {
	i, ok := d.keyIndex(section, key)
	if !ok {
		return nil, false, nil
	}
	_, raw, _ := strings.Cut(d.lines[i], ":")
	raw = strings.TrimSpace(raw)
	if raw == "[]" {
		return nil, true, nil
	}
	if raw != "" {
		return nil, false, fmt.Errorf("the list is written inline (%q); box can only edit a list written one item per line", raw)
	}
	indent := d.lines[i][:len(d.lines[i])-len(strings.TrimLeft(d.lines[i], " \t"))]
	var flat []string
	for _, line := range d.lines[i+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			break
		}
		if !strings.HasPrefix(line, indent) {
			break
		}
		value := trimmed
		for strings.HasPrefix(value, "- ") {
			value = strings.TrimPrefix(value, "- ")
		}
		flat = append(flat, unquoteScalar(value))
	}
	if len(flat)%2 != 0 {
		return nil, false, fmt.Errorf("holds %d values, which is not a list of pairs", len(flat))
	}
	for i := 0; i < len(flat); i += 2 {
		pairs = append(pairs, [2]string{flat[i], flat[i+1]})
	}
	return pairs, true, nil
}

// setPairs replaces one list with the given pairs, in a shape Dynaconf reads as
// a list of lists.
func (d *settingsDoc) setPairs(section, key string, pairs [][2]string) {
	i, ok := d.keyIndex(section, key)
	if !ok {
		return
	}
	indent := d.lines[i][:len(d.lines[i])-len(strings.TrimLeft(d.lines[i], " \t"))]
	end := i + 1
	for _, line := range d.lines[i+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "- ") || !strings.HasPrefix(line, indent) {
			break
		}
		end++
	}
	var block []string
	if len(pairs) == 0 {
		block = []string{indent + key + ": []"}
	} else {
		block = append(block, indent+key+":")
		for _, p := range pairs {
			block = append(block, indent+"- - "+scalarText(p[0], true), indent+"  - "+scalarText(p[1], true))
		}
	}
	d.lines = append(d.lines[:i], append(block, d.lines[end:]...)...)
}

// scalarText writes a value as a YAML scalar: a string is quoted so a path that
// starts with a symbol YAML reads as a comment or a tag stays a path, and a
// number is left bare because the app's validators are typed.
func scalarText(value string, quoted bool) string {
	if !quoted {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(value[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// unquoteScalar reads a YAML scalar the two shapes this file uses can take: a
// double- or single-quoted string, or a plain one with an optional comment.
func unquoteScalar(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '"':
		if end := strings.LastIndexByte(v, '"'); end > 0 {
			inner := v[1:end]
			var b strings.Builder
			for i := 0; i < len(inner); i++ {
				if inner[i] == '\\' && i+1 < len(inner) {
					i++
					switch inner[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(inner[i])
					}
					continue
				}
				b.WriteByte(inner[i])
			}
			return b.String()
		}
	case '\'':
		if end := strings.LastIndexByte(v, '\''); end > 0 {
			return strings.ReplaceAll(v[1:end], "''", "'")
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	if v == "~" || v == "null" {
		return ""
	}
	return strings.TrimSpace(v)
}
