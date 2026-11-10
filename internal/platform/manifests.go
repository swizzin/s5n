package platform

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	admissionassets "github.com/swizzin/s5n/install/platform/admission"
	certmanagerassets "github.com/swizzin/s5n/install/platform/cert-manager"
	gatewayassets "github.com/swizzin/s5n/install/platform/gateway"
	"github.com/swizzin/s5n/internal/kube"
)

// Access is one permission the platform needs before it writes: may this
// identity do a verb on a resource? The type and the review live in
// internal/kube, with the verbs server-side apply needs, so `box bootstrap`,
// root's per-host TLS write and `box apply` ask the same question.
type Access = kube.Access

// gatewayManifest is the GatewayClass and Gateway the chart's HTTPRoutes
// attach to. The bytes are install/platform/gateway/gateway.yaml, the same
// file the kind e2e applies with kubectl, embedded rather than copied: box and
// the e2e cannot disagree about what the platform's Gateway is.
var gatewayManifest = gatewayassets.Manifest

// gatewayObjects is the Gateway box applies on every run: the namespace, the
// Gateway pointed at the chosen class, and -- only when box installs Envoy
// Gateway -- the GatewayClass box owns. Keeping one embedded definition and
// editing it here is what stops the e2e's copy and box's copy from drifting;
// the class is the only field that varies.
func gatewayObjects(class string, withClass bool) ([]map[string]any, error) {
	docs, err := ParseManifests(gatewayManifest)
	if err != nil {
		return nil, fmt.Errorf("the Gateway manifest: %w", err)
	}
	out := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		kind, _, _ := objectMeta(doc)
		if kind == "GatewayClass" && !withClass {
			continue
		}
		if kind == "Gateway" {
			spec, _ := doc["spec"].(map[string]any)
			if spec == nil {
				return nil, errors.New("the Gateway manifest: the Gateway has no spec")
			}
			spec["gatewayClassName"] = class
		}
		out = append(out, doc)
	}
	return out, nil
}

// admissionPolicy is install/platform/admission/policy.yaml, embedded from the
// file beside it. The Go tests in internal/admission read that file by path,
// and box compiles the same bytes in, so there is one definition of the rule.
var admissionPolicy = admissionassets.Policy

// issuerManifest is install/platform/cert-manager/issuers.yaml: the two
// ClusterIssuers root owns. The file names Let's Encrypt's production
// directory; issuerObjects points it elsewhere only for a development run.
var issuerManifest = certmanagerassets.Issuers

// issuerNames is how the plan and the tests name the two issuers.
const (
	LetsEncryptIssuer = "s5n-letsencrypt"
	SelfSignedIssuer  = "s5n-selfsigned"
)

// issuerObjects is the ClusterIssuers box applies on every run. acmeServer is
// empty in a real run, which leaves the manifest's Let's Encrypt directory as
// written. A development run (the e2e's Pebble) names another directory, and
// that ACME endpoint is a test server with its own self-signed certificate, so
// the issuer is marked to skip verifying the endpoint's TLS. It is a per-issuer
// field on the test issuer only; production keeps verification on.
func issuerObjects(acmeServer string) ([]map[string]any, error) {
	docs, err := ParseManifests(issuerManifest)
	if err != nil {
		return nil, fmt.Errorf("the cert-manager issuers manifest: %w", err)
	}
	if acmeServer == "" {
		return docs, nil
	}
	found := false
	for _, doc := range docs {
		kind, _, name := objectMeta(doc)
		if kind != "ClusterIssuer" || name != LetsEncryptIssuer {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		acme, _ := spec["acme"].(map[string]any)
		if acme == nil {
			return nil, errors.New("the cert-manager issuers manifest: s5n-letsencrypt has no acme spec")
		}
		acme["server"] = acmeServer
		acme["skipTLSVerify"] = true
		found = true
	}
	if !found {
		return nil, errors.New("the cert-manager issuers manifest: s5n-letsencrypt is missing")
	}
	return docs, nil
}

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
