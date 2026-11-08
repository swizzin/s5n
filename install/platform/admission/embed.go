// Package admissionassets embeds the admission policy the platform layer
// installs. policy.yaml beside this file is the only definition: the tests in
// internal/admission read it by path, and box compiles the same bytes in.
package admissionassets

import _ "embed"

// Policy is install/platform/admission/policy.yaml.
//
//go:embed policy.yaml
var Policy []byte
