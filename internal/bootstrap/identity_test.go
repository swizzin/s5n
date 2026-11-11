package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCA is a stand-in CA bundle. Nothing parses it: box hashes the bytes it
// would trust, and a fixture that looked like a real certificate would suggest
// the test depends on its contents.
var testCA = []byte("-----BEGIN CERTIFICATE-----\nexample cluster CA\n-----END CERTIFICATE-----\n")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// kubeconfigForServer is a one-cluster kubeconfig the way k0s and k3s write it:
// the CA inline as certificate-authority-data, and a token for the identity.
func kubeconfigForServer(server string, ca []byte) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: example
contexts:
- context:
    cluster: example
    user: example
  name: example
current-context: example
users:
- name: example
  user:
    token: example
`, base64.StdEncoding.EncodeToString(ca), server))
}

// kubeconfigForCAFile is the same shape with the CA named as a file instead of
// carried inline.
func kubeconfigForCAFile(server, caFile string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority: %s
    server: %s
  name: example
contexts:
- context:
    cluster: example
    user: example
  name: example
current-context: example
users:
- name: example
  user:
    token: example
`, caFile, server))
}

// The identity is what a record carries: the server URL client-go resolved, and
// the sha256 of the CA bundle it would trust, inline or from a file.
func TestClusterIdentity(t *testing.T) {
	const server = "https://192.0.2.10:6443"

	t.Run("inline CA bundle", func(t *testing.T) {
		id := identityOf(kubeconfigForServer(server, testCA))
		if id.Server != server {
			t.Errorf("server = %q, want %q", id.Server, server)
		}
		if id.CAHash != sha256Hex(testCA) {
			t.Errorf("caHash = %q, want the bundle's sha256 %q", id.CAHash, sha256Hex(testCA))
		}
	})

	t.Run("CA bundle named as a file", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(caFile, testCA, 0o600); err != nil {
			t.Fatal(err)
		}
		id := identityOf(kubeconfigForCAFile(server, caFile))
		if id.Server != server || id.CAHash != sha256Hex(testCA) {
			t.Errorf("identity = %+v, want server %q and the file's sha256 %q", id, server, sha256Hex(testCA))
		}
	})

	t.Run("no CA bundle", func(t *testing.T) {
		id := identityOf(kubeconfigFixtureToken(t, server))
		if id.CAHash != sha256Hex(nil) {
			t.Errorf("caHash = %q, want the empty bundle's digest %q", id.CAHash, sha256Hex(nil))
		}
	})

	t.Run("a trailing slash is the same server", func(t *testing.T) {
		id := identityOf(kubeconfigForServer(server+"/", testCA))
		if id.Server != server {
			t.Errorf("server = %q, want %q", id.Server, server)
		}
	})

	t.Run("a kubeconfig box cannot resolve carries no identity", func(t *testing.T) {
		if id := identityOf([]byte("kubeconfig")); id != (clusterIdentity{}) {
			t.Errorf("identity = %+v, want the zero identity so the prober reports the refusal", id)
		}
	})
}

// The recorded cluster the identity tests re-run against: k3s's admin
// kubeconfig path, one server, and a CA bundle whose digest the record holds.
const (
	identityKubeconfig = "/etc/rancher/k3s/k3s.yaml"
	identityServer     = "https://192.0.2.10:6443"
)

type identityRun struct {
	res   Result
	recs  *fakeRecords
	inst  *fakeInstaller
	probe *fakeProber
	trace *[]string
}

// runRecordedIdentity stages a re-run of a record carrying id and a kubeconfig
// whose current context reaches identityServer with testCA.
func runRecordedIdentity(t *testing.T, id clusterIdentity) identityRun {
	t.Helper()
	return runRecordedIdentityKubeconfig(t, id, kubeconfigForServer(identityServer, testCA))
}

// runRecordedIdentityKubeconfig is runRecordedIdentity with the kubeconfig the
// recorded path now holds given explicitly.
func runRecordedIdentityKubeconfig(t *testing.T, id clusterIdentity, kc []byte) identityRun {
	t.Helper()
	rec := externalRecord(identityKubeconfig)
	rec.Server, rec.CAHash = id.Server, id.CAHash
	probe := &fakeProber{
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
	d, src, recs, inst, trace := recordedCluster(rec, probe)
	src.files[identityKubeconfig] = kc
	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return identityRun{res: res, recs: recs, inst: inst, probe: probe, trace: trace}
}

// refusedBeforeProbing is the shape every mismatch must have: a stop that names
// what the test asks for, no probe, no record, no install.
func (r identityRun) refusedBeforeProbing(t *testing.T, wants ...string) {
	t.Helper()
	if r.res.Action != ActionStop {
		t.Fatalf("action = %q, want stop; reason: %s", r.res.Action, r.res.Reason)
	}
	for _, want := range wants {
		if !strings.Contains(r.res.Reason, want) {
			t.Errorf("reason = %q, want it to name %q", r.res.Reason, want)
		}
	}
	if len(r.probe.calls) != 0 {
		t.Errorf("probed before refusing: %v", r.probe.calls)
	}
	if len(r.recs.saved) != 0 || len(r.inst.plans) != 0 {
		t.Errorf("a refused re-run wrote: %d records, %d installs", len(r.recs.saved), len(r.inst.plans))
	}
	if strings.Contains(strings.Join(*r.trace, "|"), "save record") {
		t.Errorf("a refused re-run rewrote the record: %v", *r.trace)
	}
}

// The YAML a box from before this change wrote has neither key. It parses to a
// record with no identity -- accepted, not refused as malformed -- which is what
// makes the upgrade path safe.
func TestParseRecordWithoutClusterIdentity(t *testing.T) {
	rec, err := parseRecord([]byte(`source: external
kubeconfig: "/etc/rancher/k3s/k3s.yaml"
distribution: k3s
nodes: 1
platform:
  gateway-api: auto
  loadbalancer: auto
  gateway: auto
  cert-manager: auto
gatewayClass: "eg"
`))
	if err != nil {
		t.Fatalf("an old record must parse: %v", err)
	}
	if rec.Server != "" || rec.CAHash != "" {
		t.Errorf("identity = %q/%q, want both empty", rec.Server, rec.CAHash)
	}
}

// A re-run of a record whose kubeconfig still reaches the recorded cluster
// probes it and writes nothing.
func TestBootstrapRecordIdentityMatch(t *testing.T) {
	run := runRecordedIdentity(t, clusterIdentity{Server: identityServer, CAHash: sha256Hex(testCA)})
	if run.res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; reason: %s", run.res.Action, run.res.Reason)
	}
	if run.probe.calls["version"] != 1 {
		t.Errorf("the matching identity did not reach the probe: %v", run.probe.calls)
	}
	if len(run.recs.saved) != 0 || len(run.inst.plans) != 0 {
		t.Errorf("an idempotent re-run wrote: %d records, %d installs", len(run.recs.saved), len(run.inst.plans))
	}
}

// The same cluster reached at another address (a tailnet name instead of the
// local one) is still the recorded cluster: the CA is its identity, the URL is
// only the route. It is probed, not refused.
func TestBootstrapRecordIdentityServerMoved(t *testing.T) {
	run := runRecordedIdentity(t, clusterIdentity{Server: "https://198.51.100.7:6443", CAHash: sha256Hex(testCA)})
	if run.res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; the same CA at another address is the same cluster; reason: %s", run.res.Action, run.res.Reason)
	}
	if run.probe.calls["version"] != 1 {
		t.Errorf("the moved server did not reach the probe: %v", run.probe.calls)
	}
}

// A cluster trusted without a CA bundle has no identity but its URL: every such
// cluster hashes the empty bundle. There a different server is refused.
func TestBootstrapRecordIdentityNoCAServerMismatch(t *testing.T) {
	if noCAHash != sha256Hex(nil) {
		t.Fatalf("noCAHash = %q, want the empty bundle's sha256 %q", noCAHash, sha256Hex(nil))
	}
	rec := clusterIdentity{Server: "https://198.51.100.7:6443", CAHash: noCAHash}
	runRecordedIdentityKubeconfig(t, rec, kubeconfigFixtureToken(t, identityServer)).refusedBeforeProbing(t,
		rec.Server, identityServer, RecordPath, "trusted without a CA bundle", "will not treat a different cluster")
}

// A kubeconfig box cannot resolve has no identity to compare. The check stands
// aside so the prober refuses the file with its own message instead of a
// comparison against an empty server.
func TestIdentityConflictUnresolvedKubeconfig(t *testing.T) {
	rec := &Record{Source: SourceExternal, Kubeconfig: identityKubeconfig, Server: identityServer, CAHash: sha256Hex(testCA)}
	if got := identityConflict(rec, clusterIdentity{}, RecordPath); got != "" {
		t.Errorf("identityConflict = %q, want no refusal for an unresolved kubeconfig", got)
	}
}

// The same server with a different CA bundle is another cluster too: box would
// trust a different CA while applying the record's decisions.
func TestBootstrapRecordIdentityCAMismatch(t *testing.T) {
	rec := clusterIdentity{Server: identityServer, CAHash: sha256Hex([]byte("another CA"))}
	runRecordedIdentity(t, rec).refusedBeforeProbing(t,
		identityServer, RecordPath, "CA bundle", "will not treat a different cluster")
}

// A record written before box kept the identity has neither field. It is
// accepted -- box cannot tell, and must not refuse the cluster it has managed
// all along -- and goes on to the probe.
func TestBootstrapRecordIdentityOldRecord(t *testing.T) {
	run := runRecordedIdentity(t, clusterIdentity{})
	if run.res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; an old record must not be refused for the missing fields; reason: %s", run.res.Action, run.res.Reason)
	}
	if run.probe.calls["version"] != 1 {
		t.Errorf("an old record did not reach the probe: %v", run.probe.calls)
	}
	if len(run.recs.saved) != 0 || len(run.inst.plans) != 0 {
		t.Errorf("an old record's re-run wrote: %d records, %d installs", len(run.recs.saved), len(run.inst.plans))
	}
}

// The record box writes for a cluster it just adopted carries the identity, or
// no later run could compare it.
func TestBootstrapRecordWritesClusterIdentity(t *testing.T) {
	const path = "/explicit.yaml"
	const server = "https://192.0.2.10:6443"
	src := &fakeSource{files: map[string][]byte{path: kubeconfigForServer(server, testCA)}}
	prober := &fakeProber{
		version: Version{GitVersion: "v1.37.2+k3s1"},
		cluster: Cluster{Nodes: 1},
		platform: PlatformProbe{
			GatewayAPI:   OutcomePresent,
			LoadBalancer: OutcomePresent,
			Gateway:      OutcomePresent,
			CertManager:  OutcomePresent,
			Classes:      []GatewayClass{{Name: EnvoyGatewayClass, ControllerName: EnvoyGatewayController, Accepted: true, ManagedByBox: true}},
		},
	}
	recs := &fakeRecords{}
	d := newDecision(src, prober, &fakeHost{}, recs, &fakeInstaller{})

	res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: path, Platform: DefaultPlatform()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster; reason: %s", res.Action, res.Reason)
	}
	if len(recs.saved) != 1 {
		t.Fatalf("saved %d records, want 1", len(recs.saved))
	}
	saved := recs.saved[0]
	if saved.Server != server || saved.CAHash != sha256Hex(testCA) {
		t.Fatalf("saved identity = %q/%q, want %q/%q", saved.Server, saved.CAHash, server, sha256Hex(testCA))
	}
	// The record's fixed schema round-trips the identity, so the next run reads
	// what this one wrote.
	back, err := parseRecord(encodeRecord(saved))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if back.Server != saved.Server || back.CAHash != saved.CAHash {
		t.Errorf("round trip lost the identity: %+v, want %+v", back, saved)
	}
}
