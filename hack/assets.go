// Package hack holds the repository's end-to-end assets as data.
//
// The e2e scripts apply these files with kubectl, and box embeds the same
// bytes (go:embed cannot reach a sibling tree from internal/platform, so the
// declaration lives beside the file it embeds). One definition, two callers:
// the Gateway box installs is the Gateway the kind e2e asserts, byte for byte.
package hack

import _ "embed"

// EnvoyGatewayManifest is hack/envoy-gateway.yaml.
//
//go:embed envoy-gateway.yaml
var EnvoyGatewayManifest []byte
