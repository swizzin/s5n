// Package seedbox owns the two files that define one seedbox: the root-owned
// grant in /etc/swizzin/seedboxes/<user>.yaml and the user-owned values file in
// ~<user>/.config/box/values.yaml.
//
// Why two files: a user's values file is applied with root's power, so one
// storage.data.path pointing at someone else's downloads would end the
// isolation between boxes. The split is therefore not a convenience. The grant
// holds every key that decides what a box can reach; values holds everything a
// user may tune. The grant always wins, and a grant key in values is an error
// rather than something silently ignored: "I set it and nothing happened" is
// how somebody discovers a security boundary by accident.
//
// Nothing here regenerates a file. Load reads what is on disk, Merge combines
// the two documents, and Editor changes the smallest byte range that has to
// change, so a hand edit, a comment and an odd indent all survive a box
// command. That is the same rule PLAN.md sets for migrations: change one key,
// never rewrite the file.
package seedbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// GrantDir holds one grant per user, root-owned and root-written (docs/box.md,
// "Two files per seedbox").
const GrantDir = "/etc/swizzin/seedboxes"

// GrantPath is the grant file for a user.
func GrantPath(user string) string {
	return filepath.Join(GrantDir, user+".yaml")
}

// ValuesPath is a user's values file, given that user's home directory.
func ValuesPath(home string) string {
	return filepath.Join(home, ".config", "box", "values.yaml")
}

// Doc is one parsed YAML document plus the bytes it came from. The bytes are
// kept because every edit is a splice against them, not a re-render.
type Doc struct {
	Path string // the file the document came from, for error messages
	Src  []byte // the exact bytes on disk
	// Exists is false when the file is absent. A missing values file is normal
	// (a box that has never been tuned); a missing grant is not.
	Exists bool

	root *yaml.Node
	data map[string]any
	mode fs.FileMode // permission bits to keep on write
}

// Seedbox is a user's grant and values documents, loaded from disk.
type Seedbox struct {
	Grant  *Doc
	Values *Doc
}

// Load reads a seedbox's two files. The grant must exist: it is what root
// decided. A missing values file is an empty document, because a user who has
// never run `box init` or edited anything has no file yet.
func Load(grantPath, valuesPath string) (*Seedbox, error) {
	grant, err := loadDoc(grantPath, true)
	if err != nil {
		return nil, err
	}
	values, err := loadDoc(valuesPath, false)
	if err != nil {
		return nil, err
	}
	return &Seedbox{Grant: grant, Values: values}, nil
}

func loadDoc(path string, required bool) (*Doc, error) {
	if path == "" {
		if required {
			return nil, errors.New("no grant path: a seedbox cannot exist without a grant")
		}
		return &Doc{data: map[string]any{}, mode: 0o600}, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if required {
				return nil, fmt.Errorf("seedbox grant %s does not exist: root creates it with `box adduser`", path)
			}
			return &Doc{Path: path, data: map[string]any{}, mode: 0o600}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	d, err := parseDoc(path, src)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil {
		d.mode = info.Mode().Perm()
	}
	return d, nil
}

// parseDoc parses one file into a document. It is used for the initial load and
// again after every edit, so an edit that would not parse is refused before it
// reaches the disk.
func parseDoc(path string, src []byte) (*Doc, error) {
	d := &Doc{Path: path, Src: src, Exists: true, data: map[string]any{}, mode: 0o600}
	if len(strings.TrimSpace(string(src))) == 0 {
		return d, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root == nil {
		return d, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s:%d: the file must be a mapping of keys, not a %s", path, root.Line, kindName(root))
	}
	m, err := fromNode(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d.root = root
	d.data = m.(map[string]any)
	return d, nil
}

// Map returns a copy of the document's data.
func (d *Doc) Map() map[string]any { return cloneMap(d.data) }

// fromNode converts a YAML node tree into the JSON-shaped values a chart takes.
// It is deliberately stricter than yaml.Unmarshal into interface{}: timestamps
// stay the text the author wrote (yaml would turn `2020-01-01` into a time.Time,
// which then re-marshals as a different string), and anything box cannot safely
// round-trip is an error rather than a surprise.
func fromNode(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || (k.Tag != "!!str" && k.Tag != "") {
				return nil, fmt.Errorf("line %d: mapping keys must be plain strings", k.Line)
			}
			if k.Value == "<<" {
				return nil, fmt.Errorf("line %d: YAML merge keys (<<) are not supported here; write the keys out", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: %s is set twice", k.Line, k.Value)
			}
			val, err := fromNode(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = val
		}
		return m, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalarValue(n)
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: YAML anchors and aliases are not supported here; write the value out", n.Line)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

func scalarValue(n *yaml.Node) (any, error) {
	if n.Tag == "!!timestamp" {
		// Keep the text: a schedule or a date in values is a string to the
		// chart, and re-encoding a time.Time would rewrite it.
		return n.Value, nil
	}
	if n.Tag == "!!binary" {
		return nil, fmt.Errorf("line %d: binary values are not valid chart values", n.Line)
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	switch x := v.(type) {
	case nil, string, bool, int, int64, float64:
		return x, nil
	case uint64:
		return x, nil
	case time.Time:
		return n.Value, nil
	}
	return nil, fmt.Errorf("line %d: unsupported value %T", n.Line, v)
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.ScalarNode:
		return "single value"
	}
	return "node"
}

// Step is one segment of a values path: a mapping key, or a list index.
type Step struct {
	Key     string
	Index   int
	IsIndex bool
}

// Path is a values path such as apps.sonarr.instances[1].enabled. It is the
// name box uses in messages, so a user can paste it back into their file.
type Path []Step

// ParsePath parses a dotted path with optional [n] list indices.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, errors.New("empty path")
	}
	var p Path
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return nil, fmt.Errorf("path %q has an empty segment", s)
		}
		name, idxs := part, ""
		if i := strings.IndexByte(part, '['); i >= 0 {
			name, idxs = part[:i], part[i:]
		}
		if name != "" {
			p = append(p, Step{Key: name})
		} else if idxs == "" {
			return nil, fmt.Errorf("path %q has an empty segment", s)
		}
		for idxs != "" {
			if idxs[0] != '[' {
				return nil, fmt.Errorf("path %q: expected [ after a key", s)
			}
			j := strings.IndexByte(idxs, ']')
			if j < 0 {
				return nil, fmt.Errorf("path %q: unclosed [", s)
			}
			n, err := strconv.Atoi(idxs[1:j])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("path %q: %q is not a list index", s, idxs[1:j])
			}
			p = append(p, Step{Index: n, IsIndex: true})
			idxs = idxs[j+1:]
		}
	}
	if len(p) == 0 {
		return nil, fmt.Errorf("path %q names nothing", s)
	}
	return p, nil
}

func (p Path) String() string {
	var b strings.Builder
	for i, st := range p {
		if st.IsIndex {
			fmt.Fprintf(&b, "[%d]", st.Index)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(st.Key)
	}
	return b.String()
}

// Keys is the path with list indices dropped. Grant rules are written against
// keys, because an instance the grant owns is owned whichever index it sits at.
func (p Path) Keys() []string {
	out := make([]string, 0, len(p))
	for _, st := range p {
		if !st.IsIndex {
			out = append(out, st.Key)
		}
	}
	return out
}

// resolve walks as far into the document as the path goes. depth is how many
// steps matched; a depth below len(p) means the rest of the path does not exist
// (or runs into a value of the wrong shape), which is what Set turns into an
// insertion.
func resolve(root *yaml.Node, p Path) (cur, parent, key *yaml.Node, depth int) {
	cur = root
	for i, st := range p {
		if cur == nil {
			return nil, parent, key, i
		}
		if st.IsIndex {
			if cur.Kind != yaml.SequenceNode || st.Index < 0 || st.Index >= len(cur.Content) {
				return cur, parent, key, i
			}
			parent, key = cur, nil
			cur = cur.Content[st.Index]
			continue
		}
		if cur.Kind != yaml.MappingNode {
			return cur, parent, key, i
		}
		next := (*yaml.Node)(nil)
		var keyNode *yaml.Node
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == st.Key {
				keyNode, next = cur.Content[j], cur.Content[j+1]
				break
			}
		}
		if next == nil {
			return cur, parent, key, i
		}
		parent, key, cur = cur, keyNode, next
	}
	return cur, parent, key, len(p)
}

// lookup finds a dotted path in a document. It returns the value node and
// whether it exists.
func (d *Doc) lookup(path string) (*yaml.Node, bool, error) {
	p, err := ParsePath(path)
	if err != nil {
		return nil, false, err
	}
	n, _, _, depth := resolve(d.root, p)
	if depth != len(p) || n == nil {
		return nil, false, nil
	}
	return n, true, nil
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneValue(x[i])
		}
		return out
	}
	return v
}
