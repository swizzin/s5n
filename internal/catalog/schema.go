package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
)

// schema is the part of a chart's values.schema.json the catalog needs: whether a
// values path is a leaf, and its type. Local "definitions" references are
// followed; any other reference is refused, because following it would load
// something outside the chart box was given.
type schema struct {
	root map[string]any
}

// maxRefDepth bounds $ref chasing in a hand-written schema; the chart nests two.
const maxRefDepth = 10

func parseSchema(src []byte) (*schema, error) {
	var root map[string]any
	if err := json.Unmarshal(src, &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("is empty")
	}
	return &schema{root: root}, nil
}

// at walks a key path through the schema's properties; a nil node with a nil
// error means the schema does not describe the key.
func (s *schema) at(keys []string) (map[string]any, error) {
	node := s.root
	for _, k := range keys {
		resolved, err := s.resolve(node, 0)
		if err != nil {
			return nil, err
		}
		props, ok := resolved["properties"].(map[string]any)
		if !ok {
			return nil, nil
		}
		next, ok := props[k].(map[string]any)
		if !ok {
			return nil, nil
		}
		node = next
	}
	return s.resolve(node, 0)
}

// resolve follows one $ref, if the node has one.
func (s *schema) resolve(node map[string]any, depth int) (map[string]any, error) {
	ref, ok := node["$ref"].(string)
	if !ok {
		return node, nil
	}
	if depth >= maxRefDepth {
		return nil, fmt.Errorf("values.schema.json has a reference loop at %s", ref)
	}
	const prefix = "#/definitions/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("values.schema.json refers to %q; box only follows refs under #/definitions/", ref)
	}
	defs, _ := s.root["definitions"].(map[string]any)
	target, ok := defs[ref[len(prefix):]].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("values.schema.json's %s is defined nowhere", ref)
	}
	return s.resolve(target, depth+1)
}
