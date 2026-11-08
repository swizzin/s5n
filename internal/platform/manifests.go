package platform

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	gatewayassets "github.com/swizzin/s5n/hack"
	admissionassets "github.com/swizzin/s5n/install/platform/admission"
)

// Access is one SelfSubjectAccessReview question: may this identity do verb on
// resource in namespace (empty means cluster-scoped)?
type Access struct {
	Verb      string
	Group     string
	Resource  string
	Namespace string
	// What is how box names the permission when it is missing.
	What string
}

// gatewayManifest is the GatewayClass and Gateway the chart's HTTPRoutes
// attach to. The bytes are hack/envoy-gateway.yaml, the same file the kind e2e
// applies with kubectl, embedded rather than copied: box and the e2e cannot
// disagree about what the platform's Gateway is.
var gatewayManifest = gatewayassets.EnvoyGatewayManifest

// admissionPolicy is install/platform/admission/policy.yaml, embedded from the
// file beside it. The Go tests in internal/admission read that file by path,
// and box compiles the same bytes in, so there is one definition of the rule.
var admissionPolicy = admissionassets.Policy

// poolManifest is MetalLB's address pool: the node's primary InternalIP as a
// /32, which is the PLAN.md decision -- one address, no extra ARP, no cloud
// integration. The L2 advertisement is what makes the speaker answer for it.
func poolManifest(ip string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: %s
  namespace: %s
spec:
  addresses:
    - %s/32
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: %s
  namespace: %s
spec:
  ipAddressPools:
    - %s
`, MetalLBPoolName, MetalLBNamespace, ip, MetalLBL2Name, MetalLBNamespace, MetalLBPoolName))
}

// ParseManifests splits a multi-document YAML/JSON manifest into the objects
// box applies. It decodes the way the API server does (YAML to JSON types), so
// a manifest that parses here is one the cluster can read.
func ParseManifests(raw []byte) ([]map[string]any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []map[string]any
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		if kind, _ := obj["kind"].(string); kind == "" {
			return nil, errors.New("manifest: a document has no kind")
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, errors.New("manifest: no objects")
	}
	return out, nil
}

// objectMeta reads the name, namespace and kind of a decoded manifest. It is
// what box's output and tests use; the cluster client maps the kind itself.
func objectMeta(obj map[string]any) (kind, namespace, name string) {
	kind, _ = obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return kind, "", ""
	}
	namespace, _ = meta["namespace"].(string)
	name, _ = meta["name"].(string)
	return kind, namespace, name
}
