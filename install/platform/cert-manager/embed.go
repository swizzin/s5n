// Package certmanagerassets embeds the ClusterIssuers the platform layer
// installs. issuers.yaml beside this file is the only definition: box compiles
// the bytes in, so what box applies and what a reader reviews cannot drift.
package certmanagerassets

import _ "embed"

// Issuers is install/platform/cert-manager/issuers.yaml.
//
//go:embed issuers.yaml
var Issuers []byte
