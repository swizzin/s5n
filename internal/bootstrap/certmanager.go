package bootstrap

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// CertManagerMinVersion is the oldest cert-manager whose controller can solve
// an HTTP-01 challenge through a Gateway API HTTPRoute. Gateway API support
// graduated to beta in v1.15 with the --enable-gateway-api flag (and the
// gatewayHTTPRoute solver); older controllers have no such solver at all.
const CertManagerMinVersion = "1.15.0"

// CertManagerState is everything a read-only probe can learn about a
// cert-manager that is already installed. It is deliberately only facts: which
// of them decides "use", "install" or "stop" is classifyCertManager, a pure
// function a test can drive from a stub.
type CertManagerState struct {
	// CRD says the certificates.cert-manager.io CRD exists.
	CRD bool
	// Controller says a controller Deployment answered, so there is a running
	// install and not merely leftover CRDs.
	Controller bool
	// Webhook says that install's webhook Deployment is available. A webhook
	// that is not running cannot admit a Certificate or an Issuer.
	Webhook bool
	// Version is the controller image's tag (or, failing that, the version
	// label on its Deployment), without the leading "v".
	Version string
	// GatewayAPI says the controller runs with Gateway API support enabled:
	// the --enable-gateway-api flag (v1.15-v1.19) or gatewayAPI.enabled in
	// the controller's own --config file (v1.16 and later).
	GatewayAPI bool
}

// classifyCertManager turns what the probe saw into box's decision for the
// cert-manager component. It is pure so the refusals -- which must happen
// before the confirm and before any write -- are testable without a cluster.
//
// An install box cannot use stops bootstrap rather than being replaced: box
// never changes another install's flags, and Helm would refuse to install its
// own release under the same name anyway. The reason names the setting to
// change, because that is the operator's next move.
func classifyCertManager(s CertManagerState) (ProbeOutcome, string) {
	if !s.CRD || !s.Controller {
		return OutcomeAbsent, ""
	}
	if !s.Webhook {
		return OutcomeConflict, "cert-manager is installed, but its webhook is not answering: a Certificate or Issuer cannot be admitted until it does. Repair that install (its webhook Deployment is not available) and run bootstrap again, or set platform.cert-manager.mode=external to have box verify it instead of installing one"
	}
	if !atLeast(s.Version, CertManagerMinVersion) {
		return OutcomeConflict, fmt.Sprintf("cert-manager %s is older than v%s, the first release whose controller can solve an HTTP-01 challenge through a Gateway API HTTPRoute: upgrade that install, or set platform.cert-manager.mode=external to have box verify it instead of installing one", versionOrUnknown(s.Version), CertManagerMinVersion)
	}
	if !s.GatewayAPI {
		return OutcomeConflict, "cert-manager is installed but its controller does not have Gateway API support enabled, so it cannot solve an HTTP-01 challenge through the s5n Gateway: set config.gatewayAPI.enabled=true (cert-manager 1.16 and later) or start the controller with --enable-gateway-api (1.15) and run bootstrap again. box never changes another install's flags, and will not install a second cert-manager beside it"
	}
	return OutcomePresent, ""
}

func versionOrUnknown(v string) string {
	if v == "" {
		return "(unknown version)"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

// atLeast reports whether a controller version such as "v1.21.3" is at least
// min. semver owns the comparison, so the "v" and any build or pre-release
// suffix are its rules rather than a hand-kept parser's; a string that is not
// a version (an empty tag, "latest") is not one box can trust, and fails the
// comparison so the operator gets the version refusal above.
func atLeast(v, min string) bool {
	got, err := semver.NewVersion(strings.TrimSpace(v))
	if err != nil {
		return false
	}
	want, err := semver.NewVersion(min)
	if err != nil {
		return false
	}
	return !got.LessThan(want)
}

// configEnablesGatewayAPI reads the one setting out of a cert-manager
// ControllerConfiguration document. cert-manager 1.16 moved controller flags
// into that file, and the chart renders it as YAML
// (config.gatewayAPI.enabled: true); a hand-written ConfigMap may hold the
// same document as JSON. It is decoded with the same YAML-or-JSON decoder the
// platform's manifests use, so the document is read rather than pattern-
// matched, and a quoted "true" is still true for cert-manager's loader.
func configEnablesGatewayAPI(text string) bool {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return false
	}
	gw, _ := doc["gatewayAPI"].(map[string]any)
	switch v := gw["enabled"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}
