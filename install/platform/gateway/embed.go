// Package gatewayassets embeds the GatewayClass and Gateway the platform layer
// installs. gateway.yaml beside this file is the only definition: the kind e2e
// applies it with kubectl, and box compiles the same bytes in, so the Gateway
// box installs is the Gateway the e2e asserts.
package gatewayassets

import _ "embed"

// Manifest is install/platform/gateway/gateway.yaml.
//
//go:embed gateway.yaml
var Manifest []byte
