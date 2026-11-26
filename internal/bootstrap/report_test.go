package bootstrap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
)

// What `box bootstrap` says about the cluster: one ✓ row, the evidence in the log.
// FormatResult stays the dry run's output.
func report(res Result) (screen, log string) {
	var out, details bytes.Buffer
	ReportResult(res, &say.Plain{W: &out, Log: &details})
	return out.String(), details.String()
}

// A fresh install's first block: what box found. The kubeconfig path, the probes and
// the class are the log's.
func TestTheClusterHalfOfTheScreenIsOneRow(t *testing.T) {
	res := Result{
		Action:       ActionUseCluster,
		Source:       SourceExternal,
		Kubeconfig:   "/home/example/.kube/config",
		Distribution: DistK3s,
		Nodes:        1,
		Platform: []ComponentResult{
			{Component: ComponentGatewayAPI, Mode: "auto", Action: "install", Reason: "no Gateway API CRDs"},
			{Component: ComponentCertManager, Mode: "auto", Action: "use", Reason: "cert-manager 1.16 is ready"},
		},
		Gateway: GatewayChoice{Class: "eg", ControllerName: "gateway.envoyproxy.io/gatewayclass-controller", BoxInstalls: true},
		Skips:   []string{"/etc/rancher/k3s/k3s.yaml: not reachable"},
		Notes:   []string{"docker is installed"},
		Reason:  "using external at /home/example/.kube/config (k3s)",
	}
	screen, log := report(res)
	if want := "✓ k3s, 1 node · box uses it as it is\n"; screen != want {
		t.Fatalf("screen = %q, want %q", screen, want)
	}
	// The log looks like the screen, then the detail (rule 12).
	if !strings.HasPrefix(log, "✓ k3s, 1 node · box uses it as it is\ndecision: use-cluster\n") {
		t.Fatalf("log does not open with the row and the decision:\n%s", log)
	}
	for _, line := range []string{
		"kubeconfig: /home/example/.kube/config",
		"nodes: 1",
		"skipped: /etc/rancher/k3s/k3s.yaml: not reachable",
		"note: docker is installed",
		"platform gateway-api: auto -> install (no Gateway API CRDs)",
		"gateway class: eg (gateway.envoyproxy.io/gatewayclass-controller), installed by box",
	} {
		if !strings.Contains(log, line+"\n") {
			t.Fatalf("log is missing %q:\n%s", line, log)
		}
	}
}

// A warning is `!`: a node on a tunnel is the operator's to fix.
func TestAWarningIsTheOneThingOnTheScreenBesidesTheRow(t *testing.T) {
	res := Result{
		Action:       ActionUseCluster,
		Distribution: DistK3s,
		Nodes:        2,
		Warnings:     []string{"the node registered 100.64.0.7, the address on tun0, a tunnel"},
	}
	screen, _ := report(res)
	want := "✓ k3s, 2 nodes · box uses it as it is\n" +
		"! the node registered 100.64.0.7, the address on tun0, a tunnel\n"
	if screen != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", screen, want)
	}
}

// A clean host: the row says what box will do, and the installer's rows follow.
func TestACleanHostIsOneRow(t *testing.T) {
	screen, _ := report(Result{Action: ActionInstall, Source: SourceK0sManaged, Distribution: DistK0s})
	if want := "✓ clean host · box installs k0s\n"; screen != want {
		t.Fatalf("screen = %q, want %q", screen, want)
	}
}

// A refusal is a ✗ row with the reason under it: one fact and its consequences.
func TestARefusalIsOneCrossRow(t *testing.T) {
	res := Result{
		Action: ActionStop,
		Reason: "this host already runs k3s, and box will not replace it:\n  k3s.service is running\nrun `box bootstrap --kubeconfig` with the cluster you want box to use.",
		Skips:  []string{"/etc/rancher/k3s/k3s.yaml: not reachable"},
		Notes:  []string{"a containerd daemon is running"},
	}
	screen, log := report(res)
	want := "✗ this host already runs k3s, and box will not replace it:\n" +
		"  k3s.service is running\n" +
		"  run `box bootstrap --kubeconfig` with the cluster you want box to use.\n"
	if screen != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", screen, want)
	}
	for _, line := range []string{"skipped: /etc/rancher/k3s/k3s.yaml: not reachable", "note: a containerd daemon is running"} {
		if !strings.Contains(log, line+"\n") {
			t.Fatalf("log is missing %q:\n%s", line, log)
		}
	}
}

// The one refusal the design page draws byte for byte: what is in the way, the
// command, what next. The why is the log's.
func TestTheK0sRefusalIsTheFactAndTheFix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k0s.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: k0s.k0sproject.io/v1beta1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := k0sConfigConflict(path, "box's own bytes")
	want := path + " exists\n" +
		"$ sudo mv " + path + " " + path + ".before-box\n" +
		"then run this again"
	if got != want {
		t.Fatalf("refusal:\n%s\nwant:\n%s", got, want)
	}
	screen, _ := report(Result{Action: ActionStop, Reason: got})
	wantScreen := "✗ " + path + " exists\n" +
		"  $ sudo mv " + path + " " + path + ".before-box\n" +
		"  then run this again\n"
	if screen != wantScreen {
		t.Fatalf("screen:\n%s\nwant:\n%s", screen, wantScreen)
	}
}
