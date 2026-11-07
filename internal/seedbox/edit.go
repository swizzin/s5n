package seedbox

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Editor changes one key at a time and writes the file back atomically. It uses
// the parser only to find where the key lives; the bytes are spliced, never
// re-rendered. That is what keeps a user's comments, blank lines, quoting, key
// order and odd indentation through a box command, and it is why box can
// promise it never rewrites a file it did not have to.
//
// What it will not do: replace a block (a mapping or a list that already exists),
// edit inside a flow-style collection, or write a value that needs more than one
// line in place of one that does not. Those cases are refused with a message
// that says to edit the file by hand, because silently reformatting the
// surrounding document is the failure this type exists to prevent.
type Editor struct {
	doc *Doc
	src []byte
	// rename is os.Rename in production. Tests replace it to prove that a failed
	// write leaves the original file exactly as it was.
	rename func(oldpath, newpath string) error
}

// Editor returns an editor over the document. The document is not changed until
// Save succeeds.
func (d *Doc) Editor() *Editor {
	return &Editor{doc: d, src: d.Src, rename: os.Rename}
}

// SetGrant sets one key in the grant and saves it. Only grant keys may be
// written: root's file decides isolation, and a key that tunes an app belongs
// in the user's values file.
func (s *Seedbox) SetGrant(path string, value any) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	if !grantOwned(p.Keys()) {
		return fmt.Errorf("%s is not a grant key; it belongs in the user's values file", p)
	}
	return s.edit(s.Grant, p, value)
}

// SetValue sets one key in the user's values file and saves it. A grant key is
// refused: the grant always wins, so writing it here would be a change that
// silently does nothing.
func (s *Seedbox) SetValue(path string, value any) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	if rule, ok := GrantRule(p.Keys()); ok {
		return fmt.Errorf("%s is set by the grant (%s); change it there, as root", p, rule.Why)
	}
	return s.edit(s.Values, p, value)
}

// DeleteGrant removes one key from the grant.
func (s *Seedbox) DeleteGrant(path string) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	return s.delete(s.Grant, p)
}

// DeleteValue removes one key from the values file.
func (s *Seedbox) DeleteValue(path string) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	return s.delete(s.Values, p)
}

func grantOwned(keys []string) bool {
	_, ok := GrantRule(keys)
	return ok
}

func (s *Seedbox) edit(d *Doc, p Path, value any) error {
	e := d.Editor()
	if err := e.Set(p.String(), value); err != nil {
		return err
	}
	return e.Save()
}

func (s *Seedbox) delete(d *Doc, p Path) error {
	e := d.Editor()
	if err := e.Delete(p.String()); err != nil {
		return err
	}
	return e.Save()
}

// Set sets one key and leaves the result in the editor. Nothing is written until
// Save.
func (e *Editor) Set(path string, value any) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	backup := e.src
	if err := e.set(p, value); err != nil {
		e.src = backup
		return err
	}
	if err := e.verify(p, value); err != nil {
		e.src = backup
		return err
	}
	return nil
}

// Delete removes one key (or one list entry) and leaves the result in the
// editor. A delete that would leave its parent empty is refused: an empty
// mapping is a null value, and the caller meant to remove the parent.
func (e *Editor) Delete(path string) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	backup := e.src
	if err := e.delete(p); err != nil {
		e.src = backup
		return err
	}
	if err := e.verifyGone(p); err != nil {
		e.src = backup
		return err
	}
	return nil
}

// Bytes is the document as it stands, edits applied.
func (e *Editor) Bytes() []byte { return append([]byte(nil), e.src...) }

// Changed reports whether the editor holds any edit.
func (e *Editor) Changed() bool { return !bytes.Equal(e.src, e.doc.Src) }

// Save writes the document back, atomically, keeping its permission bits. A
// document with no edit is not rewritten, so its mtime still means "the user
// changed this".
func (e *Editor) Save() error {
	if !e.Changed() {
		return nil
	}
	if e.doc.Path == "" {
		return errors.New("cannot save: the document has no file")
	}
	if err := writeAtomic(e.doc.Path, e.src, e.doc.mode, e.rename); err != nil {
		return err
	}
	d, err := parseDoc(e.doc.Path, e.src)
	if err != nil {
		return err
	}
	d.mode = e.doc.mode
	*e.doc = *d
	return nil
}

func (e *Editor) set(p Path, value any) error {
	if len(p) == 0 {
		return errors.New("empty path")
	}
	if err := checkLineEndings(e.src); err != nil {
		return err
	}
	cur, parent, key, depth := resolve(e.doc.root, p)

	if depth == len(p) {
		return e.replace(p, cur, parent, value)
	}

	rest := p[depth:]
	if cur == nil {
		return e.appendAtRoot(rest, value)
	}
	switch cur.Kind {
	case yaml.MappingNode:
		if rest[0].IsIndex {
			return fmt.Errorf("%s is a mapping, not a list", Path(p[:depth]))
		}
		return e.appendToMapping(p, cur, rest, value)
	case yaml.ScalarNode:
		if isBareNull(cur) && !rest[0].IsIndex {
			return e.appendUnderKey(p, key, rest, value)
		}
		return fmt.Errorf("%s is the value %q, not a mapping; edit the file by hand to turn it into one", Path(p[:depth]), cur.Value)
	case yaml.SequenceNode:
		if !rest[0].IsIndex {
			return fmt.Errorf("%s is a list", Path(p[:depth]))
		}
		if rest[0].Index != len(cur.Content) {
			return fmt.Errorf("%s has %d entries; box can change an existing entry or add the next one, nothing in between", Path(p[:depth]), len(cur.Content))
		}
		if len(rest) > 1 {
			return fmt.Errorf("add the entry to %s first, then set %s", Path(p[:depth]), p)
		}
		return e.appendToSequence(Path(p[:depth]), cur, value)
	}
	return fmt.Errorf("cannot set %s", p)
}

// replace swaps one scalar in place. Everything else on the line — the key, the
// spacing before the value, a trailing comment — stays byte for byte.
func (e *Editor) replace(p Path, n, parent *yaml.Node, value any) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("%s is a %s; box changes one value at a time and will not rewrite it — edit the file by hand", p, kindName(n))
	}
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return fmt.Errorf("%s is a multi-line block of text; edit it by hand", p)
	}
	line := e.line(n.Line)
	start := runeOffset(line, n.Column)
	if start < 0 || start >= len(line) {
		return fmt.Errorf("%s: cannot find the value on line %d", p, n.Line)
	}
	inFlow := parent != nil && parent.Style&yaml.FlowStyle != 0
	end := tokenEnd(line[start:], inFlow)
	if end <= 0 {
		return fmt.Errorf("%s: cannot read the value on line %d; edit the file by hand", p, n.Line)
	}
	end = start + end
	if err := sameToken(line[start:end], n); err != nil {
		return fmt.Errorf("%s: %v; edit the file by hand", p, err)
	}
	text, err := encodeStyled(value, n.Style)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	e.src = splice(e.src, e.lineStart(n.Line)+start, e.lineStart(n.Line)+end, []byte(text))
	return nil
}

// appendToMapping adds the rest of the path at the end of a block mapping, so a
// new key lands after the ones already there and before any trailing comment.
func (e *Editor) appendToMapping(p Path, m *yaml.Node, rest Path, value any) error {
	if m.Style&yaml.FlowStyle != 0 {
		return fmt.Errorf("%s is written on one line; write it out over several lines before box can add to it", Path(p[:len(p)-len(rest)]))
	}
	if len(m.Content) < 2 {
		return fmt.Errorf("%s has no keys to follow; edit the file by hand", Path(p[:len(p)-len(rest)]))
	}
	lastKey := m.Content[len(m.Content)-2]
	indent := lastKey.Column - 1
	lines, err := chainLines(rest, value, indent, e.indentStep())
	if err != nil {
		return err
	}
	return e.insertAfter(e.nodeEnd(m.Content[len(m.Content)-1]), lines)
}

// appendUnderKey turns "key:" (with nothing under it) into a mapping header and
// writes the new keys beneath it.
func (e *Editor) appendUnderKey(p Path, key *yaml.Node, rest Path, value any) error {
	lines, err := chainLines(rest, value, key.Column-1+e.indentStep(), e.indentStep())
	if err != nil {
		return err
	}
	return e.insertAfter(key.Line, lines)
}

func (e *Editor) appendToSequence(p Path, seq *yaml.Node, value any) error {
	if seq.Style&yaml.FlowStyle != 0 {
		return fmt.Errorf("%s is written on one line; write it out over several lines before box can add to it", p)
	}
	indent := e.sequenceIndent(seq)
	text, err := encodeValue(value)
	if err != nil {
		return err
	}
	parts := strings.Split(text, "\n")
	lines := make([]string, 0, len(parts))
	pad := strings.Repeat(" ", indent)
	lines = append(lines, pad+"- "+parts[0])
	for _, l := range parts[1:] {
		lines = append(lines, pad+"  "+l)
	}
	return e.insertAfter(e.nodeEnd(seq), lines)
}

// appendAtRoot handles a document with no keys at all, which is what a box that
// has never been tuned has. Comments already in the file are left above.
func (e *Editor) appendAtRoot(rest Path, value any) error {
	lines, err := chainLines(rest, value, 0, 2)
	if err != nil {
		return err
	}
	if len(e.src) == 0 {
		e.src = []byte(strings.Join(lines, "\n") + "\n")
		return nil
	}
	return e.insertAfter(len(lineStarts(e.src))-1, lines)
}

// sequenceIndent is the column of the "- " that introduces an entry, found in
// the source line: yaml reports the position of the entry's content, not of the
// dash, and the dash is what a new entry has to line up with.
func (e *Editor) sequenceIndent(seq *yaml.Node) int {
	if len(seq.Content) == 0 {
		return seq.Column - 1
	}
	first := seq.Content[0]
	line := string(e.line(first.Line))
	off := runeOffset([]byte(line), first.Column)
	if off > len(line) {
		off = len(line)
	}
	if i := strings.LastIndex(line[:off], "-"); i >= 0 {
		return i
	}
	return seq.Column - 1
}

func (e *Editor) delete(p Path) error {
	if err := checkLineEndings(e.src); err != nil {
		return err
	}
	cur, parent, key, depth := resolve(e.doc.root, p)
	if depth != len(p) {
		return fmt.Errorf("%s is not set", p)
	}
	if parent == nil || key == nil && parent.Kind != yaml.SequenceNode {
		return fmt.Errorf("cannot remove %s: it is the whole file", p)
	}
	if parent.Kind == yaml.MappingNode && len(parent.Content) <= 2 {
		return fmt.Errorf("cannot remove %s: it is the only key left under %s, which would leave it empty", p, Path(p[:len(p)-1]))
	}
	if parent.Kind == yaml.SequenceNode && len(parent.Content) <= 1 {
		return fmt.Errorf("cannot remove %s: it is the only entry in %s", p, Path(p[:len(p)-1]))
	}

	from := cur.Line
	indent := 0
	if key != nil {
		from = key.Line
		indent = key.Column - 1
	}
	from = e.commentStart(from, indent)
	to := e.nodeEnd(cur)

	starts := lineStarts(e.src)
	fromOff := starts[from-1]
	toOff := e.lineEnd(to)
	e.src = splice(e.src, fromOff, toOff, nil)
	return nil
}

// commentStart walks up over the comment block attached to a key, so deleting a
// key does not leave its explanation behind.
func (e *Editor) commentStart(from, indent int) int {
	for from > 1 {
		prev := e.line(from - 1)
		trimmed := strings.TrimLeft(string(prev), " \t")
		if strings.TrimSpace(trimmed) == "" {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			break
		}
		if len(prev)-len(trimmed) < indent {
			break // the comment belongs to an outer key
		}
		from--
	}
	return from
}

func (e *Editor) insertAfter(line int, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	starts := lineStarts(e.src)
	if line < 1 || line > len(starts)-1 {
		return fmt.Errorf("line %d is outside the file", line)
	}
	off := starts[line]
	text := strings.Join(lines, "\n") + "\n"
	if off > 0 && e.src[off-1] != '\n' {
		text = "\n" + text // the file did not end with a newline
	}
	e.src = splice(e.src, off, off, []byte(text))
	return nil
}

// verify re-reads the edited bytes and proves they parse and that the path holds
// the value asked for. An edit that does not survive this never reaches the
// disk.
func (e *Editor) verify(p Path, want any) error {
	d, err := parseDoc(e.doc.Path, e.src)
	if err != nil {
		return fmt.Errorf("the edit would not parse: %v", err)
	}
	n, ok, err := d.lookup(p.String())
	if err != nil {
		return err
	}
	if !ok || n == nil {
		return fmt.Errorf("%s was not set", p)
	}
	got := any(nil)
	if err := n.Decode(&got); err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if !sameValue(got, want) {
		return fmt.Errorf("%s did not take the value", p)
	}
	return nil
}

func (e *Editor) verifyGone(p Path) error {
	d, err := parseDoc(e.doc.Path, e.src)
	if err != nil {
		return fmt.Errorf("the edit would not parse: %v", err)
	}
	if _, ok, _ := d.lookup(p.String()); ok {
		return fmt.Errorf("%s is still set", p)
	}
	return nil
}

// sameValue compares a decoded node with a value the caller supplied by putting
// the caller's value through YAML too, so int/float64 and map shapes line up the
// way they will on disk.
func sameValue(got, want any) bool {
	b, err := yaml.Marshal(want)
	if err != nil {
		return false
	}
	var round any
	if err := yaml.Unmarshal(b, &round); err != nil {
		return false
	}
	return reflect.DeepEqual(got, round)
}

// chainLines renders "rest" as nested keys ending in value.
func chainLines(rest Path, value any, indent, step int) ([]string, error) {
	var lines []string
	pad := strings.Repeat(" ", indent)
	for i, st := range rest {
		if st.IsIndex {
			return nil, fmt.Errorf("cannot add list entry [%d] under a key", st.Index)
		}
		key, err := encodeKey(st.Key)
		if err != nil {
			return nil, err
		}
		if i == len(rest)-1 {
			line, err := renderKeyLine(pad, key, value, step)
			if err != nil {
				return nil, err
			}
			return append(lines, line), nil
		}
		lines = append(lines, pad+key+":")
		pad += strings.Repeat(" ", step)
	}
	return nil, errors.New("empty path")
}

func renderKeyLine(pad, key string, value any, step int) (string, error) {
	text, err := encodeValue(value)
	if err != nil {
		return "", err
	}
	if !strings.Contains(text, "\n") {
		return pad + key + ": " + text, nil
	}
	var b strings.Builder
	b.WriteString(pad + key + ":")
	inner := pad + strings.Repeat(" ", step)
	for _, l := range strings.Split(text, "\n") {
		b.WriteString("\n" + inner + l)
	}
	return b.String(), nil
}

func encodeKey(k string) (string, error) { return encodeStyled(k, 0) }

// encodeValue renders a value the way the chart file would hold it.
func encodeValue(v any) (string, error) {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return "", err
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if strings.TrimSpace(text) == "" {
		return "null", nil
	}
	return text, nil
}

// encodeStyled renders a scalar, keeping the quoting style the file already used
// when the new value is a string. A quoted "1000" therefore stays a string, and
// a plain 1000 stays a number.
func encodeStyled(v any, style yaml.Style) (string, error) {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return "", err
	}
	if _, isString := v.(string); isString {
		switch {
		case style&yaml.DoubleQuotedStyle != 0:
			n.Style = yaml.DoubleQuotedStyle
		case style&yaml.SingleQuotedStyle != 0:
			n.Style = yaml.SingleQuotedStyle
		default:
			n.Style = 0
		}
	} else {
		n.Style = 0
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if strings.Contains(text, "\n") {
		return "", errors.New("the value needs more than one line; box only replaces single values in place")
	}
	return text, nil
}

// sameToken proves the bytes found on the line are the whole value and nothing
// else: a block scalar, a multi-line plain scalar, an anchor or a flow collection
// would all fail this, and are refused rather than half-replaced.
func sameToken(token []byte, n *yaml.Node) error {
	var got any
	if err := yaml.Unmarshal(token, &got); err != nil {
		return fmt.Errorf("the value at line %d is not a simple value", n.Line)
	}
	want := any(nil)
	if err := n.Decode(&want); err != nil {
		return fmt.Errorf("the value at line %d cannot be read", n.Line)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("the value at line %d spans more than one line", n.Line)
	}
	return nil
}

// tokenEnd finds the end of a scalar token at the start of s. In a flow
// collection (on a single line), a plain scalar also ends at , ] or } — that is
// what lets box change {memory: 1Gi} without rewriting the line around it.
func tokenEnd(s []byte, inFlow bool) int {
	if len(s) == 0 {
		return 0
	}
	switch s[0] {
	case '"':
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == '"' {
				return i + 1
			}
		}
		return 0
	case '\'':
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		}
		return 0
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return trimTrailing(s[:i])
		}
		if s[i] == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			return trimTrailing(s[:i])
		}
		if inFlow && (s[i] == ',' || s[i] == ']' || s[i] == '}') {
			return trimTrailing(s[:i])
		}
	}
	return trimTrailing(s)
}

func trimTrailing(s []byte) int {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return end
}

// runeOffset converts a 1-based column, which yaml counts in characters, into a
// byte offset in the line.
func runeOffset(line []byte, column int) int {
	if column <= 1 {
		return 0
	}
	col := 1
	for i := range string(line) {
		if col == column {
			return i
		}
		col++
	}
	return len(line)
}

func (e *Editor) line(n int) []byte {
	starts := lineStarts(e.src)
	if n < 1 || n >= len(starts) {
		return nil
	}
	return e.src[starts[n-1]:starts[n]]
}

func (e *Editor) lineStart(n int) int {
	starts := lineStarts(e.src)
	if n < 1 || n > len(starts) {
		return len(e.src)
	}
	return starts[n-1]
}

// lineEnd is the offset just past line n, including its newline.
func (e *Editor) lineEnd(n int) int {
	starts := lineStarts(e.src)
	if n < 1 || n >= len(starts) {
		return len(e.src)
	}
	return starts[n]
}

// lineStarts lists the byte offset of every line start, plus one past the end.
func lineStarts(src []byte) []int {
	starts := make([]int, 1, bytes.Count(src, []byte("\n"))+2)
	starts[0] = 0
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	if starts[len(starts)-1] != len(src) {
		starts = append(starts, len(src))
	}
	return starts
}

func splice(src []byte, from, to int, repl []byte) []byte {
	out := make([]byte, 0, len(src)-(to-from)+len(repl))
	out = append(out, src[:from]...)
	out = append(out, repl...)
	out = append(out, src[to:]...)
	return out
}

// indentStep is the indentation the file already uses, so a key box adds lines up
// with its neighbours instead of imposing two spaces on a four-space file.
func (e *Editor) indentStep() int {
	step := 0
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode && len(n.Content) >= 2 {
			if child := n.Content[1]; child.Kind == yaml.MappingNode && len(child.Content) >= 2 {
				if d := child.Column - n.Column; d > 0 && (step == 0 || d < step) {
					step = d
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(e.doc.root)
	if step <= 0 || step > 8 {
		return 2
	}
	return step
}

// nodeEnd is the last source line a node occupies. Nodes carry only a start
// position, and a block or multi-line plain scalar has no child nodes for its
// lines, so the end is found by walking lines that are indented deeper than the
// scalar's own line. Without this, appending to a mapping whose last value is a
// block scalar would write inside that text.
func (e *Editor) nodeEnd(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	end := n.Line
	for _, c := range n.Content {
		if x := e.nodeEnd(c); x > end {
			end = x
		}
	}
	if n.Kind != yaml.ScalarNode {
		return end
	}
	header := e.line(n.Line)
	indent := len(header) - len(bytes.TrimLeft(header, " \t"))
	for {
		next := e.line(end + 1)
		if next == nil {
			break
		}
		text := strings.TrimRight(string(next), "\r\n")
		if strings.TrimSpace(text) == "" {
			if !e.indentedAfter(end+1, indent) {
				break
			}
			end++
			continue
		}
		if len(text)-len(strings.TrimLeft(text, " \t")) <= indent {
			break
		}
		end++
	}
	return end
}

// indentedAfter reports whether the next non-blank line is deeper than indent,
// which is what makes a blank line part of a block scalar rather than a gap
// between keys.
func (e *Editor) indentedAfter(from, indent int) bool {
	for n := from; ; n++ {
		line := e.line(n)
		if line == nil {
			return false
		}
		text := strings.TrimRight(string(line), "\r\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		return len(text)-len(strings.TrimLeft(text, " \t")) > indent
	}
}

func isBareNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null" && n.Value == ""
}

func checkLineEndings(src []byte) error {
	if bytes.Contains(src, []byte("\r\n")) {
		return errors.New("this file has Windows line endings; edit it once with a Unix editor, then box can change it")
	}
	return nil
}

// writeAtomic writes through a temporary file in the same directory and renames
// it over the target, so a crash mid-write leaves the old file whole. The mode
// is the one the file already had.
func writeAtomic(path string, data []byte, mode fs.FileMode, rename func(string, string) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, mode.Perm()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
