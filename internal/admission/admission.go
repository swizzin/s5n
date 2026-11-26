// Package admission is the platform layer's admission contract: Render turns one
// user's grant into the params ConfigMap and the two bindings that attach the
// platform policies to that namespace, and UnblessedImages applies the image rule
// to pods that already exist, which a binding never re-checks.
//
// The image rule lives here in Go as well as in policy.yaml, so the pods a new
// binding strands can be named before it is created. TestBlessedMatchesThePolicyRule
// and TestPolicyCELStillMatchesTheGoRule hold the two rules together.
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
// exposes no peers port grants nothing, and a ClusterIP Service is still
// admitted by the policy's first rule.
var ServiceTypes = []string{"ClusterIP", "LoadBalancer", "NodePort"}

// nodePortRange is the default Kubernetes NodePort range: a grant's port is
// compared against every nodePort a Service writes.
const (
	minNodePort = 30000
	maxNodePort = 32767
)

// Grant is the part of one user's grant the cluster enforces.
type Grant struct {
	// Namespace is the user's seedbox namespace: the binding selects exactly it,
	// and the params object is named after it.
	Namespace string
	// TorrentNodePort is the single nodePort a Service in this namespace may set.
	// It is a port on the node, so root assigns it (docs/box.md).
	TorrentNodePort int
	// ServiceType is the type the torrent peers Service may use, besides
	// ClusterIP, which is always admitted.
	ServiceType string
}

// validate refuses a grant the params render cannot express, naming the input as
// given.
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

// validDNSLabel reports whether s is an RFC 1123 label: it becomes the binding's
// selector value and the params object's name, so a malformed one would be
// refused by the API server long after root believed the user was set up.
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

// Image is one blessed variant: the repository the chart resolves it to, and the
// pins inside it. `box update` reads it to tell which enabled apps' images move
// between two releases, and so which databases a step snapshots.
type Image struct {
	// Repository is the only image the chart can run for this app; values may not
	// name one (docs/box.md, "Image choice: blessed variants").
	Repository string
	// Tag and Digest are the pins inside that repository. A digest bump alone is
	// an image change, which is why update compares Repository and Digest.
	Tag    string
	Digest string
}

// Images is the chart's variants.yaml as app -> variant -> image. It is the one
// parse of the file, so the blessed list, the policy's params and update's
// moving-app list cannot disagree.
//
// An app or variant that is not a mapping is an error: that is a chart typo, not
// a variant without an image. A variant that names no repository is kept as-is;
// Repositories skips it.
func Images(variants []byte) (map[string]map[string]Image, error) {
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
	out := make(map[string]map[string]Image, len(apps))
	for app, variantsAny := range apps {
		byName, ok := variantsAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("variants.yaml: app %q is not a map of variants", app)
		}
		images := make(map[string]Image, len(byName))
		for name, variantAny := range byName {
			variant, ok := variantAny.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("variants.yaml: %s.%s is not a variant", app, name)
			}
			repo, _ := variant["repository"].(string)
			tag, _ := variant["tag"].(string)
			digest, _ := variant["digest"].(string)
			images[name] = Image{Repository: repo, Tag: tag, Digest: digest}
		}
		out[app] = images
	}
	return out, nil
}

// Repositories is the blessed list: every distinct repository named by the
// chart's variants.yaml, sorted. It is built only here, in box as in
// hack/render-grant, so "blessed by the chart" has one definition.
func Repositories(variants []byte) ([]string, error) {
	apps, err := Images(variants)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(apps))
	var names []string
	for _, byName := range apps {
		for _, image := range byName {
			if image.Repository == "" {
				continue
			}
			if !seen[image.Repository] {
				seen[image.Repository] = true
				names = append(names, image.Repository)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("variants.yaml names no repositories")
	}
	sort.Strings(names)
	return names, nil
}

// Render is the pure render: variants.yaml's bytes and one grant in, the objects
// that carry it out. The caller writes them; this package never talks to a
// cluster.
func Render(variants []byte, g Grant) (Docs, error) {
	if err := g.validate(); err != nil {
		return Docs{}, err
	}
	return renderForValidGrant(variants, g)
}

// renderForValidGrant is Render without the grant validation, keeping the two
// error sources distinguishable: a bad namespace is the caller's input, a file
// this cannot read is the chart's.
func renderForValidGrant(variants []byte, g Grant) (Docs, error) {
	repos, err := Repositories(variants)
	if err != nil {
		return Docs{}, err
	}
	return grantDocs(repos, g), nil
}

// RenderChart is Render for a chart directory, reading <chart-dir>/variants.yaml
// so the params and the release cannot disagree about which repositories exist.
func RenderChart(chartDir string, g Grant) (Docs, error) {
	// The grant is checked before the chart is read: a bad namespace is about the
	// input root typed, and prefixing it with variants.yaml would send root looking
	// in the chart.
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
