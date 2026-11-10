package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyCertManager(t *testing.T) {
	full := CertManagerState{CRD: true, Controller: true, Webhook: true, Version: "1.21.3", GatewayAPI: true}
	cases := []struct {
		name  string
		state CertManagerState
		want  ProbeOutcome
		says  []string
	}{
		{
			name:  "nothing installed",
			state: CertManagerState{},
			want:  OutcomeAbsent,
		},
		{
			name:  "CRDs without a controller are not an install",
			state: CertManagerState{CRD: true},
			want:  OutcomeAbsent,
		},
		{
			name:  "a working Gateway-capable install is used",
			state: full,
			want:  OutcomePresent,
		},
		{
			name:  "the webhook not answering stops",
			state: CertManagerState{CRD: true, Controller: true, Version: "1.21.3", GatewayAPI: true},
			want:  OutcomeConflict,
			says:  []string{"webhook is not answering"},
		},
		{
			name:  "a controller too old for the Gateway solver stops",
			state: CertManagerState{CRD: true, Controller: true, Webhook: true, Version: "1.14.4", GatewayAPI: true},
			want:  OutcomeConflict,
			says:  []string{"v1.14.4", "v1.15.0", "Gateway API HTTPRoute"},
		},
		{
			name:  "an unknown version is not trusted",
			state: CertManagerState{CRD: true, Controller: true, Webhook: true, GatewayAPI: true},
			want:  OutcomeConflict,
			says:  []string{"unknown version"},
		},
		{
			name:  "Gateway API switched off names the setting",
			state: CertManagerState{CRD: true, Controller: true, Webhook: true, Version: "1.21.3"},
			want:  OutcomeConflict,
			says:  []string{"Gateway API support enabled", "config.gatewayAPI.enabled=true", "--enable-gateway-api"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyCertManager(tc.state)
			if got != tc.want {
				t.Fatalf("outcome = %q, want %q (%s)", got, tc.want, reason)
			}
			for _, want := range tc.says {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not name %q", reason, want)
				}
			}
			if got != OutcomeConflict && reason != "" {
				t.Errorf("a non-conflict outcome carries a reason: %q", reason)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"v1.21.3", "1.15.0", true},
		{"1.21.3", "1.15.0", true},
		{"v1.15.0", "1.15.0", true},
		{"v1.15.0-alpha.1", "1.15.0", false}, // semver: a pre-release is before the release
		{"v1.14.9", "1.15.0", false},
		{"v2.0.0", "1.15.0", true},
		{"v1.21", "1.15.0", true},
		{"latest", "1.15.0", false},
		{"", "1.15.0", false},
		{"v1.21.3+build.7", "1.15.0", true},
	}
	for _, tc := range cases {
		if got := atLeast(tc.v, tc.min); got != tc.want {
			t.Errorf("atLeast(%q, %q) = %v, want %v", tc.v, tc.min, got, tc.want)
		}
	}
}

func TestConfigEnablesGatewayAPI(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "the chart's controller config",
			text: "apiVersion: controller.config.cert-manager.io/v1alpha1\ngatewayAPI:\n  enabled: true\nkind: ControllerConfiguration\n",
			want: true,
		},
		{
			name: "off",
			text: "apiVersion: controller.config.cert-manager.io/v1alpha1\ngatewayAPI:\n  enabled: false\n",
			want: false,
		},
		{
			name: "absent",
			text: "apiVersion: controller.config.cert-manager.io/v1alpha1\nlogging:\n  verbosity: 2\n",
			want: false,
		},
		{
			name: "a sibling enabled key does not count",
			text: "logging:\n  enabled: true\ngatewayAPI:\n  enabled: false\n",
			want: false,
		},
		{
			name: "a later top-level key ends the block",
			text: "gatewayAPI:\n  enabled: true\nlogging:\n  verbosity: 2\n",
			want: true,
		},
		{
			name: "JSON",
			text: `{"apiVersion":"controller.config.cert-manager.io/v1alpha1","gatewayAPI":{"enabled":true}}`,
			want: true,
		},
		{
			name: "inline YAML",
			text: "gatewayAPI: {enabled: true}\n",
			want: true,
		},
		{
			name: "a quoted true is still true to cert-manager's loader",
			text: "gatewayAPI:\n  enabled: \"true\"\n",
			want: true,
		},
		{
			name: "a quoted key",
			text: "\"gatewayAPI\":\n  enabled: true\n",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configEnablesGatewayAPI(tc.text); got != tc.want {
				t.Errorf("configEnablesGatewayAPI = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"quay.io/jetstack/cert-manager-controller:v1.21.3":    "v1.21.3",
		"cert-manager-controller:1.21.3":                      "1.21.3",
		"registry:5000/cert-manager-controller:v1.21.3":       "v1.21.3",
		"quay.io/jetstack/cert-manager-controller@sha256:abc": "",
		"quay.io/jetstack/cert-manager-controller":            "",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// The read path over a real API: controller flags, the mounted controller
// configuration, and the webhook beside them. The decision is
// classifyCertManager's (tested above); this pins what the probe collects.
func TestHTTPProberReadsInstalledCertManager(t *testing.T) {
	mux := http.NewServeMux()
	list := `{"items":[
{"metadata":{"name":"cert-manager","namespace":"cert-manager","labels":{"app.kubernetes.io/version":"v1.21.3"}},
 "spec":{"template":{"spec":{"volumes":[{"name":"config","configMap":{"name":"cert-manager"}}],
   "containers":[{"name":"cert-manager-controller","image":"quay.io/jetstack/cert-manager-controller:v1.21.3",
     "args":["--v=2","--config=/var/cert-manager/config/config.yaml"],
     "volumeMounts":[{"name":"config","mountPath":"/var/cert-manager/config"}]}]}}},
 "status":{"availableReplicas":1,"conditions":[{"type":"Available","status":"True"}]}},
{"metadata":{"name":"cert-manager-webhook","namespace":"cert-manager","labels":{"app.kubernetes.io/name":"webhook","app.kubernetes.io/component":"webhook"}},
 "status":{"availableReplicas":1,"conditions":[{"type":"Available","status":"True"}]}}
]}`
	mux.HandleFunc("/apis/apps/v1/deployments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(list))
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/cert-manager/deployments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(list))
	})
	mux.HandleFunc("/api/v1/namespaces/cert-manager/configmaps/cert-manager", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"config.yaml":"apiVersion: controller.config.cert-manager.io/v1alpha1\ngatewayAPI:\n  enabled: true\nkind: ControllerConfiguration\n"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := NewHTTPProber(kubeconfigFixtureToken(t, srv.URL), true)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	prober := p.(*HTTPProber)
	state, err := prober.certManagerState(context.Background(), true)
	if err != nil {
		t.Fatalf("certManagerState: %v", err)
	}
	if !state.CRD || !state.Controller || !state.Webhook || !state.GatewayAPI {
		t.Fatalf("state = %+v, want a complete Gateway-capable install", state)
	}
	if state.Version != "v1.21.3" {
		t.Fatalf("version = %q", state.Version)
	}
	if outcome, reason := classifyCertManager(state); outcome != OutcomePresent {
		t.Fatalf("outcome = %q (%s), want present", outcome, reason)
	}
}

// A controller started with the legacy flag needs no ConfigMap read.
func TestCertManagerLegacyFlag(t *testing.T) {
	dep := deploymentItem{}
	dep.Spec.Template.Spec.Containers = []deploymentContainer{{
		Image: "quay.io/jetstack/cert-manager-controller:v1.15.5",
		Args:  []string{"--enable-gateway-api"},
	}}
	enabled, err := gatewayAPIEnabled(context.Background(), nil, dep)
	if err != nil || !enabled {
		t.Fatalf("gatewayAPIEnabled = %v, %v", enabled, err)
	}
}

// A cert-manager box cannot use stops the whole bootstrap, with the probe's
// own words, in every mode: box never installs beside it or rewrites its
// flags.
func TestDecidePlatformStopsOnCertManagerConflict(t *testing.T) {
	const reason = "cert-manager is installed but its controller does not have Gateway API support enabled: set config.gatewayAPI.enabled=true"
	components, conflict := decidePlatform(DefaultPlatform(), PlatformProbe{
		GatewayAPI:        OutcomePresent,
		Gateway:           OutcomePresent,
		Classes:           []GatewayClass{{Name: "eg", ControllerName: EnvoyGatewayController, Accepted: true}},
		CertManager:       OutcomeConflict,
		CertManagerReason: reason,
	})
	if !strings.Contains(conflict, reason) {
		t.Fatalf("conflict = %q, want the probe's reason", conflict)
	}
	for _, c := range components {
		if c.Component == ComponentCertManager {
			if c.Action != "stop" || c.Reason != reason {
				t.Fatalf("cert-manager decision = %+v, want stop with the probe's reason", c)
			}
			return
		}
	}
	t.Fatal("cert-manager is not in the decision")
}
