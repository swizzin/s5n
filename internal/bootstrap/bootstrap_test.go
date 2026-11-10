package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// ---- fakes ----

type fakeSource struct {
	candidates  []Candidate
	files       map[string][]byte
	readErr     map[string]error
	reads       []string
	candidatesN int
	log         *[]string
}

func (f *fakeSource) Candidates() []Candidate {
	f.candidatesN++
	return f.candidates
}

func (f *fakeSource) Read(path string) ([]byte, error) {
	if f.log != nil {
		*f.log = append(*f.log, "read "+path)
	}
	f.reads = append(f.reads, path)
	if err, ok := f.readErr[path]; ok {
		return nil, err
	}
	b, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

type fakeProber struct {
	version     Version
	versionErr  error
	adminErr    error
	cluster     Cluster
	describeErr error
	platform    PlatformProbe
	platformErr error
	factoryErr  error

	calls map[string]int
	log   *[]string
}

func newFakeProber(p *fakeProber) func([]byte) (Prober, error) {
	return func([]byte) (Prober, error) {
		if p.calls == nil {
			p.calls = map[string]int{}
		}
		p.calls["new"]++
		if p.log != nil {
			*p.log = append(*p.log, "prober new")
		}
		if p.factoryErr != nil {
			return nil, p.factoryErr
		}
		return p, nil
	}
}

func (f *fakeProber) bump(name string) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
	if f.log != nil {
		*f.log = append(*f.log, "prober "+name)
	}
}

func (f *fakeProber) Version(context.Context) (Version, error) {
	f.bump("version")
	return f.version, f.versionErr
}

func (f *fakeProber) CanAdmin(context.Context) error {
	f.bump("admin")
	return f.adminErr
}

func (f *fakeProber) Describe(context.Context) (Cluster, error) {
	f.bump("describe")
	return f.cluster, f.describeErr
}

func (f *fakeProber) Platform(context.Context) (PlatformProbe, error) {
	f.bump("platform")
	return f.platform, f.platformErr
}

type fakeHost struct {
	findings []Finding
	called   int
}

func (f *fakeHost) Present() []Finding {
	f.called++
	return f.findings
}

type fakeRecords struct {
	rec     *Record
	path    string
	loadErr error
	saved   []*Record
	saveErr error
	log     *[]string
}

func (f *fakeRecords) Load() (*Record, error) { return f.rec, f.loadErr }

func (f *fakeRecords) RecordPath() string {
	if f.path != "" {
		return f.path
	}
	return RecordPath
}

func (f *fakeRecords) Save(r *Record) error {
	if f.log != nil {
		*f.log = append(*f.log, "save record")
	}
	f.saved = append(f.saved, r)
	return f.saveErr
}

type fakeInstaller struct {
	plans []InstallPlan
	err   error
	log   *[]string
}

func (f *fakeInstaller) Install(_ context.Context, p InstallPlan) error {
	if f.log != nil {
		*f.log = append(*f.log, "install k0s")
	}
	f.plans = append(f.plans, p)
	return f.err
}

func newDecision(src *fakeSource, prober *fakeProber, host *fakeHost, recs *fakeRecords, inst *fakeInstaller) *Decision {
	d := &Decision{
		Source:  src,
		Host:    host,
		Records: recs,
		Install: inst,
	}
	if prober != nil {
		d.NewProber = newFakeProber(prober)
	} else {
		d.NewProber = func([]byte) (Prober, error) {
			return nil, errors.New("prober factory not wired")
		}
	}
	return d
}

// ---- explicit cluster: use it or fail, never fall back ----

func TestBootstrapExplicitBrokenNeverFallsBack(t *testing.T) {
	const path = "/explicit.yaml"
	cases := []struct {
		name       string
		setup      func(*fakeSource, *fakeProber, *fakeHost)
		wantReason string
	}{
		{
			name: "kubeconfig missing",
			setup: func(src *fakeSource, _ *fakeProber, _ *fakeHost) {
				// no file registered -> Read returns os.ErrNotExist
			},
			wantReason: "explicit kubeconfig",
		},
		{
			name: "prober cannot parse it",
			setup: func(src *fakeSource, p *fakeProber, _ *fakeHost) {
				src.files[path] = []byte("kubeconfig")
				p.factoryErr = errors.New("cannot parse kubeconfig")
			},
			wantReason: "explicit cluster",
		},
		{
			name: "version does not answer",
			setup: func(src *fakeSource, p *fakeProber, _ *fakeHost) {
				src.files[path] = []byte("kubeconfig")
				p.versionErr = errors.New("connection refused")
			},
			wantReason: "did not answer /version",
		},
		{
			name: "identity cannot manage it",
			setup: func(src *fakeSource, p *fakeProber, _ *fakeHost) {
				src.files[path] = []byte("kubeconfig")
				p.version = Version{GitVersion: "v1.37.2+k0s.1"}
				p.adminErr = errors.New("identity cannot create namespaces")
			},
			wantReason: "cannot manage it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{
				files:   map[string][]byte{},
				readErr: map[string]error{},
			}
			prober := &fakeProber{}
			host := &fakeHost{}
			recs := &fakeRecords{}
			inst := &fakeInstaller{}
			tc.setup(src, prober, host)
			d := newDecision(src, prober, host, recs, inst)

			res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: path})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Action != ActionStop {
				t.Fatalf("action = %q, want stop", res.Action)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			// never fell back to host probing or installation
			if host.called != 0 {
				t.Errorf("host probing was reached (%d calls); explicit cluster must never fall back", host.called)
			}
			if src.candidatesN != 0 {
				t.Errorf("candidate enumeration was reached; explicit cluster must never fall back")
			}
			if len(inst.plans) != 0 {
				t.Errorf("installer was reached; explicit cluster must never fall back")
			}
		})
	}
}

func TestBootstrapExplicitUsesCluster(t *testing.T) {
	const path = "/explicit.yaml"
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	prober := &fakeProber{
		version: Version{GitVersion: "v1.37.2+k3s1"},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomePresent,
			LoadBalancer: OutcomePresent,
			Gateway:      OutcomePresent,
			CertManager:  OutcomePresent,
			Classes:      []GatewayClass{{Name: "eg", ControllerName: "gateway.envoyproxy.io/gatewayclass-controller", Accepted: true, ManagedByBox: true}},
		},
	}
	host := &fakeHost{}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, host, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; reason: %s", res.Action, res.Reason)
	}
	if res.Source != SourceExternal {
		t.Errorf("source = %q, want external", res.Source)
	}
	if res.Distribution != DistK3s {
		t.Errorf("distribution = %q, want k3s", res.Distribution)
	}
	if len(recs.saved) != 1 || recs.saved[0].Source != SourceExternal {
		t.Errorf("record not saved as external")
	}
}

// ---- candidate: /version answers but SSAR denies -> not usable ----

func TestBootstrapCandidateSSARDeniedNotUsable(t *testing.T) {
	// A user-owned kubeconfig is often a remote cluster: unusable, it is a skip
	// rather than evidence that this host holds a broken installation.
	const candidate = "/home/example/.kube/config"

	t.Run("falls through to clean host", func(t *testing.T) {
		src := &fakeSource{
			candidates: []Candidate{{Path: candidate}},
			files:      map[string][]byte{candidate: []byte("kubeconfig")},
		}
		prober := &fakeProber{
			version:  Version{GitVersion: "v1.37.2+k3s1"},
			adminErr: errors.New("identity cannot create namespaces, create clusterroles"),
			cluster:  Cluster{Nodes: 1},
			platform: PlatformProbe{},
		}
		host := &fakeHost{}
		recs := &fakeRecords{}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{DryRun: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionInstall {
			t.Fatalf("action = %q, want install (candidate not usable, host clean)", res.Action)
		}
		if len(res.Skips) != 1 || !strings.Contains(res.Skips[0], "cannot manage") && !strings.Contains(res.Skips[0], "privileges") {
			t.Fatalf("skips = %v, want the SSAR denial recorded", res.Skips)
		}
		if prober.calls["platform"] != 0 || prober.calls["describe"] != 0 {
			t.Errorf("a not-usable candidate must not be described or probed")
		}
	})

	t.Run("stops when the host shows an existing cluster", func(t *testing.T) {
		src := &fakeSource{
			candidates: []Candidate{{Path: candidate}},
			files:      map[string][]byte{candidate: []byte("kubeconfig")},
		}
		prober := &fakeProber{
			version:  Version{GitVersion: "v1.37.2+k3s1"},
			adminErr: errors.New("forbidden"),
			cluster:  Cluster{Nodes: 1},
		}
		host := &fakeHost{findings: []Finding{{Kind: "process", Name: "kubelet"}}}
		recs := &fakeRecords{}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop {
			t.Fatalf("action = %q, want stop", res.Action)
		}
		if !strings.Contains(res.Reason, "kubelet") {
			t.Errorf("reason should name the finding: %s", res.Reason)
		}
	})
}

// ---- a dead distribution kubeconfig blocks; a dead user one does not ----

// A distribution writes its admin kubeconfig only after it installed a
// cluster, so one that exists but no longer answers is a broken installation.
// Installing k0s on top would be a second cluster over the first. The finding
// names it alongside any host evidence, and the skip line keeps the reason.
func TestBootstrapDeadDistributionKubeconfigStops(t *testing.T) {
	const candidate = "/etc/kubernetes/admin.conf"
	trace := &[]string{}
	src := &fakeSource{
		candidates: []Candidate{{Path: candidate, DistributionOwned: true}},
		files:      map[string][]byte{candidate: []byte("kubeconfig")},
		log:        trace,
	}
	prober := &fakeProber{versionErr: errors.New("connection refused"), log: trace}
	recs := &fakeRecords{log: trace}
	inst := &fakeInstaller{log: trace}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
	}
	if !strings.Contains(res.Reason, "kubeconfig "+candidate) {
		t.Errorf("reason = %q, want the dead distribution kubeconfig listed as a finding", res.Reason)
	}
	if !strings.Contains(res.Reason, "box will not install a second cluster over a broken first one") {
		t.Errorf("reason = %q, want the broken-first-cluster framing", res.Reason)
	}
	if len(res.Skips) != 1 || !strings.Contains(res.Skips[0], candidate) || !strings.Contains(res.Skips[0], "connection refused") {
		t.Errorf("skips = %v, want the skip line kept with its reason", res.Skips)
	}
	// No gate after a side effect: nothing is capability-probed, installed or
	// recorded.
	if prober.calls["platform"] != 0 {
		t.Errorf("platform probed before the refusal: %v", prober.calls)
	}
	if len(inst.plans) != 0 || len(recs.saved) != 0 {
		t.Errorf("a refused host was installed onto or recorded: %d plans, %d records", len(inst.plans), len(recs.saved))
	}
	if got := strings.Join(*trace, "|"); strings.Contains(got, "install k0s") || strings.Contains(got, "save record") {
		t.Errorf("the refusal reached a write: %v", *trace)
	}
}

// ~/.kube/config is not distribution-owned: it is often a remote cluster, so
// an unusable one stays a printed skip and the clean-host install goes ahead.
func TestBootstrapDeadUserKubeconfigInstalls(t *testing.T) {
	const candidate = "/home/example/.kube/config"
	src := &fakeSource{
		candidates: []Candidate{{Path: candidate, DistributionOwned: false}},
		files:      map[string][]byte{candidate: []byte("kubeconfig")},
	}
	prober := &fakeProber{versionErr: errors.New("connection refused")}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall {
		t.Fatalf("action = %q, want install; reason: %s", res.Action, res.Reason)
	}
	if len(res.Skips) != 1 || !strings.Contains(res.Skips[0], candidate) {
		t.Errorf("skips = %v, want the unusable user kubeconfig printed", res.Skips)
	}
	if len(inst.plans) != 1 {
		t.Errorf("installer plans = %v, want one clean-host plan", inst.plans)
	}
	if len(recs.saved) != 0 {
		t.Errorf("a dry run recorded a cluster it did not install")
	}
}

// A dry run refuses to probe a remote cluster (ErrRemoteCluster), even at a
// distribution path reached through a symlink or a copied file. That guard is
// not evidence of a broken install and must not block the plan.
func TestBootstrapRemoteCandidateSkipStillPlans(t *testing.T) {
	const candidate = "/etc/rancher/k3s/k3s.yaml"
	src := &fakeSource{
		candidates: []Candidate{{Path: candidate, DistributionOwned: true}},
		files:      map[string][]byte{candidate: []byte("kubeconfig")},
	}
	prober := &fakeProber{versionErr: fmt.Errorf("%w (server 203.0.113.10:6443)", ErrRemoteCluster)}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall {
		t.Fatalf("action = %q, want install; reason: %s", res.Action, res.Reason)
	}
	if len(res.Skips) != 1 || !strings.Contains(res.Skips[0], "skipped: remote cluster") {
		t.Errorf("skips = %v, want the remote skip printed", res.Skips)
	}
	if len(inst.plans) != 1 {
		t.Errorf("installer plans = %v, want one clean-host plan", inst.plans)
	}
}

// ---- present but unusable: stop with an explanation naming what was found ----

func TestBootstrapPresentButUnusableStops(t *testing.T) {
	src := &fakeSource{candidates: []Candidate{{Path: "/var/lib/k0s/pki/admin.conf", DistributionOwned: true}}}
	prober := &fakeProber{versionErr: errors.New("connection refused")}
	host := &fakeHost{findings: []Finding{
		{Kind: "process", Name: "kubelet"},
		{Kind: "unit", Name: "k0scontroller.service"},
		{Kind: "port", Name: "6443"},
	}}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, host, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	for _, want := range []string{"kubelet", "k0scontroller.service", "6443"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason should name %q: %s", want, res.Reason)
		}
	}
	if !strings.Contains(res.Reason, "will not install a second cluster") {
		t.Errorf("reason should explain it never installs over a broken cluster: %s", res.Reason)
	}
	if len(inst.plans) != 0 {
		t.Errorf("installer must not run when something present is unusable")
	}
}

// ---- a bare container runtime is not a cluster; it is an install target ----

func TestBootstrapContainerRuntimeNotACluster(t *testing.T) {
	cases := []struct {
		name       string
		findings   []Finding
		wantAction Action
		wantReason string   // substring, checked on stop
		wantNotes  []string // exact
	}{
		{
			name:       "docker only installs with a note",
			findings:   []Finding{{Kind: "process", Name: "dockerd"}},
			wantAction: ActionInstall,
			wantNotes:  []string{"dockerd running; not a cluster by itself"},
		},
		{
			name:       "containerd only installs",
			findings:   []Finding{{Kind: "process", Name: "containerd"}},
			wantAction: ActionInstall,
			wantNotes:  []string{"containerd running; not a cluster by itself"},
		},
		{
			name: "docker and containerd install with two notes",
			findings: []Finding{
				{Kind: "process", Name: "dockerd"},
				{Kind: "process", Name: "containerd"},
			},
			wantAction: ActionInstall,
			wantNotes:  []string{"dockerd running; not a cluster by itself", "containerd running; not a cluster by itself"},
		},
		{
			name: "dockerd with 6443 listening stops on the port",
			findings: []Finding{
				{Kind: "process", Name: "dockerd"},
				{Kind: "port", Name: "6443"},
			},
			wantAction: ActionStop,
			wantReason: "port 6443",
			wantNotes:  []string{"dockerd running; not a cluster by itself"},
		},
		{
			name:       "kubelet alone still stops",
			findings:   []Finding{{Kind: "process", Name: "kubelet"}},
			wantAction: ActionStop,
			wantReason: "process kubelet",
		},
		{
			name:       "k3s binary alone still stops",
			findings:   []Finding{{Kind: "binary", Name: "/usr/local/bin/k3s"}},
			wantAction: ActionStop,
			wantReason: "binary /usr/local/bin/k3s",
		},
		{
			name: "k0s unit still stops even with its containerd visible",
			findings: []Finding{
				{Kind: "process", Name: "containerd"},
				{Kind: "unit", Name: "k0scontroller.service"},
			},
			wantAction: ActionStop,
			wantReason: "unit k0scontroller.service",
			wantNotes:  []string{"containerd running; not a cluster by itself"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{}
			prober := &fakeProber{}
			host := &fakeHost{findings: tc.findings}
			recs := &fakeRecords{}
			inst := &fakeInstaller{}
			d := newDecision(src, prober, host, recs, inst)

			res, err := d.Bootstrap(context.Background(), Options{DryRun: true})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Action != tc.wantAction {
				t.Fatalf("action = %q, want %q; reason: %s", res.Action, tc.wantAction, res.Reason)
			}
			if len(tc.wantNotes) != len(res.Notes) {
				t.Fatalf("notes = %v, want %v", res.Notes, tc.wantNotes)
			}
			for i, want := range tc.wantNotes {
				if res.Notes[i] != want {
					t.Errorf("note[%d] = %q, want %q", i, res.Notes[i], want)
				}
			}

			switch tc.wantAction {
			case ActionInstall:
				if len(inst.plans) != 1 || inst.plans[0].Version != K0sVersion {
					t.Errorf("installer should get one plan for %s: %v", K0sVersion, inst.plans)
				}
				// The plan is all a dry run produces: the cluster it describes
				// does not exist yet, so nothing can be probed or recorded.
				if len(recs.saved) != 0 {
					t.Errorf("a dry run recorded a cluster it did not install")
				}
				out := FormatResult(res)
				for _, want := range tc.wantNotes {
					if !strings.Contains(out, "note: "+want) {
						t.Errorf("dry run should print %q:\n%s", "note: "+want, out)
					}
				}
			case ActionStop:
				if !strings.Contains(res.Reason, tc.wantReason) {
					t.Errorf("reason = %q, want it to name %q", res.Reason, tc.wantReason)
				}
				if !strings.Contains(res.Reason, "will not install a second cluster") {
					t.Errorf("reason should explain it never installs over a broken cluster: %s", res.Reason)
				}
				if len(inst.plans) != 0 {
					t.Errorf("installer must not run when something present is unusable")
				}
			}
		})
	}
}

// ---- existing record ---- (see recorded_test.go: a record is re-probed, not trusted)

// ---- clean host: install the cluster, then adopt and record it ----

func TestBootstrapCleanHostInstalls(t *testing.T) {
	trace := &[]string{}
	src := &fakeSource{
		candidates: []Candidate{},
		files:      map[string][]byte{K0sAdminConfig: []byte("kubeconfig")},
		log:        trace,
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
		log: trace,
	}
	host := &fakeHost{}
	recs := &fakeRecords{log: trace}
	inst := &fakeInstaller{log: trace}
	d := newDecision(src, prober, host, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall {
		t.Fatalf("action = %q, want install; reason: %s", res.Action, res.Reason)
	}
	if res.Source != SourceK0sManaged {
		t.Errorf("source = %q, want k0s-managed", res.Source)
	}
	if res.Kubeconfig != K0sAdminConfig {
		t.Errorf("kubeconfig = %q, want %q", res.Kubeconfig, K0sAdminConfig)
	}
	if len(inst.plans) != 1 || inst.plans[0].Version != K0sVersion || !inst.plans[0].CleanHost {
		t.Fatalf("installer should get one clean-host plan for %s: %+v", K0sVersion, inst.plans)
	}
	if inst.plans[0].SHA256 != k0sSHA256[inst.plans[0].Arch] {
		t.Errorf("plan carries no pinned checksum: %+v", inst.plans[0])
	}

	// The cluster box just started is probed like any other, so the platform
	// plan that follows is a real decision rather than a default: a fresh k0s
	// has no GatewayClass, so box installs Envoy Gateway and its own class.
	if len(res.Platform) == 0 {
		t.Fatal("a freshly installed cluster must be platform-probed before it is recorded")
	}
	if res.Gateway.Class != EnvoyGatewayClass || !res.Gateway.BoxInstalls {
		t.Errorf("gateway = %+v, want box's own %s on a bare cluster", res.Gateway, EnvoyGatewayClass)
	}
	if len(recs.saved) != 1 || recs.saved[0].Source != SourceK0sManaged {
		t.Fatalf("clean install must record one k0s-managed cluster: %+v", recs.saved)
	}
	rec := recs.saved[0]
	if rec.Kubeconfig != K0sAdminConfig || rec.GatewayClass != EnvoyGatewayClass {
		t.Errorf("recorded %+v, want k0s's admin.conf and class %s", rec, EnvoyGatewayClass)
	}
	if rec.Platform.GatewayAPI != ModeAuto || rec.Platform.LoadBalancer != ModeAuto {
		t.Errorf("recorded platform = %+v, want the configured modes", rec.Platform)
	}
	// k0s hands out no addresses, so a cluster box just started gets MetalLB
	// planned outright rather than after a wait that can only say no.
	for _, c := range res.Platform {
		if c.Component == ComponentLoadBalancer && c.Action != "install" {
			t.Errorf("loadbalancer on a fresh k0s = %+v, want install without the question", c)
		}
	}

	// The order is the contract: nothing is read from an API that does not
	// exist, and the record is written only after the cluster answers.
	want := []string{
		"install k0s",
		"read " + K0sAdminConfig,
		"prober new",
		"prober version",
		"prober admin",
		"prober describe",
		"prober platform",
		"save record",
	}
	if got := strings.Join(*trace, "|"); got != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", *trace, want)
	}
}

// A cluster box installed but cannot reach is not recorded as one box manages.
func TestBootstrapCleanHostInstallUnreachableStops(t *testing.T) {
	src := &fakeSource{files: map[string][]byte{K0sAdminConfig: []byte("kubeconfig")}}
	prober := &fakeProber{versionErr: errors.New("connection refused")}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop || !strings.Contains(res.Reason, "connection refused") {
		t.Fatalf("action=%q reason=%q, want stop naming the unreachable API", res.Action, res.Reason)
	}
	if len(recs.saved) != 0 {
		t.Errorf("an unreachable fresh cluster must not be recorded")
	}
}

// A failed install stops the run: nothing is probed, nothing is recorded.
func TestBootstrapCleanHostInstallFailsStops(t *testing.T) {
	src := &fakeSource{}
	prober := &fakeProber{}
	recs := &fakeRecords{}
	inst := &fakeInstaller{err: errors.New("disk full")}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop || !strings.Contains(res.Reason, "disk full") {
		t.Fatalf("action=%q reason=%q, want stop naming the failure", res.Action, res.Reason)
	}
	if prober.calls["new"] != 0 {
		t.Errorf("a failed install must not build a prober: %v", prober.calls)
	}
	if len(recs.saved) != 0 {
		t.Errorf("a failed install must not be recorded")
	}
}

// A dry run prints the plan and stops: there is no cluster to adopt, and
// nothing may be probed or written.
func TestBootstrapCleanHostDryRunPlansOnly(t *testing.T) {
	src := &fakeSource{}
	prober := &fakeProber{}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, &fakeHost{}, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall || !strings.Contains(res.Reason, "dry run") {
		t.Fatalf("action=%q reason=%q, want an install plan only", res.Action, res.Reason)
	}
	if len(src.reads) != 0 {
		t.Errorf("a dry run read %v; it must not touch the cluster it did not create", src.reads)
	}
	if prober.calls["new"] != 0 || len(recs.saved) != 0 {
		t.Errorf("a dry run probed or recorded something: %v %v", prober.calls, recs.saved)
	}
	if len(inst.plans) != 1 || !inst.plans[0].CleanHost {
		t.Errorf("a dry run still asks the (dry-run) installer for the plan: %+v", inst.plans)
	}
}

// A usable-looking kubeconfig that was skipped must be printed on the install
// path too: silence reads as "there was nothing there", which is exactly what a
// remote cluster's ~/.kube/config must not look like.
func TestFormatResultInstallPrintsSkips(t *testing.T) {
	res := Result{
		Action:       ActionInstall,
		Source:       SourceK0sManaged,
		Kubeconfig:   K0sAdminConfig,
		Distribution: DistK0s,
		Skips:        []string{"/home/example/.kube/config: skipped: remote cluster"},
		Reason:       "clean host: k0s install planned",
	}
	out := FormatResult(res)
	if !strings.Contains(out, "skipped candidates:") || !strings.Contains(out, "skipped: remote cluster") {
		t.Fatalf("install output must print the skips:\n%s", out)
	}
}

// ---- the record read is a fault, not a clean host ----

func TestBootstrapRecordLoadError(t *testing.T) {
	src := &fakeSource{}
	prober := &fakeProber{}
	host := &fakeHost{}
	recs := &fakeRecords{loadErr: errors.New("permission denied")}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, host, recs, inst)

	_, err := d.Bootstrap(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "read cluster record") {
		t.Fatalf("err = %v, want a record-read error", err)
	}
	if len(inst.plans) != 0 {
		t.Errorf("must not install when the record cannot be read")
	}
}

// ---- v1 drives one node box owns; everything else is refused ----

func TestSupportProblem(t *testing.T) {
	cases := []struct {
		dist  Distribution
		nodes int
		want  string // "" means supported
	}{
		{DistK0s, 1, ""},
		{DistK3s, 1, ""},
		{DistVanilla, 1, ""},
		{DistKubeadm, 1, ""},
		{DistEKS, 1, "managed clusters are not supported"},
		{DistGKE, 1, "managed clusters are not supported"},
		{DistEKS, 3, "managed clusters are not supported"}, // named before the count
		{DistK3s, 0, "lists no nodes"},
		{DistK3s, 2, "2 nodes: box runs one seedbox on one node"},
		{DistK3s, 10, "10 nodes"},
	}
	for _, tc := range cases {
		got := supportProblem(tc.dist, tc.nodes)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("supportProblem(%s, %d) = %q, want supported", tc.dist, tc.nodes, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("supportProblem(%s, %d) = %q, want it to contain %q", tc.dist, tc.nodes, got, tc.want)
		}
	}
}

func TestBootstrapRefusesUnsupportedCluster(t *testing.T) {
	const path = "/explicit.yaml"
	cases := []struct {
		name       string
		version    string
		nodes      int
		wantReason string
	}{
		{"eks is refused by name", "v1.30.5-eks-123abc", 1, "managed clusters are not supported"},
		{"gke is refused by name, not by node count", "v1.29.2-gke.100", 3, "managed clusters are not supported"},
		{"two nodes", "v1.37.2+k3s1", 2, "2 nodes: box runs one seedbox on one node"},
		{"three nodes", "v1.37.2+k0s.1", 3, "3 nodes"},
		{"no nodes", "v1.37.2+k3s1", 0, "lists no nodes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
			prober := &fakeProber{
				version: Version{GitVersion: tc.version},
				cluster: Cluster{Nodes: tc.nodes},
			}
			host := &fakeHost{}
			recs := &fakeRecords{}
			inst := &fakeInstaller{}
			d := newDecision(src, prober, host, recs, inst)

			res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: path})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Action != ActionStop {
				t.Fatalf("action = %q, want stop; reason: %s", res.Action, res.Reason)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if prober.calls["describe"] != 1 {
				t.Errorf("the node count must come from the cluster: %v", prober.calls)
			}
			// Refused before anything is configured or recorded.
			if prober.calls["platform"] != 0 {
				t.Errorf("an unsupported cluster must not be platform-probed: %v", prober.calls)
			}
			if len(inst.plans) != 0 {
				t.Errorf("an unsupported cluster must not be installed onto")
			}
			if len(recs.saved) != 0 {
				t.Errorf("an unsupported cluster must not be recorded as usable")
			}
		})
	}
}

func TestBootstrapRecordUnsupportedClusterStops(t *testing.T) {
	// A record written by an older box can name a cluster v1 no longer drives;
	// it is refused before any probe.
	const path = "/var/lib/k0s/pki/admin.conf"
	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	prober := &fakeProber{version: Version{GitVersion: "v1.30.5-eks-123abc"}}
	host := &fakeHost{}
	recs := &fakeRecords{rec: &Record{Source: SourceExternal, Kubeconfig: path, Distribution: DistEKS, Nodes: 1}}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, host, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop || !strings.Contains(res.Reason, "managed clusters are not supported") {
		t.Fatalf("action=%q reason=%q, want stop naming the managed cluster", res.Action, res.Reason)
	}
	if prober.calls["new"] != 0 || prober.calls["version"] != 0 {
		t.Errorf("an unsupported recorded cluster must not be probed: %v", prober.calls)
	}
	if len(inst.plans) != 0 {
		t.Errorf("installer must not run")
	}
}

// ---- KUBECONFIG env and path-list resolution ----

func TestFirstKubeconfig(t *testing.T) {
	cases := []struct {
		flag, env string
		want      string
	}{
		{"", "", ""},
		{"/flag", "", "/flag"},
		{"/flag", "/env", "/flag"}, // flag wins
		{"", "/env", "/env"},
		{"", "/a:/b", "/a"}, // first of a list
	}
	for _, tc := range cases {
		if got := firstKubeconfig(tc.flag, tc.env); got != tc.want {
			t.Errorf("firstKubeconfig(%q, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
		}
	}
}

// Only auto mode is settled up front. An operator who said the address layer
// is theirs (external) is still verified by the Gateway, and managed already
// installs.
func TestFreshClusterAddressesKeepsExternal(t *testing.T) {
	in := []ComponentResult{
		{Component: ComponentLoadBalancer, Mode: ModeExternal, Action: "verify"},
		{Component: ComponentGateway, Mode: ModeAuto, Action: "install"},
	}
	out := freshClusterAddresses(in)
	if out[0].Action != "verify" || out[1].Action != "install" {
		t.Fatalf("got %+v, want external left to verify", out)
	}
	if in[0].Action != "verify" {
		t.Fatal("freshClusterAddresses changed its input")
	}
}
