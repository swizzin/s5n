package seedbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Chart is one chart release's own defaults: the values.yaml and
// values.schema.json that ship inside it. box reads both out of the release it
// is installing, so this package never hard-codes a repository path — a box
// tracks the release, not the git checkout box was built from.
type Chart struct {
	Values []byte
	Schema []byte
}

// Base is the chart's own values with every app switched off: the document a
// seedbox starts from before either file is read.
//
// Starting from the chart's defaults is not a shortcut. The schema requires
// `image` and `port` on every app object, so a "disabled" app can only be
// written as a complete object, and the only complete objects are the chart's
// own. Starting from them is also what makes the grant authoritative over which
// apps exist: Helm would otherwise take the chart default `enabled: true` for
// any app the grant never mentions, and a new app in a new release would appear
// on every box without root deciding it.
//
// `box init` reads it directly for a host that has no files yet: the questions
// are prefilled from what the chart would render, which is the same document an
// apply of those files would produce.
func Base(c Chart) (map[string]any, error) {
	base, err := parseChartValues(c.Values)
	if err != nil {
		return nil, err
	}
	apps, ok := base["apps"].(map[string]any)
	if !ok {
		return nil, errors.New("chart values: no apps section; the chart cannot describe a seedbox")
	}
	for _, name := range sortedKeys(apps) {
		if entry, ok := apps[name].(map[string]any); ok {
			entry["enabled"] = false
		}
	}
	return base, nil
}

// Effective returns the values the chart would actually render for this
// seedbox: Base, then the user's values, then the grant.
func (s *Seedbox) Effective(c Chart) (map[string]any, error) {
	base, err := Base(c)
	if err != nil {
		return nil, err
	}
	merged, err := s.Merge()
	if err != nil {
		return nil, err
	}
	vols, err := s.Storage()
	if err != nil {
		return nil, err
	}
	overlay(base, merged, nil)
	// auth.edge is box's: the chart ships no login and refuses to render
	// without the statement that one is in front, and box is what puts it
	// there. It is written here, not in Merge, because only the release being
	// installed can say whether the key exists at all: a box keeps reading
	// every release back to the oldest upgrade-from it knows, and 0.2.0's
	// schema has no auth section, so writing it blindly would refuse a
	// perfectly good install with "auth: is not a key the chart knows".
	// Overlaid after both files so no file can shadow it; a file that tries
	// (true or false) is already refused by checkValues and checkGrant.
	declares, err := declaresAuthEdge(c.Schema)
	if err != nil {
		return nil, err
	}
	if declares {
		overlay(base, map[string]any{"auth": map[string]any{"edge": true}}, nil)
	}
	// The Gateway's proxy address, when the box's grant has one and the chart
	// declares the key. It is BoxOnly, which overlay() skips by design -- the
	// grant is box's record and never a chart value -- so it is injected here
	// instead, the same way auth.edge is: after both files, so no file can
	// shadow it, and only when the schema has the key, because a chart from
	// before the provider would refuse the install.
	declaresIP, err := declaresOIDCGatewayIP(c.Schema)
	if err != nil {
		return nil, err
	}
	if declaresIP {
		if ip, ok := s.Grant.data["auth"].(map[string]any); ok {
			if oidc, ok := ip["oidc"].(map[string]any); ok {
				if addr, ok := oidc["gatewayIP"].(string); ok && strings.TrimSpace(addr) != "" {
					setPath(base, []string{"auth", "oidc", "gatewayIP"}, addr)
				}
			}
		}
	}
	// The claim names are box's, from the volumes it creates: not the chart's
	// defaults and not either file. Applied last so nothing can shadow them.
	overlay(base, claims(vols), nil)
	if err := Validate(c.Schema, base); err != nil {
		return nil, err
	}
	return base, nil
}

// declaresAuthEdge reports whether the chart's schema describes auth.edge.
// Presence is the whole question: a schema that never mentions the key has a
// release that would reject it, so box leaves it out rather than refusing the
// install. An empty schema answers false; Validate refuses that document after
// this with the sentence about a schema nothing has checked, and a schema that
// does not parse is reported here in the same words Validate uses.
func declaresAuthEdge(schema []byte) (bool, error) {
	if len(schema) == 0 {
		return false, nil
	}
	var doc struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return false, fmt.Errorf("chart schema: %w", err)
	}
	auth, ok := doc.Properties["auth"]
	if !ok {
		return false, nil
	}
	_, ok = auth.Properties["edge"]
	return ok, nil
}

// declaresOIDCGatewayIP reports whether the chart's schema describes
// auth.oidc.gatewayIP. Presence is the whole question, exactly as for auth.edge:
// a chart from before the provider has no such key, and a values document that
// carried it would be refused by that chart's own schema.
// DeclaresOIDCGatewayIP reports whether a chart's schema describes the key, for
// a caller outside this package (init writes the grant key only when it does).
func DeclaresOIDCGatewayIP(schema []byte) (bool, error) { return declaresOIDCGatewayIP(schema) }

func declaresOIDCGatewayIP(schema []byte) (bool, error) {
	if len(schema) == 0 {
		return false, nil
	}
	var doc struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return false, fmt.Errorf("chart schema: %w", err)
	}
	auth, ok := doc.Properties["auth"]
	if !ok {
		return false, nil
	}
	oidc, ok := auth.Properties["oidc"]
	if !ok {
		return false, nil
	}
	_, ok = oidc.Properties["gatewayIP"]
	return ok, nil
}

// setPath writes one value into the document at the given key path, creating
// mappings as it goes. It bypasses overlay() deliberately: the keys it writes
// are box's, and overlay() is the function that keeps box's keys out.
func setPath(root map[string]any, keys []string, value any) {
	cur := root
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = value
			return
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
}

// parseChartValues reads a release's values.yaml into the same shape as a user
// document, so one merge and one validation cover both.
func parseChartValues(src []byte) (map[string]any, error) {
	d, err := parseDoc("chart values", src)
	if err != nil {
		return nil, err
	}
	if d.root == nil {
		return nil, errors.New("chart values: empty")
	}
	return d.data, nil
}

// Problem is one schema violation, named the way a user would find it in their
// file: by key path, not by JSON pointer.
type Problem struct {
	Path    string
	Message string
}

// SchemaError lists every violation in one pass, so a user fixes their file
// once instead of one key per apply.
type SchemaError struct {
	Problems []Problem
}

func (e *SchemaError) Error() string {
	var b strings.Builder
	b.WriteString("the seedbox does not match the chart's schema:")
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s: %s", p.Path, p.Message)
	}
	return b.String()
}

// schemaURL is the name the chart schema is compiled under. It is a bare
// synthetic URL, not a path: the compiler resolves relative references against
// it and never has a reason to touch the filesystem for it.
const schemaURL = "https://chart.s5n.invalid/values.schema.json"

// noRefLoader refuses every URL the compiler would otherwise load. See Validate.
type noRefLoader struct{}

func (noRefLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is outside the chart schema; refusing to load it", url)
}

// Validate checks a complete values document against a chart release's schema.
// The schema is a parameter, not a file this package reads: the schema that
// matters is the one in the release being installed.
func Validate(schema []byte, values map[string]any) error {
	if len(schema) == 0 {
		return errors.New("no chart schema: refusing to apply values nothing has checked")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return fmt.Errorf("chart schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	// The schema arrives inside a chart and box runs as root, so a $ref must
	// never reach outside the schema itself. v6 loads meta-schemas from an
	// embedded copy and sends everything else to this loader, which opens
	// nothing.
	c.UseLoader(noRefLoader{})
	if err := c.AddResource(schemaURL, doc); err != nil {
		return fmt.Errorf("chart schema: %w", err)
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("chart schema: %w", err)
	}
	if err := sch.Validate(values); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return &SchemaError{Problems: problemsOf(ve)}
		}
		return err
	}
	return nil
}

var printer = message.NewPrinter(language.English)

// problemsOf flattens the validator's error tree to one entry per offending
// key. Branch errors ("doesn't validate with ...") are dropped: they name a
// schema location, not something a user can act on.
func problemsOf(ve *jsonschema.ValidationError) []Problem {
	var out []Problem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		out = append(out, problemsFor(e)...)
	}
	walk(ve)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Message < out[j].Message
	})
	deduped := out[:0]
	for i, p := range out {
		if i > 0 && p == out[i-1] {
			continue
		}
		deduped = append(deduped, p)
	}
	return deduped
}

func problemsFor(e *jsonschema.ValidationError) []Problem {
	base := tokenPath(e.InstanceLocation)
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		var out []Problem
		for _, name := range k.Missing {
			out = append(out, Problem{Path: joinPath(base, name), Message: "is required"})
		}
		return out
	case *kind.AdditionalProperties:
		var out []Problem
		for _, name := range k.Properties {
			out = append(out, Problem{Path: joinPath(base, name), Message: "is not a key the chart knows"})
		}
		return out
	case *kind.Not:
		return []Problem{{Path: base, Message: "is not an allowed value"}}
	case *kind.Type:
		// The library words this "got string, want integer"; the user's file
		// reads better the other way round, and the existing errors keep their
		// wording across the v5 -> v6 move.
		return []Problem{{Path: base, Message: fmt.Sprintf("expected %s, got %s", strings.Join(k.Want, " or "), k.Got)}}
	}
	return []Problem{{Path: base, Message: e.ErrorKind.LocalizedString(printer)}}
}

// tokenPath turns the validator's instance tokens (apps, sonarr, instances,
// 1, enabled) into apps.sonarr.instances[1].enabled.
func tokenPath(tokens []string) string {
	if len(tokens) == 0 {
		return "(top level)"
	}
	var b strings.Builder
	for i, part := range tokens {
		if isIndex(part) {
			fmt.Fprintf(&b, "[%s]", part)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func joinPath(base, name string) string {
	if base == "" || base == "(top level)" {
		return name
	}
	return base + "." + name
}
