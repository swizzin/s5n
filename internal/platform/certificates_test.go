package platform

import (
	"context"
	"reflect"
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
	if httpListenerManager("box.example.test") != "box-tls-box-example-test-http" {
		t.Errorf("httpListenerManager = %q", httpListenerManager("box.example.test"))
	}
	if httpsListenerManager("box.example.test") != "box-tls-box-example-test-https" {
		t.Errorf("httpsListenerManager = %q", httpsListenerManager("box.example.test"))
	}
	if ListenerPolicyName("box.example.test") != "s5n-no-login-box-example-test" {
		t.Errorf("ListenerPolicyName = %q", ListenerPolicyName("box.example.test"))
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

// The flow's order is the design: the host's port-80 listener, then the Allow
// that keeps the login off it, then the Certificate, then -- only once it is
// Ready -- both listeners (https included) and the redirect. The listener
// writes use the host's own manager and never carry the shared, generic http
// listener.
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
	if !res.Ready || !res.Listener || !res.Policy || !res.HTTPS || !res.Redirect {
		t.Fatalf("result = %+v", res)
	}
	if len(cl.writes) != 5 {
		t.Fatalf("writes = %d, want port-80 listener, policy, Certificate, listeners, redirect", len(cl.writes))
	}
	if kind, ns, name := objectMeta(cl.writes[0][0]); kind != "Gateway" || ns != GatewayNamespace || name != GatewayName {
		t.Fatalf("first write = %s %s/%s, want the port-80 listener", kind, ns, name)
	}
	first := listenersOf(t, cl.writes[0][0])
	if len(first) != 1 {
		t.Fatalf("first Gateway write carries %d listeners, want only http-box-example-test: %v", len(first), first)
	}
	if l, ok := first["http-box-example-test"]; !ok || l["port"] != int64(80) {
		t.Fatalf("first Gateway write = %v, want the host's port-80 listener alone", first)
	}
	if cl.managers[0] != "box-tls-box-example-test-http" {
		t.Fatalf("port-80 listener manager = %q", cl.managers[0])
	}
	if kind, ns, name := objectMeta(cl.writes[1][0]); kind != "SecurityPolicy" || ns != GatewayNamespace || name != "s5n-no-login-box-example-test" {
		t.Fatalf("second write = %s %s/%s, want the port-80 Allow", kind, ns, name)
	}
	if cl.managers[1] != "box-tls-box-example-test-http" {
		t.Fatalf("policy manager = %q, want the port-80 listener's own manager", cl.managers[1])
	}
	if kind, ns, name := objectMeta(cl.writes[2][0]); kind != "Certificate" || ns != GatewayNamespace || name != "s5n-tls-box-example-test" {
		t.Fatalf("third write = %s %s/%s, want the Certificate", kind, ns, name)
	}
	if kind, _, _ := objectMeta(cl.writes[3][0]); kind != "Gateway" {
		t.Fatalf("fourth write = %s, want the HTTPS listener", kind)
	}
	if kind, ns, name := objectMeta(cl.writes[4][0]); kind != "HTTPRoute" || ns != GatewayNamespace || name != "s5n-tls-box-example-test-redirect" {
		t.Fatalf("fifth write = %s %s/%s", kind, ns, name)
	}
	if cl.managers[4] != kube.FieldManager {
		t.Fatalf("redirect manager = %q", cl.managers[4])
	}
	// The Certificate carries the issuer init chose and the one DNS name.
	certSpec, _ := cl.writes[2][0]["spec"].(map[string]any)
	issuer, _ := certSpec["issuerRef"].(map[string]any)
	names, _ := certSpec["dnsNames"].([]any)
	if issuer["name"] != LetsEncryptIssuer || issuer["kind"] != "ClusterIssuer" || names[0] != "box.example.test" {
		t.Fatalf("certificate spec = %v", certSpec)
	}
	// The fourth write is the HTTPS listener alone, under its own manager:
	// the two listeners never share a manager, so neither apply can remove
	// the other by omitting it.
	got := listenersOf(t, cl.writes[3][0])
	if len(got) != 1 {
		t.Fatalf("HTTPS write carries %d listeners, want one: %v", len(got), got)
	}
	if _, ok := got["http"]; ok {
		t.Fatal("the TLS apply wrote the platform's generic http listener")
	}
	if _, ok := got["http-box-example-test"]; ok {
		t.Fatal("the TLS apply rewrote the host's port-80 listener: it has its own manager")
	}
	if cl.managers[3] != "box-tls-box-example-test-https" {
		t.Fatalf("HTTPS listener manager = %q", cl.managers[3])
	}
	https, ok := got["https-box-example-test"]
	if !ok {
		t.Fatalf("listeners = %v, want https-box-example-test", got)
	}
	if https["port"] != int64(443) || https["protocol"] != "HTTPS" || https["hostname"] != "box.example.test" {
		t.Fatalf("https listener = %v", https)
	}
	// The redirect names the host's own port-80 listener, not the shared one.
	routeSpec, _ := cl.writes[4][0]["spec"].(map[string]any)
	parentRefs, _ := routeSpec["parentRefs"].([]any)
	ref, _ := parentRefs[0].(map[string]any)
	if ref["sectionName"] != "http-box-example-test" {
		t.Fatalf("redirect sectionName = %v, want http-box-example-test", ref["sectionName"])
	}
}

// The port-80 Allow is one object with one job: this host's http-<host>
// listener, an explicit Allow, and no extAuth at all. Anything more (a
// Gateway-wide target, an extAuth left in place, a rule that denies
// something) would either put the login back on the listener or narrow the
// HTTPS side.
func TestListenerPolicyManifest(t *testing.T) {
	docs, err := ParseManifests(listenerPolicyManifest("box.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("manifest carries %d documents", len(docs))
	}
	doc := docs[0]
	if kind, ns, name := objectMeta(doc); kind != "SecurityPolicy" || ns != GatewayNamespace || name != "s5n-no-login-box-example-test" {
		t.Fatalf("object = %s %s/%s", kind, ns, name)
	}
	spec, _ := doc["spec"].(map[string]any)
	for key := range spec {
		if key != "targetRefs" && key != "authorization" {
			t.Fatalf("spec carries %q: the Allow is targetRefs and authorization only", key)
		}
	}
	if _, ok := spec["extAuth"]; ok {
		t.Fatal("the port-80 policy carries extAuth: the login is exactly what it must not have")
	}
	refs, _ := spec["targetRefs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("targetRefs = %v, want the one listener", refs)
	}
	ref, _ := refs[0].(map[string]any)
	if ref["group"] != "gateway.networking.k8s.io" || ref["kind"] != "Gateway" || ref["name"] != GatewayName {
		t.Fatalf("targetRefs = %v", refs)
	}
	if ref["sectionName"] != "http-box-example-test" {
		t.Fatalf("sectionName = %v, want the host's port-80 listener, not the shared one", ref["sectionName"])
	}
	auth, _ := spec["authorization"].(map[string]any)
	if auth["defaultAction"] != "Allow" {
		t.Fatalf("authorization = %v, want defaultAction: Allow", auth)
	}
	if _, ok := auth["rules"]; ok {
		t.Fatal("the port-80 policy carries rules: there is nothing to deny on a listener only root's routes attach to")
	}
}

// The policy is written only after the listener it names exists -- a
// sectionName that resolves to nothing is an error Envoy Gateway reports
// instead of an Allow -- and only on that path: a Certificate that is not
// Ready writes no policy at all.
func TestListenerPolicyFollowsItsListener(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	listenerAt, policyAt := -1, -1
	for i, docs := range cl.writes {
		kind, ns, _ := objectMeta(docs[0])
		switch kind {
		case "Gateway":
			if _, ok := listenersOf(t, docs[0])["http-box-example-test"]; ok && listenerAt < 0 {
				listenerAt = i
			}
		case "SecurityPolicy":
			if ns != GatewayNamespace {
				t.Fatalf("SecurityPolicy %d is in %s, want %s", i, ns, GatewayNamespace)
			}
			if policyAt >= 0 {
				t.Fatal("the policy was written twice in one apply")
			}
			policyAt = i
		}
	}
	if listenerAt < 0 {
		t.Fatal("the apply wrote no http-box-example-test listener")
	}
	if policyAt < 0 {
		t.Fatal("the apply wrote no port-80 Allow")
	}
	if policyAt <= listenerAt {
		t.Fatalf("policy written at %d, its listener at %d: the sectionName has to exist first", policyAt, listenerAt)
	}
}

// Every Gateway field is owned by exactly one box manager: the platform's box
// manager owns the generic http listener, and each host's listeners have a
// manager each -- one for port 80, one for 443. Sharing one manager between
// them would mean every apply has to restate both, and the port-80 apply (made
// while the certificate is still issuing, with no HTTPS listener in existence)
// would delete the HTTPS listener of an already-established box on every
// re-run. A TLS apply that carried "http" would make two managers flip the
// shared listener on every bootstrap.
func TestEachListenerHasItsOwnManager(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[int]struct {
		manager   string
		listeners []string
	}{
		0: {httpListenerManager("box.example.test"), []string{"http-box-example-test"}},
		3: {httpsListenerManager("box.example.test"), []string{"https-box-example-test"}},
	}
	for i, docs := range cl.writes {
		if kind, _, _ := objectMeta(docs[0]); kind != "Gateway" {
			continue
		}
		w, ok := want[i]
		if !ok {
			t.Fatalf("unexpected Gateway write at %d", i)
		}
		if cl.managers[i] != w.manager {
			t.Fatalf("Gateway write %d under manager %q, want %q", i, cl.managers[i], w.manager)
		}
		got := listenersOf(t, docs[0])
		if len(got) != len(w.listeners) {
			t.Fatalf("Gateway write %d carries %v, want %v", i, got, w.listeners)
		}
		for _, name := range w.listeners {
			if _, ok := got[name]; !ok {
				t.Fatalf("Gateway write %d carries %v, want %v", i, got, w.listeners)
			}
		}
		if _, ok := got["http"]; ok {
			t.Fatal("a per-host apply wrote the shared http listener")
		}
	}
	if httpListenerManager("box.example.test") == httpsListenerManager("box.example.test") {
		t.Fatal("both listeners share one manager: an apply that names one would remove the other")
	}
	if cl.managers[1] != httpListenerManager("box.example.test") {
		t.Fatalf("the port-80 Allow is written under %q, want the port-80 manager", cl.managers[1])
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

// A certificate that is not Ready leaves the host unserved: port 80 answers
// the challenge and nothing else -- no HTTPS listener, no redirect -- because
// the redirect would send browsers to a listener that does not exist. The
// port-80 listener and its Allow do stay: they are what lets cert-manager try
// again on the next run.
func TestCertificateNotReadyWritesOnlyPort80(t *testing.T) {
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
	if res.Ready || res.HTTPS || res.Redirect {
		t.Fatalf("result = %+v, want nothing served", res)
	}
	if !res.Listener || !res.Policy {
		t.Fatalf("result = %+v, want the port-80 listener and its Allow to stay", res)
	}
	if len(cl.writes) != 3 {
		t.Fatalf("writes = %d, want the port-80 listener, its Allow and the Certificate", len(cl.writes))
	}
	for i, docs := range cl.writes {
		kind, _, name := objectMeta(docs[0])
		switch kind {
		case "Certificate":
			if name != "s5n-tls-box-example-test" {
				t.Fatalf("write %d = %s, want the Certificate", i, name)
			}
		case "SecurityPolicy":
			spec, _ := docs[0]["spec"].(map[string]any)
			refs, _ := spec["targetRefs"].([]any)
			ref, _ := refs[0].(map[string]any)
			if ref["sectionName"] != "http-box-example-test" {
				t.Fatalf("the Allow names %v", ref["sectionName"])
			}
		case "Gateway":
			got := listenersOf(t, docs[0])
			if _, ok := got["https-box-example-test"]; ok {
				t.Fatal("a Certificate that is not Ready wrote an HTTPS listener")
			}
			if _, ok := got["http-box-example-test"]; !ok || len(got) != 1 {
				t.Fatalf("write %d carries %v, want only the port-80 listener", i, got)
			}
		case "HTTPRoute":
			t.Fatal("a Certificate that is not Ready wrote the redirect")
		default:
			t.Fatalf("write %d is %s", i, kind)
		}
	}
	if !strings.Contains(res.Reason, "HTTP-01") {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// A second run against a certificate that is already Ready writes the same
// objects in the same order: the TLS half is idempotent, so a box that is
// bootstrapped twice does not churn its listeners.
func TestCertificateApplyIsIdempotent(t *testing.T) {
	cl := &fakeCertCluster{ready: true, serves: true}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := len(cl.writes)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cl.writes) != 2*first {
		t.Fatalf("two runs wrote %d objects, want %d", len(cl.writes), 2*first)
	}
	for i := 0; i < first; i++ {
		a, b := cl.writes[i], cl.writes[i+first]
		if len(a) != len(b) {
			t.Fatalf("write %d: %d documents then %d", i, len(a), len(b))
		}
		for j := range a {
			if !reflect.DeepEqual(a[j], b[j]) {
				t.Fatalf("write %d document %d changed:\n%v\n%v", i, j, a[j], b[j])
			}
		}
		if cl.managers[i] != cl.managers[i+first] {
			t.Fatalf("write %d manager %q then %q", i, cl.managers[i], cl.managers[i+first])
		}
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
