package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/kube"
)

// fakeCertCluster is the per-host flow's cluster: one Certificate's status,
// whether the CRD is served, and a record of every write and its manager in
// order.
type fakeCertCluster struct {
	ready    bool
	reason   string
	serves   bool
	deny     string
	writes   [][]map[string]any
	managers []string
	allowed  []Access
	reads    int
}

func (f *fakeCertCluster) Allowed(_ context.Context, a Access) (bool, error) {
	f.allowed = append(f.allowed, a)
	if a.Resource == f.deny {
		return false, nil
	}
	return true, nil
}

func (f *fakeCertCluster) PrimaryInternalIP(context.Context) (string, error) {
	return "192.0.2.10", nil
}

func (f *fakeCertCluster) Apply(ctx context.Context, docs []map[string]any) error {
	return f.ApplyWith(ctx, kube.FieldManager, docs)
}

func (f *fakeCertCluster) ApplyWith(_ context.Context, manager string, docs []map[string]any) error {
	cp := make([]map[string]any, len(docs))
	copy(cp, docs)
	f.writes = append(f.writes, cp)
	f.managers = append(f.managers, manager)
	return nil
}

func (f *fakeCertCluster) Await(_ context.Context, w Wait) error {
	if w.Kind != WaitCertificate {
		return nil
	}
	f.reads++
	if f.ready {
		return nil
	}
	return ErrNotReady
}

func (f *fakeCertCluster) CertificateReady(context.Context, string, string) (bool, string, error) {
	if f.ready {
		return true, "", nil
	}
	return false, f.reason, nil
}

func (f *fakeCertCluster) Serves(context.Context, string, string, string) (bool, error) {
	return f.serves, nil
}

func TestCertificateNames(t *testing.T) {
	cases := map[string]string{
		"box.example.test":  "box-example-test",
		"Box.Example.Test":  "box-example-test",
		"box_1.example":     "box-1-example",
		"a.b.c.example.com": "a-b-c-example-com",
	}
	for host, want := range cases {
		if got := dnsLabel(host); got != want {
			t.Errorf("dnsLabel(%q) = %q, want %q", host, got, want)
		}
	}
	if CertificateName("box.example.test") != "s5n-tls-box-example-test" {
		t.Errorf("CertificateName = %q", CertificateName("box.example.test"))
	}
	if ListenerName("box.example.test") != "https-box-example-test" {
		t.Errorf("ListenerName = %q", ListenerName("box.example.test"))
	}
	if HTTPListenerName("box.example.test") != "http-box-example-test" {
		t.Errorf("HTTPListenerName = %q", HTTPListenerName("box.example.test"))
	}
	if tlsManager("box.example.test") != "box-tls-box-example-test" {
		t.Errorf("tlsManager = %q", tlsManager("box.example.test"))
	}
}

// The listener is a complete item on its own: a partial server-side apply
// merges it into the Gateway's keyed listener list without touching the http
// listener, and it admits the seedbox's routes.
func TestHTTPSListener(t *testing.T) {
	l := httpsListener("box.example.test", "s5n-tls-box-example-test")
	if l["name"] != "https-box-example-test" || l["hostname"] != "box.example.test" {
		t.Fatalf("listener = %v", l)
	}
	if l["port"] != int64(443) || l["protocol"] != "HTTPS" {
		t.Fatalf("listener = %v", l)
	}
	tlsSpec, _ := l["tls"].(map[string]any)
	refs, _ := tlsSpec["certificateRefs"].([]any)
	ref, _ := refs[0].(map[string]any)
	if tlsSpec["mode"] != "Terminate" || ref["kind"] != "Secret" || ref["name"] != "s5n-tls-box-example-test" {
		t.Fatalf("tls = %v", tlsSpec)
	}
	allowed, _ := l["allowedRoutes"].(map[string]any)
	ns, _ := allowed["namespaces"].(map[string]any)
	if ns["from"] != "All" {
		t.Fatalf("allowedRoutes = %v, want every namespace (the seedbox's routes)", allowed)
	}
}

// The host's port-80 listener is a complete item: same port as the platform's
// generic listener, matched by hostname so isolation keeps the two apart, and
// narrowed to the Gateway's namespace so app routes cannot attach to it.
func TestHTTPHostListener(t *testing.T) {
	l := httpHostListener("box.example.test")
	if l["name"] != "http-box-example-test" || l["hostname"] != "box.example.test" {
		t.Fatalf("listener = %v", l)
	}
	if l["port"] != int64(80) || l["protocol"] != "HTTP" {
		t.Fatalf("listener = %v", l)
	}
	allowed, _ := l["allowedRoutes"].(map[string]any)
	ns, _ := allowed["namespaces"].(map[string]any)
	if ns["from"] != "Selector" {
		t.Fatalf("allowedRoutes = %v, want a namespace selector", allowed)
	}
	selector, _ := ns["selector"].(map[string]any)
	labels, _ := selector["matchLabels"].(map[string]any)
	if labels["kubernetes.io/metadata.name"] != GatewayNamespace {
		t.Fatalf("selector = %v", selector)
	}
	if _, ok := l["tls"]; ok {
		t.Fatal("the port-80 listener carries TLS")
	}
}

func TestGatewayPartial(t *testing.T) {
	doc := gatewayPartial(map[string]any{"listeners": []any{httpsListener("box.example.test", "s5n-tls-box-example-test")}})
	if doc["apiVersion"] != "gateway.networking.k8s.io/v1" || doc["kind"] != "Gateway" {
		t.Fatalf("identity = %v %v", doc["apiVersion"], doc["kind"])
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta["name"] != GatewayName || meta["namespace"] != GatewayNamespace {
		t.Fatalf("metadata = %v", meta)
	}
	if _, ok := meta["labels"]; ok {
		t.Fatal("a partial apply sets labels it does not own")
	}
}

// The flow's order is the design: the Certificate first, then (only once it
// is Ready) this host's listeners, then the redirect. The listener write uses
// the host's own manager and carries both listeners; the shared, generic http
// listener is never in it.
func TestCertificateApply(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true}
	d := &CertificateDecision{
		Host:    "box.example.test",
		Issuer:  LetsEncryptIssuer,
		Open:    func() (CertificateCluster, error) { return cl, nil },
		Timeout: time.Second,
	}
	res, err := d.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || !res.Listener || !res.Redirect {
		t.Fatalf("result = %+v", res)
	}
	if len(cl.writes) != 3 {
		t.Fatalf("writes = %d, want Certificate, listeners, redirect", len(cl.writes))
	}
	if kind, ns, name := objectMeta(cl.writes[0][0]); kind != "Certificate" || ns != GatewayNamespace || name != "s5n-tls-box-example-test" {
		t.Fatalf("first write = %s %s/%s", kind, ns, name)
	}
	if kind, _, _ := objectMeta(cl.writes[1][0]); kind != "Gateway" {
		t.Fatalf("second write = %s, want the Gateway listeners", kind)
	}
	if cl.managers[1] != "box-tls-box-example-test" {
		t.Fatalf("listener manager = %q", cl.managers[1])
	}
	if kind, ns, name := objectMeta(cl.writes[2][0]); kind != "HTTPRoute" || ns != GatewayNamespace || name != "s5n-tls-box-example-test-redirect" {
		t.Fatalf("third write = %s %s/%s", kind, ns, name)
	}
	if cl.managers[2] != kube.FieldManager {
		t.Fatalf("redirect manager = %q", cl.managers[2])
	}
	// The Certificate carries the issuer init chose and the one DNS name.
	certSpec, _ := cl.writes[0][0]["spec"].(map[string]any)
	issuer, _ := certSpec["issuerRef"].(map[string]any)
	names, _ := certSpec["dnsNames"].([]any)
	if issuer["name"] != LetsEncryptIssuer || issuer["kind"] != "ClusterIssuer" || names[0] != "box.example.test" {
		t.Fatalf("certificate spec = %v", certSpec)
	}
	// One Gateway write, two complete listeners: HTTPS on 443 for every
	// namespace, and this host's own port-80 listener for the Gateway's.
	got := listenersOf(t, cl.writes[1][0])
	if len(got) != 2 {
		t.Fatalf("listener write carries %d listeners, want two: %v", len(got), got)
	}
	if _, ok := got["http"]; ok {
		t.Fatal("the TLS apply wrote the platform's generic http listener")
	}
	https, ok := got["https-box-example-test"]
	if !ok {
		t.Fatalf("listeners = %v, want https-box-example-test", got)
	}
	if https["port"] != int64(443) || https["protocol"] != "HTTPS" || https["hostname"] != "box.example.test" {
		t.Fatalf("https listener = %v", https)
	}
	httpL, ok := got["http-box-example-test"]
	if !ok {
		t.Fatalf("listeners = %v, want http-box-example-test", got)
	}
	if httpL["port"] != int64(80) || httpL["protocol"] != "HTTP" || httpL["hostname"] != "box.example.test" {
		t.Fatalf("http listener = %v", httpL)
	}
	// The redirect names the host's own port-80 listener, not the shared one.
	routeSpec, _ := cl.writes[2][0]["spec"].(map[string]any)
	parentRefs, _ := routeSpec["parentRefs"].([]any)
	ref, _ := parentRefs[0].(map[string]any)
	if ref["sectionName"] != "http-box-example-test" {
		t.Fatalf("redirect sectionName = %v, want http-box-example-test", ref["sectionName"])
	}
}

// Every Gateway field is owned by exactly one box manager: the platform's box
// manager owns the generic http listener, and each host's manager owns only
// that host's two listeners. A TLS apply that carried "http" would make two
// managers flip the shared listener on every bootstrap.
func TestTLSApplyNeverTouchesSharedListener(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	tlsApplies := 0
	for i, docs := range cl.writes {
		if kind, _, _ := objectMeta(docs[0]); kind != "Gateway" {
			continue
		}
		if cl.managers[i] != tlsManager("box.example.test") {
			t.Fatalf("Gateway write %d under manager %q", i, cl.managers[i])
		}
		tlsApplies++
		for name := range listenersOf(t, docs[0]) {
			if name == "http" {
				t.Fatal("a TLS apply wrote the shared http listener")
			}
			if name != "http-box-example-test" && name != "https-box-example-test" {
				t.Fatalf("a TLS apply wrote %q, which is not this host's", name)
			}
		}
	}
	if tlsApplies != 1 {
		t.Fatalf("TLS applies = %d, want one Gateway write", tlsApplies)
	}
	for _, m := range cl.managers {
		if m == "box-tls-http" {
			t.Fatal("the shared listener has its own manager again")
		}
	}
}

// listenersOf indexes a partial Gateway document's listeners by name.
func listenersOf(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	spec, _ := doc["spec"].(map[string]any)
	raw, _ := spec["listeners"].([]any)
	out := make(map[string]map[string]any, len(raw))
	for _, l := range raw {
		m, _ := l.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" {
			t.Fatalf("listener without a name: %v", m)
		}
		if _, dup := out[name]; dup {
			t.Fatalf("listener %q twice", name)
		}
		out[name] = m
	}
	return out
}

func TestCertificateNotReadyWritesNothingElse(t *testing.T) {
	cl := &fakeCertCluster{ready: false, reason: "Issuing: waiting for HTTP-01 challenge", serves: true}
	d := &CertificateDecision{
		Host:    "box.example.test",
		Issuer:  SelfSignedIssuer,
		Open:    func() (CertificateCluster, error) { return cl, nil },
		Timeout: time.Millisecond,
	}
	res, err := d.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Listener || res.Redirect {
		t.Fatalf("result = %+v", res)
	}
	if len(cl.writes) != 1 {
		t.Fatalf("writes = %d, want only the Certificate", len(cl.writes))
	}
	if !strings.Contains(res.Reason, "HTTP-01") {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// The RBAC gate fires before the first write, and names what is missing.
func TestCertificateRefusesWithoutAccess(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true, deny: "certificates"}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	_, err := d.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cert-manager Certificates") {
		t.Fatalf("err = %v", err)
	}
	if len(cl.writes) != 0 {
		t.Fatalf("an RBAC refusal wrote %d objects", len(cl.writes))
	}
	if cl.reads != 0 {
		t.Fatal("an RBAC refusal waited on a Certificate")
	}
}

// Without the Certificate CRD, init says so and leaves the seedbox alone
// rather than failing a box whose platform never installed cert-manager.
func TestCertificateWithoutCertManager(t *testing.T) {
	cl := &fakeCertCluster{serves: false}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	res, err := d.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.writes) != 0 || res.Ready {
		t.Fatalf("result = %+v, writes = %d", res, len(cl.writes))
	}
}

func TestRedirectManifest(t *testing.T) {
	docs, err := ParseManifests(redirectManifest("box.example.test", "s5n-tls-box-example-test"))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := docs[0]["spec"].(map[string]any)
	parentRefs, _ := spec["parentRefs"].([]any)
	ref, _ := parentRefs[0].(map[string]any)
	if ref["name"] != GatewayName || ref["sectionName"] != "http-box-example-test" {
		t.Fatalf("parentRefs = %v, want the host's port-80 listener", parentRefs)
	}
	rules, _ := spec["rules"].([]any)
	rule, _ := rules[0].(map[string]any)
	filters, _ := rule["filters"].([]any)
	filter, _ := filters[0].(map[string]any)
	redirect, _ := filter["requestRedirect"].(map[string]any)
	if filter["type"] != "RequestRedirect" || redirect["scheme"] != "https" || redirect["statusCode"] != float64(301) {
		t.Fatalf("filters = %v", filters)
	}
}

// init's file values and the ClusterIssuer names are two vocabularies; the
// mapping is one place so a typo cannot ship a Certificate naming nothing.
func TestClusterIssuerName(t *testing.T) {
	cases := map[string]string{
		"letsencrypt":     LetsEncryptIssuer,
		"selfsigned":      SelfSignedIssuer,
		LetsEncryptIssuer: LetsEncryptIssuer,
		SelfSignedIssuer:  SelfSignedIssuer,
	}
	for in, want := range cases {
		got, err := clusterIssuerName(in)
		if err != nil || got != want {
			t.Errorf("clusterIssuerName(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := clusterIssuerName("acme"); err == nil {
		t.Fatal("an unknown issuer was accepted")
	}
}
