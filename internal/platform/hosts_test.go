package platform

import "testing"

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
