// Package admission is the platform layer's admission contract in Go.
//
// Render turns one user's grant into the params ConfigMap and the two
// ValidatingAdmissionPolicyBindings that attach the platform policies to
// exactly that namespace. box calls it as root where the platform layer used to
// run a Python script; hack/render-grant is the same render as a command until
// `box adduser` exists.
//
// It is not internal/seedbox. That package decides what a grant may contain from
// a user's own files, before anything is installed. This one is the other side
// of the contract: what root writes into the cluster, and therefore what the
// cluster refuses. They are separate because their inputs are: seedbox reads
// values files, this reads the chart's variants.yaml and the grant root
// assigned.
package admission

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ParamsNamespace is where root keeps every grant's params. A ConfigMap in the
// user's own namespace would be writable by them and enforce nothing.
const ParamsNamespace = "s5n-system"

// ServiceTypes are the grants Render accepts. ClusterIP is one: a box that
// exposes no peers port grants nothing and a ClusterIP Service is still
// admitted by the policy's first rule.
var ServiceTypes = []string{"ClusterIP", "LoadBalancer", "NodePort"}

// nodePortRange is the default Kubernetes NodePort range. A grant's port is
// compared against every nodePort a Service writes, so it has to be a port the
// API server would ever accept.
const (
	minNodePort = 30000
	maxNodePort = 32767
)

// Grant is the part of one user's grant the cluster enforces. It is a subset of
// the grant internal/seedbox owns: only the keys admission reads.
type Grant struct {
	// Namespace is the user's seedbox namespace: the binding selects exactly
	// it, and the params object is named after it.
	Namespace string
	// TorrentNodePort is the single nodePort a Service in this namespace may
	// set. It is a port on the node, so root assigns it (docs/box.md).
	TorrentNodePort int
	// ServiceType is the type the torrent peers Service may use, besides
	// ClusterIP which is always admitted.
	ServiceType string
}

// validate refuses a grant the params render cannot express, naming the input
// as given rather than the parsed value.
func (g Grant) validate() error {
	if !validDNSLabel(g.Namespace) {
		return fmt.Errorf("namespace %q is not a DNS label: lowercase letters, digits and '-', at most 63 characters, not starting or ending with '-'", g.Namespace)
	}
	if g.TorrentNodePort < minNodePort || g.TorrentNodePort > maxNodePort {
		return fmt.Errorf("torrent nodePort %d is outside the NodePort range %d-%d", g.TorrentNodePort, minNodePort, maxNodePort)
	}
	for _, t := range ServiceTypes {
		if g.ServiceType == t {
			return nil
		}
	}
	return fmt.Errorf("torrent Service type %q: a grant may only assign %s", g.ServiceType, strings.Join(ServiceTypes, ", "))
}

// validDNSLabel reports whether s is an RFC 1123 label, which is what a
// namespace name is: it becomes the binding's only selector value and the
// params object's name, so a malformed one would be refused by the API server
// long after root believed the user was set up.
func validDNSLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// Repositories is the blessed list: every distinct repository named by the
// chart's variants.yaml, sorted. This is the only place the list is built, in
// box as in hack/render-grant, so "blessed by the chart" has one definition.
//
// A variant that names no repository is skipped; an app or variant that is not
// a mapping is an error, because that is a chart typo and not a variant without
// a repository.
func Repositories(variants []byte) ([]string, error) {
	if len(strings.TrimSpace(string(variants))) == 0 {
		return nil, fmt.Errorf("variants.yaml is empty")
	}
	var apps map[string]any
	if err := yaml.Unmarshal(variants, &apps); err != nil {
		return nil, fmt.Errorf("variants.yaml: %w", err)
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("variants.yaml names no repositories")
	}
	seen := make(map[string]bool, len(apps))
	names := make([]string, 0, len(apps))
	for app, variantsAny := range apps {
		byName, ok := variantsAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("variants.yaml: app %q is not a map of variants", app)
		}
		for name, variantAny := range byName {
			variant, ok := variantAny.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("variants.yaml: %s.%s is not a variant", app, name)
			}
			repo, _ := variant["repository"].(string)
			if repo == "" {
				continue
			}
			if !seen[repo] {
				seen[repo] = true
				names = append(names, repo)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("variants.yaml names no repositories")
	}
	sort.Strings(names)
	return names, nil
}

// Render is the pure render: variants.yaml's bytes and one grant in, the
// objects that carry it out. The caller writes them; this package never talks
// to a cluster.
func Render(variants []byte, g Grant) (Docs, error) {
	if err := g.validate(); err != nil {
		return Docs{}, err
	}
	return renderForValidGrant(variants, g)
}

// renderForValidGrant is Render without the grant validation, for a caller that
// validated it already and needs the two error sources to stay distinguishable:
// a bad namespace is the caller's input, a file this package cannot read is the
// chart's.
func renderForValidGrant(variants []byte, g Grant) (Docs, error) {
	repos, err := Repositories(variants)
	if err != nil {
		return Docs{}, err
	}
	return grantDocs(repos, g), nil
}

// RenderChart is Render for a chart directory: it reads
// <chart-dir>/variants.yaml, the file the templates resolve images from, so the
// params and the release can never disagree about which repositories exist.
func RenderChart(chartDir string, g Grant) (Docs, error) {
	// The grant is checked before the chart is read, and outside the wrap
	// below: "namespace %q is not a DNS label" is about the input root typed,
	// and prefixing it with variants.yaml would send root looking in the chart.
	if err := g.validate(); err != nil {
		return Docs{}, err
	}
	path := filepath.Join(chartDir, "variants.yaml")
	variants, err := os.ReadFile(path)
	if err != nil {
		return Docs{}, fmt.Errorf("read variants: %w", err)
	}
	docs, err := renderForValidGrant(variants, g)
	if err != nil {
		return Docs{}, fmt.Errorf("%s: %w", path, err)
	}
	docs.Source = path
	return docs, nil
}
