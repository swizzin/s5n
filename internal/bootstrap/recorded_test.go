package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordedCluster stages a re-run: a record on disk, the kubeconfig it names,
// a probe with the cluster's current answers, and a trace of what was touched.
func recordedCluster(rec *Record, prober *fakeProber) (*Decision, *fakeSource, *fakeRecords, *fakeInstaller, *[]string) {
	trace := &[]string{}
	src := &fakeSource{
		files: map[string][]byte{rec.Kubeconfig: []byte("kubeconfig")},
		log:   trace,
	}
	prober.log = trace
	recs := &fakeRecords{rec: rec, log: trace}
	inst := &fakeInstaller{log: trace}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)
	return d, src, recs, inst, trace
}

func externalRecord(kubeconfig string) *Record {
	return &Record{
		Source:       SourceExternal,
		Kubeconfig:   kubeconfig,
		Distribution: DistK3s,
		Nodes:        1,
		Platform:     DefaultPlatform(),
		GatewayClass: EnvoyGatewayClass,
	}
}

func actions(res Result) map[PlatformComponent]string {
	out := map[PlatformComponent]string{}
	for _, c := range res.Platform {
		out[c.Component] = c.Action
	}
	return out
}

// A record written by a run whose platform apply then failed must not make the
// next run plan nothing: the cluster is probed again, and its missing pieces
// are installs. This is the "unknown (unexpected probe outcome)" re-run.
func TestBootstrapRecordRepairsMissingPlatform(t *testing.T) {
	const kubeconfig = "/etc/rancher/k3s/k3s.yaml"
	prober := &fakeProber{
		version: Version{GitVersion: "v1.37.2+k3s1"},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomeAbsent,
			LoadBalancer: OutcomeNotProbed,
			Gateway:      OutcomeAbsent,
			CertManager:  OutcomeAbsent,
		},
	}
	d, src, recs, inst, _ := recordedCluster(externalRecord(kubeconfig), prober)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseCluster || res.Source != SourceExternal {
		t.Fatalf("action=%q source=%q, want use-cluster external; reason: %s", res.Action, res.Source, res.Reason)
	}
	want := map[PlatformComponent]string{
		ComponentGatewayAPI:   "install",
		ComponentLoadBalancer: "probe",
		ComponentGateway:      "install",
		ComponentCertManager:  "install",
	}
	got := actions(res)
	for comp, action := range want {
		if got[comp] != action {
			t.Errorf("%s action = %q, want %q (platform = %+v)", comp, got[comp], action, res.Platform)
		}
	}
	if !res.Gateway.BoxInstalls || res.Gateway.Class != EnvoyGatewayClass {
		t.Errorf("gateway = %+v, want box installing %s again", res.Gateway, EnvoyGatewayClass)
	}
	if len(recs.saved) != 0 {
		t.Errorf("a re-run must not rewrite the record: %+v", recs.saved)
	}
	if len(inst.plans) != 0 {
		t.Errorf("a re-run of a recorded cluster must never reach the installer")
	}
	if src.candidatesN != 0 {
		t.Errorf("a record must still short-circuit the candidate scan")
	}
	for _, call := range []string{"version", "admin", "describe", "platform"} {
		if prober.calls[call] != 1 {
			t.Errorf("%s probed %d times, want once: %v", call, prober.calls[call], prober.calls)
		}
	}
	out := FormatResult(res)
	if strings.Contains(out, "unknown") {
		t.Errorf("a re-probed record must not report unknown components:\n%s", out)
	}
	if !strings.Contains(out, "gateway-api: auto -> install") {
		t.Errorf("the decision does not plan the missing Gateway API CRDs:\n%s", out)
	}
}

// Everything the record says is there, and still is: a second bootstrap plans
// no installs and writes no record.
func TestBootstrapRecordIdempotent(t *testing.T) {
	t.Run("box's own implementation", func(t *testing.T) {
		const kubeconfig = "/etc/rancher/k3s/k3s.yaml"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 1},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
				Classes:      []GatewayClass{{Name: EnvoyGatewayClass, ControllerName: EnvoyGatewayController, Accepted: true, ManagedByBox: true}},
			},
		}
		d, _, recs, inst, _ := recordedCluster(externalRecord(kubeconfig), prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionUseCluster {
			t.Fatalf("action = %q, want use-cluster; reason: %s", res.Action, res.Reason)
		}
		for _, c := range res.Platform {
			if c.Action == "install" {
				t.Errorf("%s would be installed on a cluster that already has it: %+v", c.Component, c)
			}
		}
		if res.Gateway.Class != EnvoyGatewayClass || res.Gateway.BoxInstalls {
			t.Errorf("gateway = %+v, want the recorded %s used, not reinstalled", res.Gateway, EnvoyGatewayClass)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("an idempotent re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
		out := FormatResult(res)
		if strings.Contains(out, "unknown") || strings.Contains(out, "-> install") {
			t.Errorf("an idempotent re-run planned work:\n%s", out)
		}
	})

	t.Run("a second Envoy class the operator chose", func(t *testing.T) {
		const kubeconfig = "/home/someone/.kube/config"
		rec := externalRecord(kubeconfig)
		rec.GatewayClass = "envoy"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 1},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
				Classes: []GatewayClass{
					{Name: "envoy", ControllerName: EnvoyGatewayController, Accepted: true},
					{Name: "eg", ControllerName: EnvoyGatewayController, Accepted: true},
				},
			},
		}
		d, _, recs, _, _ := recordedCluster(rec, prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionUseCluster {
			t.Fatalf("action = %q, want use-cluster; reason: %s", res.Action, res.Reason)
		}
		if res.Gateway.Class != "envoy" {
			t.Errorf("gateway = %+v, want the recorded class kept over the list", res.Gateway)
		}
		if len(recs.saved) != 0 {
			t.Errorf("a re-run must not rewrite the record")
		}
	})
}

// A recorded cluster that now fails a gate stops with the fresh-cluster
// wording, before the platform probe and before any write. The trace, not just
// the message, says so.
func TestBootstrapRecordGateFiresBeforeAnyWrite(t *testing.T) {
	t.Run("the cluster went multi-node", func(t *testing.T) {
		const kubeconfig = "/etc/rancher/k3s/k3s.yaml"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 3},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
			},
		}
		d, _, recs, inst, trace := recordedCluster(externalRecord(kubeconfig), prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop {
			t.Fatalf("action = %q, want stop", res.Action)
		}
		if !strings.HasPrefix(res.Reason, "recorded external cluster at "+kubeconfig+": 3 nodes: ") {
			t.Errorf("reason = %q, want the fresh-cluster node refusal", res.Reason)
		}
		want := []string{
			"read " + kubeconfig,
			"prober new",
			"prober version",
			"prober admin",
			"prober describe",
		}
		if strings.Join(*trace, "|") != strings.Join(want, "|") {
			t.Errorf("call trace = %v, want only %v: the gate must fire before the platform probe and every write", *trace, want)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("a refused re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
	})

	t.Run("the recorded class vanished while another implementation serves", func(t *testing.T) {
		const kubeconfig = "/etc/rancher/k3s/k3s.yaml"
		rec := externalRecord(kubeconfig)
		rec.GatewayClass = "nginx"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 1},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
				Classes: []GatewayClass{
					{Name: "traefik", ControllerName: "example.com/traefik", Accepted: true},
				},
			},
		}
		d, _, recs, inst, _ := recordedCluster(rec, prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop {
			t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
		}
		if !strings.Contains(res.Reason, `the recorded GatewayClass "nginx" is gone from the cluster (accepted: traefik)`) {
			t.Errorf("reason = %q, want the vanished recorded class named", res.Reason)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("a refused re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
	})

	t.Run("the recorded class belongs to another implementation", func(t *testing.T) {
		const kubeconfig = "/home/someone/.kube/config"
		rec := externalRecord(kubeconfig)
		rec.GatewayClass = "nginx"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 1},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
				Classes: []GatewayClass{
					{Name: "nginx", ControllerName: nginxController, Accepted: true},
				},
			},
		}
		d, _, recs, inst, _ := recordedCluster(rec, prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop {
			t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
		}
		for _, want := range []string{`the recorded GatewayClass "nginx"`, nginxController, "is not Envoy Gateway", RecordPath} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("reason = %q, want it to name %q", res.Reason, want)
			}
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("a refused re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
	})

	t.Run("the recorded class is no longer Accepted", func(t *testing.T) {
		const kubeconfig = "/etc/rancher/k3s/k3s.yaml"
		rec := externalRecord(kubeconfig)
		rec.GatewayClass = "nginx"
		prober := &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: Cluster{Nodes: 1},
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomeNotProbed,
				Gateway:      OutcomeAbsent,
				CertManager:  OutcomePresent,
				Classes: []GatewayClass{
					{Name: "nginx", ControllerName: "example.com/nginx", Accepted: false, AcceptedReason: "Waiting for controller"},
				},
			},
		}
		d, _, recs, inst, _ := recordedCluster(rec, prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop {
			t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
		}
		if !strings.Contains(res.Reason, `the recorded GatewayClass "nginx" is not Accepted (Waiting for controller)`) {
			t.Errorf("reason = %q, want the controller's reason", res.Reason)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("a refused re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
	})
}

// ---- explicit kubeconfig against an existing record ----

// The e2e and any documented re-run pass --kubeconfig on every run. When that
// path is the one the record names, it is the recorded re-run: the record's
// decisions (including a k0s-managed lifecycle) are kept and nothing is
// rewritten. filepath.Clean decides, so a harmless spelling difference is not
// a different cluster.
func TestBootstrapExplicitMatchesRecordUsesRecord(t *testing.T) {
	const kubeconfig = K0sAdminConfig
	rec := &Record{
		Source:       SourceK0sManaged,
		Kubeconfig:   kubeconfig,
		Distribution: DistK0s,
		Nodes:        1,
		Platform:     DefaultPlatform(),
		GatewayClass: EnvoyGatewayClass,
	}
	prober := &fakeProber{
		version: Version{GitVersion: K0sVersion},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomePresent,
			LoadBalancer: OutcomeNotProbed,
			Gateway:      OutcomePresent,
			CertManager:  OutcomePresent,
			Classes:      []GatewayClass{{Name: EnvoyGatewayClass, ControllerName: EnvoyGatewayController, Accepted: true, ManagedByBox: true}},
		},
	}
	d, _, recs, inst, _ := recordedCluster(rec, prober)

	// The same file, spelled differently: filepath.Clean makes it the record.
	res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: "/var/lib/k0s/pki/./admin.conf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseManaged || res.Source != SourceK0sManaged {
		t.Fatalf("action=%q source=%q, want the recorded use-managed cluster; reason: %s", res.Action, res.Source, res.Reason)
	}
	if res.Kubeconfig != kubeconfig {
		t.Errorf("kubeconfig = %q, want the record's %q", res.Kubeconfig, kubeconfig)
	}
	if len(recs.saved) != 0 {
		t.Errorf("the recorded re-run rewrote the record: %+v", recs.saved)
	}
	if len(inst.plans) != 0 {
		t.Errorf("the recorded re-run reached the installer")
	}
	if prober.calls["platform"] != 1 {
		t.Errorf("the record was not re-probed: %v", prober.calls)
	}
}

// An explicit kubeconfig naming a different file than the record is a move to
// another cluster. box refuses it before any probe or write and names both
// paths, the record's source, and the file to remove deliberately.
func TestBootstrapExplicitDifferentRecordStops(t *testing.T) {
	const recorded = K0sAdminConfig
	const explicit = "/root/.kube/config"
	prober := &fakeProber{
		version: Version{GitVersion: K0sVersion},
		cluster: Cluster{Nodes: 1},
	}
	d, src, recs, inst, trace := recordedCluster(externalRecord(recorded), prober)
	src.files[explicit] = []byte("kubeconfig") // a fall-through would find it

	res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: explicit})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	for _, want := range []string{explicit, recorded, RecordPath, string(SourceExternal), "Unset KUBECONFIG", "drop --kubeconfig", "remove"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason = %q, want it to name %q", res.Reason, want)
		}
	}
	// The gate fires before anything is read from either kubeconfig, probed,
	// installed or written.
	if len(src.reads) != 0 {
		t.Errorf("read %v before refusing; the gate must fire before any probe", src.reads)
	}
	if len(prober.calls) != 0 {
		t.Errorf("probed before refusing: %v", prober.calls)
	}
	if len(recs.saved) != 0 || len(inst.plans) != 0 {
		t.Errorf("a refused explicit path wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
	}
	if got := strings.Join(*trace, "|"); strings.Contains(got, "install k0s") || strings.Contains(got, "save record") {
		t.Errorf("the refusal reached a write: %v", *trace)
	}
}

// A record whose cluster no longer answers stops naming the kubeconfig the
// record holds, and never reaches the installer.
func TestBootstrapRecordUnreachableStops(t *testing.T) {
	const kubeconfig = "/etc/rancher/k3s/k3s.yaml"

	t.Run("the cluster does not answer", func(t *testing.T) {
		prober := &fakeProber{versionErr: errors.New("connection refused")}
		d, _, recs, inst, trace := recordedCluster(externalRecord(kubeconfig), prober)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "recorded external cluster at " + kubeconfig + " did not answer /version: connection refused"
		if res.Action != ActionStop || res.Reason != want {
			t.Fatalf("action=%q reason=%q, want stop with %q", res.Action, res.Reason, want)
		}
		if prober.calls["platform"] != 0 {
			t.Errorf("an unreachable record must not be capability-probed: %v", prober.calls)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("an unreachable record wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
		if got := strings.Join(*trace, "|"); strings.Contains(got, "save record") || strings.Contains(got, "install k0s") {
			t.Errorf("an unreachable record reached a write: %v", *trace)
		}
	})

	t.Run("the kubeconfig the record names is gone", func(t *testing.T) {
		prober := &fakeProber{}
		d, src, recs, inst, _ := recordedCluster(externalRecord(kubeconfig), prober)
		src.files = map[string][]byte{}

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop || !strings.Contains(res.Reason, kubeconfig) {
			t.Fatalf("action=%q reason=%q, want a stop naming %s", res.Action, res.Reason, kubeconfig)
		}
		if prober.calls["new"] != 0 {
			t.Errorf("a missing kubeconfig must not be probed: %v", prober.calls)
		}
		if len(recs.saved) != 0 || len(inst.plans) != 0 {
			t.Errorf("a refused re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
		}
	})
}

// A k0s-managed record is re-probed like any other; only its lifecycle stays
// box update's. Its platform layer is the one box installed, so a lost piece is
// an install on the next run rather than an unknown.
func TestBootstrapManagedRecordReprobes(t *testing.T) {
	const kubeconfig = K0sAdminConfig
	rec := &Record{
		Source:       SourceK0sManaged,
		Kubeconfig:   kubeconfig,
		Distribution: DistK0s,
		Nodes:        1,
		Platform:     DefaultPlatform(),
		GatewayClass: EnvoyGatewayClass,
	}
	prober := &fakeProber{
		version: Version{GitVersion: K0sVersion},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomeAbsent,
			LoadBalancer: OutcomeNotProbed,
			Gateway:      OutcomeAbsent,
			CertManager:  OutcomeAbsent,
		},
	}
	d, _, recs, inst, _ := recordedCluster(rec, prober)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseManaged {
		t.Fatalf("action = %q, want use-managed; reason: %s", res.Action, res.Reason)
	}
	if !strings.Contains(res.Reason, "box update") || !strings.Contains(res.Reason, "platform re-probed") {
		t.Errorf("reason = %q, want the lifecycle owner and the re-probe named", res.Reason)
	}
	if actions(res)[ComponentGateway] != "install" {
		t.Errorf("the managed box's missing implementation is not planned: %+v", res.Platform)
	}
	if len(recs.saved) != 0 || len(inst.plans) != 0 {
		t.Errorf("a managed re-run wrote: %d records, %d installs", len(recs.saved), len(inst.plans))
	}

	// The same record when k0s is not running: a stop that names the record,
	// never a fall-through to installing k0s over it.
	prober.versionErr = errors.New("connection refused")
	res, err = d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop || !strings.Contains(res.Reason, kubeconfig) {
		t.Fatalf("action=%q reason=%q, want a stop naming %s", res.Action, res.Reason, kubeconfig)
	}
	if len(inst.plans) != 0 {
		t.Errorf("an unreachable managed record reached the installer")
	}
}
