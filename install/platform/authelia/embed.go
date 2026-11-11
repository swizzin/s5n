// Package autheliaassets embeds the login's two files: the configuration box
// renders per host, and the manifests box applies for it.
//
// embed.go lives in this directory with the files it embeds because a go:embed
// directive cannot reach a sibling tree. Nothing here is a chart: Authelia runs
// as its own Deployment with box's own configuration, which is why the bytes
// box compiles in are the bytes the e2e runs (docs/box.md, "Logins").
//
// Both files are Go text/templates. They are syntax-checked with the values
// box gives them by install/platform/authelia's own tests, so a template that
// no longer renders is a failing test rather than an install that stops at the
// first API call.
package autheliaassets

import _ "embed"

// Configuration is the Authelia configuration: one per host, because the
// cookie domain, the portal's URL and the access_control rules are all the
// host's. It is a template, and the .tmpl suffix is deliberate: editors and
// linters that validate YAML would refuse a file with Go actions in it.
//
//go:embed configuration.yml.tmpl
var Configuration []byte

// Manifests are the objects the login is: the claim's Service, the Deployment,
// the ConfigMap's consumer side, the ReferenceGrant and the SecurityPolicy
// that puts the login in front of the Gateway.
//
//go:embed manifests.yaml.tmpl
var Manifests []byte
