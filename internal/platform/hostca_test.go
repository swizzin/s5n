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

// testCA is a self-signed CA, the shape of the root's Secret or a --ca-file.
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

// fakeHostCACluster is the step's cluster: objects read by name, writes and removals.
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

// certificate adds the host Certificate with the issuer it names: what the anchor
// follows when this run writes none.
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

// anchorDoc is the write's ConfigMap, or nil.
func (f *fakeHostCACluster) anchorDoc(t *testing.T) map[string]any {
	t.Helper()
	if len(f.applied) == 0 {
		return nil
	}
	return f.applied[len(f.applied)-1][0]
}

// configMap is an object the anchor already holds cert in.
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

// The whole decision as a table. The rows without a written issuer matter for the
// window a box moves in: a run writing no certificate follows the one there, whatever
// DNS says that day.
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
		// A first init: no Certificate, so the run's own decision is the input.
		{"first init, absent Certificate, this run decides self-signed", "selfsigned", "", false, "", anchorFromRoot},
		{"first init, absent Certificate, this run decides Let's Encrypt", "letsencrypt", "", false, "", anchorNothing},
		// A run writing no certificate follows the one in the cluster.
		{"the Certificate says self-signed", "", SelfSignedIssuer, true, "", anchorFromRoot},
		{"the Certificate says Let's Encrypt", "", LetsEncryptIssuer, true, "", anchorNothing},
		{"the Certificate says Let's Encrypt and a file is given", "", LetsEncryptIssuer, true, "/tmp/ca.pem", anchorFromFile},
		{"the Certificate names an issuer that is not box's", "", "someone-elses-issuer", true, "", anchorNothing},
		// This run's write outranks the Certificate: the anchor lands where the host
		// will be.
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

// The anchor is the root's tls.crt, one key, no key material even if the Secret had
// some.
func TestHostCAWritesTheRoot(t *testing.T) {
	root, rootKey := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	// A Secret with the key beside the certificate: box copies certificates only.
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

// A Pebble-issued certificate has no ca.crt, so a development run hands over the
// root.
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

// A box on a public certificate must not leave a pod trusting a root nothing chains
// to.
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
	// Removing what is not there is not a write.
	cl2 := newFakeHostCA()
	res, err = newHostCA(t, cl2, "letsencrypt", "").Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote || len(cl2.deleted) != 0 {
		t.Fatalf("an absent anchor was removed again: %+v %v", res, cl2.deleted)
	}
}

// A re-run writes nothing, keeping the ConfigMap's resource version still.
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

// An anchor from an older root is replaced: the certificate under it changed and
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

// A run writing no certificate reads the one served; the anchor moves with it.
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

// The window this decision exists for: the record is added and apply runs before
// init, so the host still serves its self-signed certificate and the anchor stays.
// The Certificate is the input, never the name's DNS answer.
func TestAPublicNameKeepsASelfSignedHostsAnchor(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	cl := newFakeHostCA()
	cl.rootCertificate(root)
	cl.certificate(SelfSignedIssuer)
	cl.configMap(root)

	// No certificate written, the Certificate still s5n-selfsigned: nothing to do.
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

// No written and no found Certificate means nothing to follow: the anchor is left.
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

// The plan names what a run writes, replaces or removes, comparing the certificate
// and not merely its presence.
func TestHostCAPlan(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	other, _ := testCA(t, "an older root")
	cases := []struct {
		name string
		// issuer and caFile are the run's inputs; caFile is written to a temp file.
		issuer string
		caFile []byte
		// cert is the issuer the host Certificate names, when there is one.
		cert string
		// anchor is what the ConfigMap holds: nil for absent.
		anchor []byte
		want   string
	}{
		{"self-signed, no anchor", "selfsigned", nil, "", nil, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"self-signed, anchor there", "selfsigned", nil, "", root, ""},
		{"self-signed, anchor holds an older root", "selfsigned", nil, "", other, "replace the host CA ConfigMap seedbox/" + HostCAName},
		{"Let's Encrypt, anchor there", "letsencrypt", nil, "", root, "remove the host CA ConfigMap seedbox/" + HostCAName},
		{"Let's Encrypt, no anchor", "letsencrypt", nil, "", nil, ""},
		{"Let's Encrypt with a file, no anchor", "letsencrypt", other, "", nil, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"Let's Encrypt with a file, anchor holds the root", "letsencrypt", other, "", root, "replace the host CA ConfigMap seedbox/" + HostCAName},
		{"Let's Encrypt with a file, anchor holds it", "letsencrypt", other, "", other, ""},
		{"the Certificate is self-signed, no anchor", "", nil, SelfSignedIssuer, nil, "write the host CA ConfigMap seedbox/" + HostCAName},
		{"the Certificate is Let's Encrypt, anchor there", "", nil, LetsEncryptIssuer, root, "remove the host CA ConfigMap seedbox/" + HostCAName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := newFakeHostCA()
			cl.rootCertificate(root)
			if tc.cert != "" {
				cl.certificate(tc.cert)
			}
			if tc.anchor != nil {
				cl.configMap(tc.anchor)
			}
			caFile := ""
			if tc.caFile != nil {
				caFile = filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(caFile, tc.caFile, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := newHostCA(t, cl, tc.issuer, caFile).Plan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

// An old platform has no root: the run says which command makes one.
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

// The upgrade decision: is the Secret's certificate issued by the root?
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

// The one reader keeps CERTIFICATE blocks and nothing else, so no key reaches the
// anchor.
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
