package bootstrap

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// ---- fakes ----

type fakeSource struct {
	candidates  []string
	files       map[string][]byte
	readErr     map[string]error
	reads       []string
	candidatesN int
}

func (f *fakeSource) Candidates() []string {
	f.candidatesN++
	return f.candidates
}

func (f *fakeSource) Read(path string) ([]byte, error) {
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
}

func newFakeProber(p *fakeProber) func([]byte) (Prober, error) {
	return func([]byte) (Prober, error) {
		if p.calls == nil {
			p.calls = map[string]int{}
		}
		p.calls["new"]++
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
	loadErr error
	saved   []*Record
	saveErr error
}

func (f *fakeRecords) Load() (*Record, error) { return f.rec, f.loadErr }

func (f *fakeRecords) Save(r *Record) error {
	f.saved = append(f.saved, r)
	return f.saveErr
}

type fakeInstaller struct {
	plans []InstallPlan
	err   error
}

func (f *fakeInstaller) Install(_ context.Context, p InstallPlan) error {
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
				p.version = Version{GitVersion: "v1.31.3+k0s.0"}
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
		version:  Version{GitVersion: "v1.31.3+k3s1"},
		cluster:  Cluster{Nodes: 3},
		platform: PlatformProbe{GatewayAPI: OutcomePresent, LoadBalancer: OutcomePresent, Gateway: OutcomePresent, CertManager: OutcomePresent},
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
	if !res.StorageClassRequired {
		t.Errorf("storage class should be required for %d nodes", res.Nodes)
	}
	if len(recs.saved) != 1 || recs.saved[0].Source != SourceExternal {
		t.Errorf("record not saved as external")
	}
}

// ---- candidate: /version answers but SSAR denies -> not usable ----

func TestBootstrapCandidateSSARDeniedNotUsable(t *testing.T) {
	const candidate = "/etc/rancher/k3s/k3s.yaml"

	t.Run("falls through to clean host", func(t *testing.T) {
		src := &fakeSource{
			candidates: []string{candidate},
			files:      map[string][]byte{candidate: []byte("kubeconfig")},
		}
		prober := &fakeProber{
			version:  Version{GitVersion: "v1.31.3+k3s1"},
			adminErr: errors.New("identity cannot create namespaces, create clusterroles"),
			cluster:  Cluster{Nodes: 1},
			platform: PlatformProbe{},
		}
		host := &fakeHost{}
		recs := &fakeRecords{}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{})
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
			candidates: []string{candidate},
			files:      map[string][]byte{candidate: []byte("kubeconfig")},
		}
		prober := &fakeProber{
			version:  Version{GitVersion: "v1.31.3+k3s1"},
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

// ---- present but unusable: stop with an explanation naming what was found ----

func TestBootstrapPresentButUnusableStops(t *testing.T) {
	src := &fakeSource{candidates: []string{"/var/lib/k0s/pki/admin.conf"}}
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

// ---- existing record: probing skipped, external lifecycle never touched ----

func TestBootstrapRecordSkipsProbing(t *testing.T) {
	const path = "/var/lib/k0s/pki/admin.conf"

	t.Run("external record: reachable, never touched", func(t *testing.T) {
		src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
		prober := &fakeProber{version: Version{GitVersion: "v1.31.3+k3s1"}}
		host := &fakeHost{findings: []Finding{{Kind: "process", Name: "kubelet"}}}
		recs := &fakeRecords{rec: &Record{
			Source:       SourceExternal,
			Kubeconfig:   path,
			Distribution: DistK3s,
			Nodes:        3,
		}}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionUseCluster || res.Source != SourceExternal {
			t.Fatalf("action=%q source=%q, want use-cluster external", res.Action, res.Source)
		}
		if res.Distribution != DistK3s || res.Nodes != 3 {
			t.Errorf("distribution/nodes must come from the record, not a re-probe")
		}
		if !res.StorageClassRequired {
			t.Errorf("3-node recorded cluster must require a storage class")
		}
		if host.called != 0 {
			t.Errorf("host probing must be skipped when a record exists")
		}
		if len(inst.plans) != 0 {
			t.Errorf("an external cluster's lifecycle must never be touched")
		}
		// only reachability (Version) is checked; Describe/Platform/CanAdmin are not
		if prober.calls["describe"] != 0 || prober.calls["platform"] != 0 || prober.calls["admin"] != 0 {
			t.Errorf("record re-run must not re-probe capabilities: %v", prober.calls)
		}
		if prober.calls["version"] != 1 {
			t.Errorf("reachability check should call /version once: %v", prober.calls)
		}
		if len(recs.saved) != 0 {
			t.Errorf("external record re-run must not rewrite the record")
		}
	})

	t.Run("external record: unreachable stops", func(t *testing.T) {
		src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
		prober := &fakeProber{versionErr: errors.New("connection refused")}
		host := &fakeHost{}
		recs := &fakeRecords{rec: &Record{Source: SourceExternal, Kubeconfig: path, Distribution: DistK3s, Nodes: 1}}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionStop || !strings.Contains(res.Reason, "unreachable") {
			t.Fatalf("action=%q reason=%q, want stop/unreachable", res.Action, res.Reason)
		}
		if len(inst.plans) != 0 {
			t.Errorf("installer must not run")
		}
	})

	t.Run("k0s-managed record: no probing at all", func(t *testing.T) {
		src := &fakeSource{}
		prober := &fakeProber{}
		host := &fakeHost{}
		recs := &fakeRecords{rec: &Record{Source: SourceK0sManaged, Kubeconfig: path, Distribution: DistK0s, Nodes: 1}}
		inst := &fakeInstaller{}
		d := newDecision(src, prober, host, recs, inst)

		res, err := d.Bootstrap(context.Background(), Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != ActionUseManaged {
			t.Fatalf("action = %q, want use-managed", res.Action)
		}
		if prober.calls["new"] != 0 {
			t.Errorf("managed re-run must not build a prober: %v", prober.calls)
		}
		if len(inst.plans) != 0 {
			t.Errorf("installer must not run on a managed re-run")
		}
	})
}

// ---- clean host: install planned ----

func TestBootstrapCleanHostInstalls(t *testing.T) {
	src := &fakeSource{candidates: []string{}}
	prober := &fakeProber{}
	host := &fakeHost{}
	recs := &fakeRecords{}
	inst := &fakeInstaller{}
	d := newDecision(src, prober, host, recs, inst)

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionInstall {
		t.Fatalf("action = %q, want install", res.Action)
	}
	if res.Source != SourceK0sManaged {
		t.Errorf("source = %q, want k0s-managed", res.Source)
	}
	if res.Kubeconfig != K0sAdminConfig {
		t.Errorf("kubeconfig = %q, want %q", res.Kubeconfig, K0sAdminConfig)
	}
	if len(inst.plans) != 1 || inst.plans[0].Version != K0sVersion {
		t.Errorf("installer should get one plan for %s: %v", K0sVersion, inst.plans)
	}
	if len(recs.saved) != 1 || recs.saved[0].Source != SourceK0sManaged {
		t.Errorf("clean install must record source k0s-managed")
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

// ---- multi-node forces a storage class ----

func TestStorageClassRequired(t *testing.T) {
	cases := []struct {
		nodes int
		want  bool
	}{{0, false}, {1, false}, {2, true}, {3, true}, {10, true}}
	for _, tc := range cases {
		if got := storageClassRequired(tc.nodes); got != tc.want {
			t.Errorf("storageClassRequired(%d) = %v, want %v", tc.nodes, got, tc.want)
		}
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
