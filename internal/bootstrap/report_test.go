package bootstrap

import (
	"bytes"
	"testing"

	"github.com/swizzin/s5n/internal/say"
)

// ReportResult is the same transcript as FormatResult: the live renderer may
// lay the facts out differently, but every non-TTY run and every e2e grep reads
// the plain reporter, and it must not drift by one byte.
func TestReportResultMatchesFormatResult(t *testing.T) {
	cases := []struct {
		name string
		res  Result
	}{
		{"stop", Result{
			Action: ActionStop,
			Reason: "this host already runs k3s, and box will not replace it",
			Skips:  []string{"/etc/rancher/k3s/k3s.yaml: not reachable", "/home/example/.kube/config: absent"},
			Notes:  []string{"a containerd daemon is running"},
		}},
		{"install", Result{
			Action:       ActionInstall,
			Source:       SourceK0sManaged,
			Kubeconfig:   "/var/lib/k0s/pki/admin.conf",
			Distribution: DistK0s,
			Reason:       "no cluster was found; box will install k0s",
			Skips:        []string{"/home/example/.kube/config: absent"},
			Notes:        []string{"docker is installed"},
		}},
		{"use-cluster", Result{
			Action:       ActionUseCluster,
			Source:       SourceExternal,
			Kubeconfig:   "/home/example/.kube/config",
			Distribution: DistK3s,
			Nodes:        1,
			Platform: []ComponentResult{
				{Component: ComponentGatewayAPI, Mode: "auto", Action: "install", Reason: "no Gateway API CRDs"},
				{Component: ComponentLoadBalancer, Mode: "auto", Action: "probe", Reason: "the Gateway's own status decides"},
				{Component: ComponentCertManager, Mode: "auto", Action: "use", Reason: "cert-manager 1.16 is ready"},
			},
			Gateway: GatewayChoice{
				Class:          "eg",
				ControllerName: "gateway.envoyproxy.io/gatewayclass-controller",
				BoxInstalls:    true,
			},
			Reason: "using the kubeconfig at /home/example/.kube/config",
		}},
		{"recorded-class", Result{
			Action:       ActionUseManaged,
			Source:       SourceExternal,
			Kubeconfig:   "/home/example/.kube/config",
			Distribution: DistVanilla,
			Nodes:        3,
			Gateway: GatewayChoice{
				Class:          "envoy",
				ControllerName: "gateway.envoyproxy.io/gatewayclass-controller",
			},
			Reason: "using the cluster box recorded",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := FormatResult(c.res)
			var buf bytes.Buffer
			ReportResult(c.res, &say.Plain{W: &buf})
			if got := buf.String(); got != want {
				t.Fatalf("plain report drifted from FormatResult\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}
