package admission

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Docs is what one grant renders to, in apply order: the params object, then
// the bindings, one per policy the grant is held to.
type Docs struct {
	// Source names the variants file the blessed list came from. It is only
	// the generated-from note on the first object; empty writes no note.
	Source string
	// Params is the grant as policy params, in s5n-system.
	Params Params
	// Bindings are cluster-scoped and select exactly the grant's namespace.
	Bindings []Binding
}

// Params is the ConfigMap the policies read as paramKind. Its name is
// s5n-grant-<namespace>, so one namespace's grant cannot be read with
// another's.
type Params struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   ObjectMeta `yaml:"metadata"`
	// Data is every value CEL reads through params.data.
	Data ParamsData `yaml:"data"`
}

// ParamsData is the grant as strings, because ConfigMap data is strings and no
// number survives the round trip otherwise. torrentNodePort is read with
// int(params.data.torrentNodePort) in the policy.
type ParamsData struct {
	// BlessedRepositories is a comma-separated list, the form CEL's
	// split(',') reads. Sorted and deduped, so a re-render of unchanged input
	// is byte-identical and `kubectl apply` is a no-op.
	BlessedRepositories string `yaml:"blessedRepositories"`
	ServiceType         string `yaml:"serviceType"`
	TorrentNodePort     string `yaml:"torrentNodePort"`
}

// Binding attaches one policy to one namespace with one params object. It is
// cluster-scoped on purpose: a binding in the user's namespace would be
// theirs to delete.
type Binding struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   ObjectMeta  `yaml:"metadata"`
	Spec       BindingSpec `yaml:"spec"`
}

// BindingSpec is the subset of a binding's spec this package writes. Anything
// else would be a selector root did not decide.
type BindingSpec struct {
	PolicyName        string   `yaml:"policyName"`
	ValidationActions []string `yaml:"validationActions"`
	// ParamRef is nil for a policy that reads no params: the login policy is
	// a constant rule, so binding it must not point at a ConfigMap.
	ParamRef       *ParamRef      `yaml:"paramRef,omitempty"`
	MatchResources MatchResources `yaml:"matchResources"`
}

// ParamRef says which object is the policy's params. parameterNotFoundAction
// Deny is what makes a deleted params object refuse writes instead of admitting
// them.
type ParamRef struct {
	Name                    string `yaml:"name"`
	Namespace               string `yaml:"namespace"`
	ParameterNotFoundAction string `yaml:"parameterNotFoundAction"`
}

// MatchResources narrows the policy to one namespace.
type MatchResources struct {
	NamespaceSelector LabelSelector `yaml:"namespaceSelector"`
}

// LabelSelector is the one selector form this package writes: a set of
// matchExpressions, no matchLabels.
type LabelSelector struct {
	MatchExpressions []MatchExpression `yaml:"matchExpressions"`
}

// MatchExpression is one requirement in a label selector.
type MatchExpression struct {
	Key      string   `yaml:"key"`
	Operator string   `yaml:"operator"`
	Values   []string `yaml:"values"`
}

// ObjectMeta is the subset of object metadata these objects carry. Namespace is
// empty on the bindings, which are cluster-scoped.
type ObjectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// labelSelectorKey is the label the API server sets on every namespace to its
// own name. A binding is cluster-scoped and the user cannot edit it, so
// selecting by that label is selecting that namespace and nothing else.
const labelSelectorKey = "kubernetes.io/metadata.name"

// grantDocs builds the objects for a validated grant and a repository list.
// Split from Render so the shape is testable without re-deriving the list.
func grantDocs(repos []string, g Grant) Docs {
	name := "s5n-grant-" + g.Namespace
	binding := func(bindingName, policy string, withParams bool) Binding {
		var params *ParamRef
		if withParams {
			params = &ParamRef{
				// The policies that read the grant's params read the one
				// object; a binding's own name is not it.
				Name:                    name,
				Namespace:               ParamsNamespace,
				ParameterNotFoundAction: "Deny",
			}
		}
		return Binding{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "ValidatingAdmissionPolicyBinding",
			Metadata: ObjectMeta{
				Name:   bindingName,
				Labels: map[string]string{"app.kubernetes.io/part-of": "s5n"},
			},
			Spec: BindingSpec{
				PolicyName:        policy,
				ValidationActions: []string{"Deny"},
				ParamRef:          params,
				MatchResources: MatchResources{
					NamespaceSelector: LabelSelector{
						MatchExpressions: []MatchExpression{{
							Key:      labelSelectorKey,
							Operator: "In",
							Values:   []string{g.Namespace},
						}},
					},
				},
			},
		}
	}
	return Docs{
		Params: Params{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Metadata: ObjectMeta{
				Name:      name,
				Namespace: ParamsNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "s5n",
					"s5n.io/grant":              g.Namespace,
				},
			},
			Data: ParamsData{
				BlessedRepositories: strings.Join(repos, ","),
				ServiceType:         g.ServiceType,
				TorrentNodePort:     fmt.Sprintf("%d", g.TorrentNodePort),
			},
		},
		Bindings: []Binding{
			binding(name, "s5n-blessed-images", true),
			binding(name+"-service", "s5n-granted-service", true),
			// The login is root's, and a namespace-scoped kubeconfig with
			// namespace-admin rights could otherwise delete or replace the
			// SecurityPolicy and route around it. The policy needs no params:
			// it refuses the writes outright.
			binding(name+"-login", "s5n-no-login-changes", false),
			// Neither does this one: Authelia serves /auth without a login,
			// and Gateway API would give a more specific route the request.
			binding(name+"-auth-route", "s5n-no-auth-routes", false),
		},
	}
}

// YAML writes the objects as a multi-document stream, the input `kubectl apply
// -f -` expects. Two-space indent, because root reads this when a user's
// namespace is bound and a four-space nested list reads worse in a terminal.
func (d Docs) YAML() ([]byte, error) {
	var buf bytes.Buffer
	if d.Source != "" {
		fmt.Fprintf(&buf, "# Generated from %s; edit the chart, not this.\n", d.Source)
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	docs := make([]any, 0, 1+len(d.Bindings))
	docs = append(docs, d.Params)
	for _, b := range d.Bindings {
		docs = append(docs, b)
	}
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("encode grant: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode grant: %w", err)
	}
	return buf.Bytes(), nil
}
