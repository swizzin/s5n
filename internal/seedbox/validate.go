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
// values.schema.json that ship inside it. box reads both from the release it is
// installing, never from the git checkout box was built from.
type Chart struct {
	Values []byte
	Schema []byte
}

// Base is the chart's own values with every app switched off: the document a
// seedbox starts from before either file is read.
//
// The schema requires `image` and `port` on every app, so a disabled app can only
// be a complete object, and the complete objects are the chart's own. Starting
// from them also makes the grant authoritative over which apps exist: Helm would
// otherwise take `enabled: true` for an app the grant never mentions, and a new
// app in a new release would appear on every box without root deciding it.
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

// Pending is the values a chart would render for a box whose grant is not written
// yet: the chart's defaults with the user's values over them. It is what a flow
// can show before the box exists, and it is deliberately not checked against the
// schema: a preview must not refuse a box the install would have made.
func Pending(c Chart, valuesPath string) (map[string]any, error) {
	base, err := Base(c)
	if err != nil {
		return nil, err
	}
	values, err := LoadValues(valuesPath)
	if err != nil {
		return nil, err
	}
	overlay(base, values.data, nil)
	if _, ok := base["apps"]; !ok {
		base["apps"] = map[string]any{}
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
	// auth.edge is box's: the chart ships no login and refuses to render without
	// the statement that one is in front. It is written here, not in Merge,
	// because only the release being installed can say whether the key exists: box
	// reads every release back to the oldest upgrade-from, and 0.2.0's schema has
	// no auth section. Overlaid after both files so no file can shadow it.
	declares, err := declaresAuthEdge(c.Schema)
	if err != nil {
		return nil, err
	}
	if declares {
		overlay(base, map[string]any{"auth": map[string]any{"edge": true}}, nil)
	}
	// The Gateway proxy's address, injected rather than overlaid because it is
	// BoxOnly (the grant is box's record, never a chart value), and only when the
	// schema declares the key: a chart from before the provider would refuse it.
	declaresIP, err := DeclaresOIDCGatewayIP(c.Schema)
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
	// rehearsal and rehearsalGate are BoxOnly, so overlay() never carries them
	// from the grant; they are written here like the Gateway's address. The lock
	// fails closed: a rehearsal box rendered by a chart without it would reach the
	// internet as the live box, so that is a refusal, not a dropped key.
	if on, _ := s.Grant.data["rehearsal"].(bool); on {
		declares, err := declaresRehearsal(c.Schema)
		if err != nil {
			return nil, err
		}
		if !declares {
			return nil, errors.New("this box is a rehearsal box, and the chart being installed cannot lock it (it has no rehearsal value): a rehearsal box runs only on a chart that keeps it off the internet")
		}
		gate, _ := s.Grant.data["rehearsalGate"].(string)
		if strings.TrimSpace(gate) == "" {
			return nil, errors.New("this box is a rehearsal box and its grant has no rehearsalGate, the address each app waits on until the lock closes it: box writes it when it applies a rehearsal snapshot, so apply that snapshot again rather than run its apps ungated")
		}
		base["rehearsal"] = true
		base["rehearsalGate"] = gate
	}
	// The claim names are box's, from the volumes it creates, applied last so
	// nothing can shadow them.
	overlay(base, claims(vols), nil)
	// login is the one chart value that comes from root's account record: an app
	// that keeps its own accounts (Jellyfin) names the person's first account
	// after it. Written only where the release declares the key.
	declaresLogin, err := Declares(c.Schema, "login")
	if err != nil {
		return nil, err
	}
	if declaresLogin {
		if name, _ := s.Grant.data["login"].(string); strings.TrimSpace(name) != "" {
			setPath(base, []string{"login"}, name)
		}
	}
	// The cluster's pod network, for the app that trusts the Gateway's forwarded
	// address: BoxOnly, so it is injected here, and a chart before the app has no
	// such key.
	if declared, err := Declares(c.Schema, "apps", "jellyfin", "knownProxies"); err != nil {
		return nil, err
	} else if declared {
		if proxies := grantStringList(s.Grant.data, "apps", "jellyfin", "knownProxies"); len(proxies) > 0 {
			list := make([]any, len(proxies))
			for i, p := range proxies {
				list[i] = p
			}
			setPath(base, []string{"apps", "jellyfin", "knownProxies"}, list)
		}
	}
	if err := Validate(c.Schema, base); err != nil {
		return nil, err
	}
	return base, nil
}

// grantStringList reads a list of strings at a key path of the grant, or nil when
// the path is absent or holds anything else.
func grantStringList(data map[string]any, keys ...string) []string {
	var cur any = data
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	items, ok := cur.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Declares reports whether a chart's schema describes one values path, key by key.
// Presence is the whole question: a release from before a key has a chart that
// would refuse it, and box writes a key only where the release can take it. An
// empty schema declares nothing.
func Declares(schema []byte, keys ...string) (bool, error) {
	if len(schema) == 0 {
		return false, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return false, fmt.Errorf("chart schema: %w", err)
	}
	var node any = doc
	for _, key := range keys {
		obj, ok := node.(map[string]any)
		if !ok {
			return false, nil
		}
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			return false, nil
		}
		if node, ok = props[key]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// declaresAuthEdge reports whether the chart's schema describes auth.edge; a
// schema that never mentions it has a release that would reject it.
func declaresAuthEdge(schema []byte) (bool, error) {
	return Declares(schema, "auth", "edge")
}

// declaresRehearsal reports whether the chart's schema describes the rehearsal
// flag and its gate; a release from before the lock has neither.
func declaresRehearsal(schema []byte) (bool, error) {
	flag, err := Declares(schema, "rehearsal")
	if err != nil || !flag {
		return false, err
	}
	return Declares(schema, "rehearsalGate")
}

// DeclaresOIDCGatewayIP reports whether a chart's schema describes
// auth.oidc.gatewayIP, for a caller outside this package.
func DeclaresOIDCGatewayIP(schema []byte) (bool, error) {
	return Declares(schema, "auth", "oidc", "gatewayIP")
}

// setPath writes one value into the document at a key path, creating mappings as
// it goes. It bypasses overlay() deliberately: overlay() is what keeps box's keys
// out.
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

// schemaURL is the name the chart schema is compiled under: a synthetic URL, so
// the compiler never touches the filesystem for it.
const schemaURL = "https://chart.s5n.invalid/values.schema.json"

// noRefLoader refuses every URL the compiler would otherwise load. See Validate.
type noRefLoader struct{}

func (noRefLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is outside the chart schema; refusing to load it", url)
}

// Validate checks a complete values document against a chart release's schema:
// the schema that matters is the one in the release being installed.
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
	// never reach outside it. v6 loads meta-schemas from an embedded copy and
	// sends everything else to this loader, which opens nothing.
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
		// The library words this "got string, want integer"; the user's file reads
		// better the other way round.
		return []Problem{{Path: base, Message: fmt.Sprintf("expected %s, got %s", strings.Join(k.Want, " or "), k.Got)}}
	}
	return []Problem{{Path: base, Message: e.ErrorKind.LocalizedString(printer)}}
}

// tokenPath turns the validator's instance tokens into
// apps.sonarr.instances[1].enabled.
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
