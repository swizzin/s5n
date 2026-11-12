package platform

import (
	"testing"

	"github.com/swizzin/s5n/internal/authelia"
)

// The read that decides whether a host needs its listener: the Gateway's own
// document, matched by the name the per-host step writes the listener under.
func TestHasHTTPSListenerReadsTheGatewaysOwnListeners(t *testing.T) {
	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP"},
				map[string]any{"name": ListenerName("box.example.com"), "port": int64(443), "protocol": "HTTPS"},
				map[string]any{"name": HTTPListenerName("box.example.com"), "port": int64(80), "protocol": "HTTP"},
			},
		},
	}
	if !hasHTTPSListener(gateway, ListenerName("box.example.com")) {
		t.Fatal("the host's own HTTPS listener was not found")
	}
	// The port-80 listener is a different object with a different job: it is
	// not what serves the app.
	if hasHTTPSListener(gateway, HTTPListenerName("box.example.com")) {
		t.Fatal("a port-80 listener counted as the host's TLS terminator")
	}
	if hasHTTPSListener(gateway, ListenerName("seerr.box.example.com")) {
		t.Fatal("a name nobody wrote a listener for was reported as served")
	}
	if hasHTTPSListener(map[string]any{}, ListenerName("box.example.com")) {
		t.Fatal("a Gateway with no listeners reported a host as served")
	}
}

// The third object a host needs: the login's rule for its name. A host with a
// listener and a Certificate but no rule gets default_policy deny from the
// login, so this read is what makes apply write it.
func TestHasHostRuleReadsTheLoginsConfiguration(t *testing.T) {
	ruled := "- domain: ['box.example.com']\n      policy: 'one_factor'\n      subject:\n        - 'user:alice'\n"
	unruled := "- domain: ['box.example.com']\n      policy: 'one_factor'\n      subject:\n        - 'user:alice'\n"
	with := map[string]any{
		"kind": "ConfigMap",
		"data": map[string]any{authelia.ConfigFile: "access_control:\n  default_policy: 'deny'\n  rules:\n    " + ruled + "    - domain: ['seerr.box.example.com']\n      policy: 'bypass'\n"},
	}
	if !hasHostRule(with, "seerr.box.example.com") {
		t.Fatal("the app host's rule was not found in the login's configuration")
	}
	if !hasHostRule(with, "box.example.com") {
		t.Fatal("the box's own rule was not found")
	}
	// A host whose listener and Certificate exist and whose rule does not is
	// the state this read has to call missing.
	without := map[string]any{
		"kind": "ConfigMap",
		"data": map[string]any{authelia.ConfigFile: "access_control:\n  default_policy: 'deny'\n  rules:\n    " + unruled},
	}
	if hasHostRule(without, "seerr.box.example.com") {
		t.Fatal("a name with no rule counted as served")
	}
	// No login at all: no configuration, no rule.
	if hasHostRule(map[string]any{}, "seerr.box.example.com") {
		t.Fatal("a box with no login configuration reported a rule")
	}
	if hasHostRule(map[string]any{"data": map[string]any{}}, "seerr.box.example.com") {
		t.Fatal("an empty ConfigMap reported a rule")
	}
}
