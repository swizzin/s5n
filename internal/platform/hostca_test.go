package platform

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/say"
)

// testCA makes a self-signed CA, the shape of both the box root's Secret and a
// --ca-file, and returns its certificate and key as PEM.
func testCA(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// fakeHostCACluster is the host-CA step's cluster: the objects it reads by
// name and the writes and removals it made.
type fakeHostCACluster struct {
	objects map[string]map[string]any
	applied [][]map[string]any
	deleted []string
}

func newFakeHostCA() *fakeHostCACluster {
	return &fakeHostCACluster{objects: map[string]map[string]any{}}
}

func (f *fakeHostCACluster) put(kind, namespace, name string, obj map[string]any) {
	f.objects[objectKey(kind, namespace, name)] = obj
}

func (f *fakeHostCACluster) rootCertificate(value []byte) {
	f.put("Secret", CertManagerNamespace, RootSecretName, map[string]any{
		"metadata": map[string]any{"name": RootSecretName, "namespace": CertManagerNamespace},
		"data":     map[string]any{"tls.crt": base64.StdEncoding.EncodeToString(value)},
	})
}

// certificate puts the box's host Certificate in with the issuer it names, the
// way cert-manager holds it: what the anchor follows when this run writes none.
func (f *fakeHostCACluster) certificate(issuer string) {
	f.put("Certificate", GatewayNamespace, CertificateName("box.example.test"), map[string]any{
		"metadata": map[string]any{"name": CertificateName("box.example.test"), "namespace": GatewayNamespace},
		"spec": map[string]any{"issuerRef": map[string]any{
			"name": issuer, "kind": "ClusterIssuer", "group": "cert-manager.io",
		}},
	})
}

func (f *fakeHostCACluster) Allowed(context.Context, Access) (bool, error) {
	return true, nil
}

func (f *fakeHostCACluster) PrimaryInternalIP(context.Context) (string, error) {
	return "192.0.2.10", nil
}

func (f *fakeHostCACluster) Apply(_ context.Context, docs []map[string]any) error {
	f.applied = append(f.applied, docs)
	return nil
}

func (f *fakeHostCACluster) Await(context.Context, Wait) error { return nil }

func (f *fakeHostCACluster) CertificateReady(context.Context, string, string) (bool, string, error) {
	return true, "", nil
}

func (f *fakeHostCACluster) Get(_ context.Context, _, _, kind, namespace, name string) (map[string]any, bool, error) {
	obj, ok := f.objects[objectKey(kind, namespace, name)]
	return obj, ok, nil
}

func (f *fakeHostCACluster) Delete(_ context.Context, _, _, kind, namespace, name string) error {
	f.deleted = append(f.deleted, objectKey(kind, namespace, name))
	delete(f.objects, objectKey(kind, namespace, name))
	return nil
}

// anchorDoc is the one ConfigMap of a write, or nil.
func (f *fakeHostCACluster) anchorDoc(t *testing.T) map[string]any {
	t.Helper()
	if len(f.applied) == 0 {
		return nil
	}
	return f.applied[len(f.applied)-1][0]
}

// configMap puts an object in as the anchor ConfigMap already holds cert.
func (f *fakeHostCACluster) configMap(cert []byte) {
	f.put("ConfigMap", "seedbox", HostCAName, map[string]any{
		"metadata": map[string]any{"name": HostCAName, "namespace": "seedbox"},
		"data":     map[string]any{HostCAKey: string(cert)},
	})
}

func newHostCA(t *testing.T, cl *fakeHostCACluster, issuer, caFile string) *HostCADecision {
	t.Helper()
	return &HostCADecision{
		Namespace: "seedbox",
		Host:      "box.example.test",
		Issuer:    issuer,
		CAFile:    caFile,
		Open:      func() (HostCACluster, error) { return cl, nil },
		Say:       &say.Plain{W: io.Discard},
	}
}

// TestAnchorFor is the whole decision as a table: the issuer this run writes
// the box's host certificate with, the issuer the Certificate in the cluster
// names, and whether a development run supplied a file.
//
// The rows without a written issuer are the ones that matter for the window a
// box moves in: a run that writes no certificate follows the one that is
// there, whatever DNS says about the name that day.
func TestAnchorFor(t *testing.T) {
	cases := []struct {
		name      string
		written   string
		cert      string
		certThere bool
		caFile    string
		want      anchorSource
	}{
		{"this run writes a self-signed certificate", "selfsigned", "", false, "", anchorFromRoot},
		{"this run writes an ACME certificate", "letsencrypt", "", false, "", anchorNothing},
		{"this run writes an ACME certificate with a file", "letsencrypt", "", false, "/tmp/ca.pem", anchorFromFile},
		{"the ClusterIssuer name is accepted", SelfSignedIssuer, "", false, "", anchorFromRoot},
		{"the ACME ClusterIssuer name is accepted", LetsEncryptIssuer, "", false, "", anchorNothing},
		// A first init: no Certificate exists yet, and the run's own decision is
		// the only input there is.
		{"first init, absent Certificate, this run decides self-signed", "selfsigned", "", false, "", anchorFromRoot},
		{"first init, absent Certificate, this run decides Let's Encrypt", "letsencrypt", "", false, "", anchorNothing},
		// A run that writes no certificate follows the one in the cluster.
		{"the Certificate says self-signed", "", SelfSignedIssuer, true, "", anchorFromRoot},
		{"the Certificate says Let's Encrypt", "", LetsEncryptIssuer, true, "", anchorNothing},
		{"the Certificate says Let's Encrypt and a file is given", "", LetsEncryptIssuer, true, "/tmp/ca.pem", anchorFromFile},
		{"the Certificate names an issuer that is not box's", "", "someone-elses-issuer", true, "", anchorNothing},
		// This run's own write outranks a Certificate that says otherwise: the
		// per-host step moves it, and the anchor has to land where it will be.
		{"this run writes self-signed over an ACME Certificate", "selfsigned", LetsEncryptIssuer, true, "", anchorFromRoot},
		{"this run writes Let's Encrypt over a self-signed Certificate", "letsencrypt", SelfSignedIssuer, true, "", anchorNothing},
		// Nothing says which issuer the host has: nothing follows.
		{"no Certificate and no write", "", "", false, "", anchorUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := anchorFor(tc.written, tc.cert, tc.certThere, tc.caFile)
			if err != nil {
				t.Fatalf("anchorFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("anchorFor(%q, %q, %v, %q) = %v, want %v", tc.written, tc.cert, tc.certThere, tc.caFile, got, tc.want)
			}
		})
	}
	if _, err := anchorFor("acme-ish", "", false, ""); err == nil {
		t.Fatal("an issuer box does not install was accepted")
	}
}

// TestHostCAWritesTheRoot: the anchor is the root's tls.crt, one key, no key
// material -- even if the Secret held some.
func TestHostCAWritesTheRoot(t *testing.T) {
	root, rootKey := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	// A Secret that carries the key beside the certificate, the way a sloppy
	// export would: box copies certificates only.
	cl.rootCertificate(append(append([]byte{}, root...), rootKey...))

	res, err := newHostCA(t, cl, "selfsigned", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote || res.Source != "root" {
		t.Fatalf("result = %+v, want a write from the root", res)
	}
	doc := cl.anchorDoc(t)
	if doc == nil {
		t.Fatal("no ConfigMap was applied")
	}
	if kind, ns, name := objectMeta(doc); kind != "ConfigMap" || ns != "seedbox" || name != HostCAName {
		t.Fatalf("wrote %s %s/%s, want the anchor in the seedbox's namespace", kind, ns, name)
	}
	data, _, _ := unstructured.NestedStringMap(doc, "data")
	if len(data) != 1 {
		t.Fatalf("data = %v, want one key", data)
	}
	if data[HostCAKey] != string(root) {
		t.Fatalf("data[%s] is not the root's certificate:\n%q", HostCAKey, data[HostCAKey])
	}
	if strings.Contains(data[HostCAKey], "PRIVATE KEY") {
		t.Fatal("the anchor carries private key material")
	}
	labels := labelsOf(doc)
	if labels["app.kubernetes.io/part-of"] != "s5n" || labels["app.kubernetes.io/managed-by"] != "box" {
		t.Fatalf("labels = %v, want box's ownership labels", labels)
	}
}

// TestHostCAReadsAFileForAnACMEHost: a Pebble-issued certificate has no ca.crt
// anywhere, so a development run hands box the root it would otherwise copy.
func TestHostCAReadsAFileForAnACMEHost(t *testing.T) {
	ca, key := testCA(t, "pebble root")
	path := filepath.Join(t.TempDir(), "pebble-root.pem")
	if err := os.WriteFile(path, append(append([]byte{}, ca...), key...), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := newFakeHostCA()
	res, err := newHostCA(t, cl, "letsencrypt", path).Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote || res.Source != "file" {
		t.Fatalf("result = %+v, want a write from the file", res)
	}
	data, _, _ := unstructured.NestedStringMap(cl.anchorDoc(t), "data")
	if data[HostCAKey] != string(ca) {
		t.Fatalf("the anchor is not the file's certificate:\n%q", data[HostCAKey])
	}
	if strings.Contains(data[HostCAKey], "PRIVATE KEY") {
		t.Fatal("the anchor carries private key material from --ca-file")
	}
}

// TestHostCARemovesTheAnchorForLetsEncrypt: a box that moved to a public
// certificate must not leave a pod trusting a root nothing chains to.
func TestHostCARemovesTheAnchorForLetsEncrypt(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.configMap(root)

	res, err := newHostCA(t, cl, "letsencrypt", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote || !res.Removed {
		t.Fatalf("result = %+v, want a removal", res)
	}
	if len(cl.deleted) != 1 || cl.deleted[0] != objectKey("ConfigMap", "seedbox", HostCAName) {
		t.Fatalf("deleted = %v, want the anchor", cl.deleted)
	}
	if len(cl.applied) != 0 {
		t.Fatalf("a removal applied %d objects", len(cl.applied))
	}
	// Removing what is not there is the state it wants, and it is not a write.
	cl2 := newFakeHostCA()
	res, err = newHostCA(t, cl2, "letsencrypt", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote || len(cl2.deleted) != 0 {
		t.Fatalf("an absent anchor was removed again: %+v %v", res, cl2.deleted)
	}
}

// TestHostCAIsIdempotent: a re-run writes nothing, which is what keeps the
// ConfigMap's resource version -- the thing a pod's file is pinned to -- still.
func TestHostCAIsIdempotent(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)
	cl.configMap(root)

	res, err := newHostCA(t, cl, "selfsigned", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote || len(cl.applied) != 0 || len(cl.deleted) != 0 {
		t.Fatalf("a run over a correct anchor wrote: %+v", res)
	}
}

// TestHostCAReplacesAStaleAnchor: an anchor from an older root is the one case
// a write must happen silently, because the certificate under it changed and
// nothing else would notice.
func TestHostCAReplacesAStaleAnchor(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	old, _ := testCA(t, "an older box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)
	cl.configMap(old)

	res, err := newHostCA(t, cl, "selfsigned", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote {
		t.Fatalf("result = %+v, want a write", res)
	}
	data, _, _ := unstructured.NestedStringMap(cl.anchorDoc(t), "data")
	if data[HostCAKey] != string(root) {
		t.Fatal("the stale anchor was not replaced")
	}
}

// TestHostCAFollowsTheCertificateInTheCluster: a run that writes no certificate
// reads the one the host is served with. The anchor moves when the Certificate
// does, and not before.
func TestHostCAFollowsTheCertificateInTheCluster(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)
	cl.certificate(SelfSignedIssuer)

	res, err := newHostCA(t, cl, "", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote || res.Source != "root" {
		t.Fatalf("result = %+v, want a write from the root", res)
	}
	if len(cl.deleted) != 0 {
		t.Fatalf("deleted = %v: a self-signed Certificate's anchor was removed", cl.deleted)
	}
	data, _, _ := unstructured.NestedStringMap(cl.anchorDoc(t), "data")
	if data[HostCAKey] != string(root) {
		t.Fatal("the anchor is not the box root")
	}

	// The same Certificate once the host has moved: the anchor goes with it.
	cl2 := newFakeHostCA()
	cl2.certificate(LetsEncryptIssuer)
	cl2.configMap(root)
	res, err = newHostCA(t, cl2, "", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed || len(cl2.deleted) != 1 {
		t.Fatalf("result = %+v, deleted = %v, want the anchor gone with the ACME Certificate", res, cl2.deleted)
	}
}

// TestAPublicNameKeepsASelfSignedHostsAnchor is the window this decision exists
// for: the operator adds the record and runs apply before init. The host still
// serves its self-signed certificate -- only init's per-host step moves it to
// Let's Encrypt -- so the anchor has to stay, and an app that restarts in
// between still trusts the login.
//
// The name's DNS answer is not an input here and cannot be: the Certificate is.
// That is what makes the assertion hold on the day the record appears.
func TestAPublicNameKeepsASelfSignedHostsAnchor(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)
	cl.certificate(SelfSignedIssuer)
	cl.configMap(root)

	// A run that writes no certificate, over a Certificate that still says
	// s5n-selfsigned: nothing to plan, nothing to write, nothing to remove.
	plan, err := newHostCA(t, cl, "", "").Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan != "" {
		t.Fatalf("plan = %q, want no anchor work", plan)
	}
	res, err := newHostCA(t, cl, "", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote || len(cl.deleted) != 0 || len(cl.applied) != 0 {
		t.Fatalf("an apply over a self-signed host touched the anchor: %+v %v %v", res, cl.deleted, cl.applied)
	}
}

// TestHostCAWithNoCertificateAndNoWrite: a box whose host Certificate this run
// did not write and cannot find has nothing to follow, so it leaves the anchor
// alone rather than guessing an issuer.
func TestHostCAWithNoCertificateAndNoWrite(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)

	plan, err := newHostCA(t, cl, "", "").Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan != "" {
		t.Fatalf("plan = %q, want nothing", plan)
	}
	res, err := newHostCA(t, cl, "", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote || len(cl.applied) != 0 || len(cl.deleted) != 0 {
		t.Fatalf("a run with nothing to follow wrote: %+v %v %v", res, cl.deleted, cl.applied)
	}
}

// TestHostCAPlan: the plan is presence only, so it says what a run will write
// without reading cert-manager's namespace.
func TestHostCAPlan(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cases := []struct {
		name   string
		issuer string
		caFile string
		cert   string
		anchor bool
		want   string
	}{
		{"self-signed, no anchor", "selfsigned", "", "", false, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"self-signed, anchor there", "selfsigned", "", "", true, ""},
		{"Let's Encrypt, anchor there", "letsencrypt", "", "", true, "remove the host CA ConfigMap seedbox/" + HostCAName},
		{"Let's Encrypt, no anchor", "letsencrypt", "", "", false, ""},
		{"Let's Encrypt with a file, no anchor", "letsencrypt", "/tmp/ca.pem", "", false, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"the Certificate is self-signed, no anchor", "", "", SelfSignedIssuer, false, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"the Certificate is Let's Encrypt, anchor there", "", "", LetsEncryptIssuer, true, "remove the host CA ConfigMap seedbox/" + HostCAName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := newFakeHostCA()
			cl.rootCertificate(root)
			if tc.cert != "" {
				cl.certificate(tc.cert)
			}
			if tc.anchor {
				cl.configMap(root)
			}
			got, err := newHostCA(t, cl, tc.issuer, tc.caFile).Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHostCAWithoutARoot: an old platform has no root, and a run must say which
// command makes one rather than writing an anchor from nothing.
func TestHostCAWithoutARoot(t *testing.T) {
	cl := newFakeHostCA()
	_, err := newHostCA(t, cl, "selfsigned", "").Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "box bootstrap") {
		t.Fatalf("want a refusal naming bootstrap, got %v", err)
	}
	if len(cl.applied) != 0 {
		t.Fatal("an anchor was written without a root")
	}
}

// TestSignedByRoot is the upgrade decision over the two certificates: is the
// one in a host's Secret issued by the root that is in the seedbox's namespace?
func TestSignedByRoot(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	other, _ := testCA(t, "a bare self-signed leaf")
	cases := []struct {
		name   string
		secret []byte
		want   bool
	}{
		{"the root itself", root, true},
		{"another CA (the 0.3.0 shape)", other, false},
		{"no ca.crt at all (an ACME Secret)", nil, false},
		{"something that is not a certificate", []byte("not a pem"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := signedByRoot(tc.secret, root)
			if err != nil {
				t.Fatalf("signedByRoot: %v", err)
			}
			if got != tc.want {
				t.Fatalf("signedByRoot = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := signedByRoot(root, []byte("not a pem")); err == nil {
		t.Fatal("a root that is not a certificate was accepted")
	}
}

// TestCertificatePEMDropsEverythingElse: the one reader both sources pass
// through keeps CERTIFICATE blocks and nothing else, so a key cannot reach the
// anchor by any route.
func TestCertificatePEMDropsEverythingElse(t *testing.T) {
	ca, key := testCA(t, "a ca")
	got, err := certificatePEM(append(append(append([]byte{}, key...), ca...), key...))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(ca) {
		t.Fatalf("certificatePEM returned:\n%s", got)
	}
	if _, err := certificatePEM(key); err == nil {
		t.Fatal("a document with no certificate was accepted")
	}
}
