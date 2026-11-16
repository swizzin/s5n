package platform

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/say"
)

// fakeCertCluster is the per-host flow's cluster: one Certificate's status,
// whether the CRD is served, the objects box reads by name, and a record of
// every write and its manager in order.
type fakeCertCluster struct {
	ready    bool
	reason   string
	serves   bool
	deny     string
	writes   [][]map[string]any
	managers []string
	allowed  []Access
	reads    int
	// objects are what Get answers, by "kind namespace/name".
	objects map[string]map[string]any
	// statusWrites records every status update, and onStatus lets a test
	// answer one the way cert-manager would: with a new Secret.
	statusWrites []map[string]any
	onStatus     func(*fakeCertCluster)
}

func objectKey(kind, namespace, name string) string {
	return kind + " " + namespace + "/" + name
}

// put records one object for Get.
func (f *fakeCertCluster) put(kind, namespace, name string, obj map[string]any) {
	if f.objects == nil {
		f.objects = map[string]map[string]any{}
	}
	f.objects[objectKey(kind, namespace, name)] = obj
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

func (f *fakeCertCluster) Get(_ context.Context, _, _, kind, namespace, name string) (map[string]any, bool, error) {
	obj, ok := f.objects[objectKey(kind, namespace, name)]
	return obj, ok, nil
}

func (f *fakeCertCluster) UpdateStatus(_ context.Context, _, _, kind, namespace string, obj map[string]any) error {
	f.statusWrites = append(f.statusWrites, obj)
	f.put(kind, namespace, obj["metadata"].(map[string]any)["name"].(string), obj)
	if f.onStatus != nil {
		f.onStatus(f)
	}
	return nil
}

func TestCertificateNames(t *testing.T) {
	// Hosts whose flattening loses nothing keep the readable label, so the
	// names hack/e2e-box.sh asserts on stay put.
	cases := map[string]string{
		"box.example.test":  "box-example-test",
		"Box.Example.Test":  "box-example-test",
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

// hashedLabel recognises the disambiguating suffix a lossy host carries.
var hashedLabel = regexp.MustCompile(`-[0-9a-f]{8}$`)

// Two hosts that flatten to one label must not share a name: sharing one
// would let either host's Certificate, listener, SecurityPolicy or field
// manager overwrite the other's.
func TestDNSLabelDisambiguatesCollidingHosts(t *testing.T) {
	longPrefix := strings.Repeat("a", 100)
	pairs := [][2]string{
		{"a-b.example.com", "a.b.example.com"},
		{"*.example.com", "example.com"},
		{"a--b.example.com", "a-b.example.com"},
		{longPrefix + "one", longPrefix + "two"},
	}
	for _, pair := range pairs {
		first, second := dnsLabel(pair[0]), dnsLabel(pair[1])
		if first == second {
			t.Errorf("dnsLabel(%q) and dnsLabel(%q) are both %q", pair[0], pair[1], first)
		}
		for _, host := range pair {
			label := dnsLabel(host)
			if len(label) > maxDNSLabel {
				t.Errorf("dnsLabel(%q) = %q is %d characters", host, label, len(label))
			}
			// The readable flattening is kept wherever it fits; the hash is
			// the only thing the suffix adds.
			flat := flattenHost(strings.ToLower(host))
			if room := maxDNSLabel - hostHashLen - 1; len(flat) <= room {
				if want := flat + label[len(flat):]; label != want {
					t.Errorf("dnsLabel(%q) = %q, want %q", host, label, want)
				}
			}
		}
	}
}

func TestDNSLabelHashedSuffix(t *testing.T) {
	lossy := []string{
		"a-b.example.com",
		"*.example.com",
		"a--b.example.com",
		"box_1.example",
		strings.Repeat("a", 100) + "one",
	}
	for _, host := range lossy {
		label := dnsLabel(host)
		if !hashedLabel.MatchString(label) {
			t.Errorf("dnsLabel(%q) = %q, want an 8-hex suffix", host, label)
		}
		if len(label) > maxDNSLabel {
			t.Errorf("dnsLabel(%q) = %q is %d characters", host, label, len(label))
		}
		// The hash is of the lowercased host: case variants share one name.
		if got, want := dnsLabel(strings.ToUpper(host)), label; got != want {
			t.Errorf("dnsLabel(%q) = %q, dnsLabel(%q) = %q", strings.ToUpper(host), got, host, want)
		}
		if got, want := dnsLabel(host), label; got != want {
			t.Errorf("dnsLabel(%q) is not deterministic: %q then %q", host, got, want)
		}
	}
	// A host longer than the limit keeps the roomiest prefix the suffix
	// leaves, so the name is exactly the limit rather than shorter.
	if got := dnsLabel(strings.Repeat("a", 100) + "one"); len(got) != maxDNSLabel {
		t.Errorf("long host label is %d characters, want %d", len(got), maxDNSLabel)
	}
}

// Every name one host gets must differ from the name the same object type
// gets for a host that flattens to the same readable label, or the two
// hosts' objects and field managers collide.
func TestCollidingHostsGetDistinctObjectNames(t *testing.T) {
	first, second := "a-b.example.com", "a.b.example.com"
	names := []struct {
		name string
		fn   func(string) string
	}{
		{"CertificateName", CertificateName},
		{"ListenerName", ListenerName},
		{"HTTPListenerName", HTTPListenerName},
		{"ListenerPolicyName", ListenerPolicyName},
		{"httpListenerManager", httpListenerManager},
		{"httpsListenerManager", httpsListenerManager},
	}
	for _, n := range names {
		if n.fn(first) == n.fn(second) {
			t.Errorf("%s(%q) == %s(%q) == %q", n.name, first, n.name, second, n.fn(first))
		}
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

// Every name a host produces is a valid object name, even for a host with
// nothing readable in it: no leading or trailing hyphen, lowercase
// alphanumerics and hyphens only.
func TestHostNamesAreValidObjectNames(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, host := range []string{"*", "*.example.com", "-a-.example.com", "box.example.test", strings.Repeat("a-", 80) + "example.com"} {
		label := dnsLabel(host)
		if !valid.MatchString(label) || len(label) > maxDNSLabel {
			t.Errorf("dnsLabel(%q) = %q, not a valid object name of at most %d characters", host, label, maxDNSLabel)
		}
	}
}

// secretDoc is a Secret's object as the dynamic client returns it: base64 data.
func secretDoc(t *testing.T, name, namespace string, kv ...any) map[string]any {
	t.Helper()
	data := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		value, _ := kv[i+1].([]byte)
		data[key] = base64.StdEncoding.EncodeToString(value)
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"data":     data,
	}
}

// A self-signed host certificate from 0.3.0 chains to itself. Changing the
// issuer's spec does not make cert-manager re-issue it, so box asks -- with the
// Issuing condition `cmctl renew` sets -- and reports the new certificate.
func TestSelfSignedCertificateFollowsTheRoot(t *testing.T) {
	root, _ := testCA(t, "s5n box root")
	old, _ := testCA(t, "a bare self-signed leaf")
	name := CertificateName("box.example.test")
	cl := &fakeCertCluster{ready: true, serves: true}
	cl.put("Secret", CertManagerNamespace, RootSecretName, secretDoc(t, RootSecretName, CertManagerNamespace, "tls.crt", root))
	cl.put("Secret", GatewayNamespace, name, secretDoc(t, name, GatewayNamespace, "ca.crt", old, "tls.crt", []byte("old-leaf")))
	cl.put("Certificate", GatewayNamespace, name, map[string]any{
		"metadata": map[string]any{"name": name, "namespace": GatewayNamespace, "generation": int64(7)},
	})
	// cert-manager answers the request the way it would: a new leaf, signed by
	// the root.
	cl.onStatus = func(f *fakeCertCluster) {
		f.put("Secret", GatewayNamespace, name, secretDoc(t, name, GatewayNamespace, "ca.crt", root, "tls.crt", []byte("new-leaf")))
	}

	d := &CertificateDecision{
		Host:    "box.example.test",
		Issuer:  SelfSignedIssuer,
		Open:    func() (CertificateCluster, error) { return cl, nil },
		Timeout: time.Second,
	}
	res, err := d.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reissued {
		t.Fatalf("result = %+v, want a re-issue", res)
	}
	if len(cl.statusWrites) != 1 {
		t.Fatalf("status writes = %d, want one Issuing condition", len(cl.statusWrites))
	}
	conditions, _, _ := unstructured.NestedSlice(cl.statusWrites[0], "status", "conditions")
	if len(conditions) != 1 {
		t.Fatalf("conditions = %v", conditions)
	}
	cond, _ := conditions[0].(map[string]any)
	if cond["type"] != "Issuing" || cond["status"] != "True" {
		t.Fatalf("condition = %v, want Issuing=True", cond)
	}
	if cond["reason"] != "ManuallyTriggered" {
		t.Fatalf("reason = %v, want the reason cmctl renew sets", cond["reason"])
	}
	if cond["observedGeneration"] != int64(7) {
		t.Fatalf("observedGeneration = %v, want the Certificate's generation", cond["observedGeneration"])
	}
	if !res.HTTPS || !res.Redirect {
		t.Fatalf("the host was not served after the re-issue: %+v", res)
	}
}

// The check is for the self-signed issuer only: an ACME certificate has no
// ca.crt and nothing in the box's namespace to chain to.
func TestLetsEncryptCertificateIsNotReissued(t *testing.T) {
	name := CertificateName("box.example.test")
	cl := &fakeCertCluster{ready: true, serves: true}
	cl.put("Secret", GatewayNamespace, name, secretDoc(t, name, GatewayNamespace, "tls.crt", []byte("leaf")))

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
	if res.Reissued || len(cl.statusWrites) != 0 {
		t.Fatalf("an ACME certificate was asked to re-issue: %+v %v", res, cl.statusWrites)
	}
}

// TestCertificateNeedsWork: a host that answers already can still hold a
// certificate this run would not issue -- one the issuer behind it changed
// under, which cert-manager never re-issues on its own. The read has to say so,
// or a run that only looks for missing hosts leaves it serving something the
// anchor does not match.
func TestCertificateNeedsWork(t *testing.T) {
	const host = "box.example.test"
	rootPEM, _ := testCA(t, "the box root")
	leafPEM, _ := testCA(t, host)

	newCluster := func(issuer string, ca []byte) *fakeHostCACluster {
		f := newFakeHostCA()
		f.rootCertificate(rootPEM)
		f.put("Certificate", GatewayNamespace, CertificateName(host), map[string]any{
			"metadata": map[string]any{"name": CertificateName(host), "namespace": GatewayNamespace},
			"spec": map[string]any{
				"issuerRef":  map[string]any{"name": issuer},
				"secretName": CertificateName(host),
			},
		})
		f.put("Secret", GatewayNamespace, CertificateName(host), map[string]any{
			"data": map[string]any{"ca.crt": base64.StdEncoding.EncodeToString(ca)},
		})
		return f
	}
	ctx := context.Background()

	// 0.3.0's shape: a bare selfSigned certificate, its own ca.crt.
	stale, err := CertificateNeedsWork(ctx, newCluster(SelfSignedIssuer, leafPEM), host, "")
	if err != nil {
		t.Fatalf("CertificateNeedsWork: %v", err)
	}
	if !stale {
		t.Fatal("a certificate signed by something other than the box root was called right")
	}

	// The same Certificate after the re-issue.
	stale, err = CertificateNeedsWork(ctx, newCluster(SelfSignedIssuer, rootPEM), host, "")
	if err != nil {
		t.Fatalf("CertificateNeedsWork: %v", err)
	}
	if stale {
		t.Fatal("a certificate chained to the box root was called stale")
	}

	// An issuer this run decided that the Certificate does not name.
	stale, err = CertificateNeedsWork(ctx, newCluster(SelfSignedIssuer, rootPEM), host, "letsencrypt")
	if err != nil {
		t.Fatalf("CertificateNeedsWork: %v", err)
	}
	if !stale {
		t.Fatal("a self-signed certificate was called right on a run that decided Let's Encrypt")
	}

	// No Certificate at all: HostServed's answer, not this one's.
	empty := newFakeHostCA()
	empty.rootCertificate(rootPEM)
	stale, err = CertificateNeedsWork(ctx, empty, host, "")
	if err != nil || stale {
		t.Fatalf("CertificateNeedsWork with no Certificate = %v, %v; want false, nil", stale, err)
	}
}

// batchCertCluster is the fake for a run over several names: it answers per
// Certificate name, and its Await calls only finish once every name's has
// started. A run that waits on the hosts one after another never gets past the
// first, so the fake asserts the shape of the wait -- one round over all the
// names under one budget -- rather than a stopwatch. The five-second fallback
// is only there to turn a serial run into a failure instead of a hang.
type batchCertCluster struct {
	*fakeCertCluster
	mu      sync.Mutex
	want    int
	ready   map[string]bool
	reason  string
	awaits  []Wait
	entered int
	allIn   chan struct{}
}

func newBatchCertCluster(want int, ready map[string]bool, reason string) *batchCertCluster {
	return &batchCertCluster{
		fakeCertCluster: &fakeCertCluster{serves: true},
		want:            want,
		ready:           ready,
		reason:          reason,
		allIn:           make(chan struct{}),
	}
}

func (f *batchCertCluster) Await(_ context.Context, w Wait) error {
	f.mu.Lock()
	f.awaits = append(f.awaits, w)
	f.entered++
	if f.entered == f.want {
		close(f.allIn)
	}
	f.mu.Unlock()
	select {
	case <-f.allIn:
	case <-time.After(5 * time.Second):
		return errors.New("the Certificate waits ran one after another: they never overlapped")
	}
	if f.ready[w.Name] {
		return nil
	}
	return fmt.Errorf("%w after %s", ErrNotReady, w.Timeout)
}

func (f *batchCertCluster) CertificateReady(_ context.Context, _, name string) (bool, string, error) {
	if f.ready[name] {
		return true, "", nil
	}
	return false, f.reason, nil
}

// certWaitHosts is four names: one that issues and three that never do.
func certWaitHosts() []CertificateRequest {
	return []CertificateRequest{
		{Host: "box.example.test", Issuer: LetsEncryptIssuer},
		{Host: "gamma.box.example.test", Issuer: LetsEncryptIssuer},
		{Host: "movies.example.test", Issuer: LetsEncryptIssuer},
		{Host: "tv.example.test", Issuer: LetsEncryptIssuer},
	}
}

// One wait per run, whatever the number of names. Three of four Certificates
// never become Ready: every Certificate is written before the wait starts, the
// wait is one round over all four under one Timeout (the fake's calls overlap,
// so a per-host wait cannot pass), the Ready host is served, and the other
// three get the warning and no HTTPS listener, in host order.
func TestCertificatesWaitOnceForTheRun(t *testing.T) {
	hosts := certWaitHosts()
	ready := map[string]bool{CertificateName(hosts[0].Host): true}
	cl := newBatchCertCluster(len(hosts), ready, "Issuing: waiting for HTTP-01 challenge")
	var out strings.Builder
	set := &Certificates{
		Hosts:   hosts,
		Open:    func() (CertificateCluster, error) { return cl, nil },
		Say:     &say.Plain{W: &out, Err: &out},
		Timeout: 250 * time.Millisecond,
	}
	results, err := set.Apply(context.Background())
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	assertOneRunWait(t, cl, len(hosts), out.String())
	assertOnlyTheReadyHostIsServed(t, results)
	assertWarningsInHostOrder(t, out.String(), results[1:])
	if https, redirects := servesWritten(t, cl); https != 1 || redirects != 1 {
		t.Fatalf("https listeners = %d, redirects = %d; want only the Ready host's", https, redirects)
	}
}

// assertOneRunWait is the shape of the whole run: one Await per host under the
// one budget, one wait line, and every Certificate applied before it.
func assertOneRunWait(t *testing.T, cl *batchCertCluster, hosts int, log string) {
	t.Helper()
	if len(cl.awaits) != hosts {
		t.Fatalf("awaits = %d, want one per host (%d): %+v", len(cl.awaits), hosts, cl.awaits)
	}
	for _, w := range cl.awaits {
		if w.Kind != WaitCertificate || w.Timeout != 250*time.Millisecond {
			t.Fatalf("await = %+v, want the run's one Certificate budget", w)
		}
	}
	if got := strings.Count(log, "Certificates to be Ready"); got != 1 {
		t.Fatalf("the run printed %d Certificate wait lines, want one:\n%s", got, log)
	}
	if want := fmt.Sprintf("waiting up to 250ms for %d Certificates to be Ready", hosts); !strings.Contains(log, want) {
		t.Fatalf("the wait does not name the run's budget and count:\n%s", log)
	}
	if want := fmt.Sprintf("1 of %d Certificates are Ready", hosts); !strings.Contains(log, want) {
		t.Fatalf("the wait's Done line does not say how many came Ready:\n%s", log)
	}
	lastApply := strings.LastIndex(log, "applied Certificate ")
	if lastApply < 0 || lastApply > strings.Index(log, "Certificates to be Ready") {
		t.Fatalf("the wait started before every Certificate was applied:\n%s", log)
	}
}

// assertOnlyTheReadyHostIsServed is the first result served and the other three
// left with nothing but the port-80 listener that answers the challenge.
func assertOnlyTheReadyHostIsServed(t *testing.T, results []CertificateResult) {
	t.Helper()
	if !results[0].Ready || !results[0].HTTPS || !results[0].Redirect {
		t.Fatalf("result 0 = %+v, want the Ready host served", results[0])
	}
	for i, res := range results[1:] {
		if res.Ready || res.HTTPS || res.Redirect {
			t.Fatalf("result %d = %+v, want nothing served", i+1, res)
		}
		if !strings.Contains(res.Reason, "HTTP-01") {
			t.Fatalf("result %d reason = %q", i+1, res.Reason)
		}
	}
}

// assertWarningsInHostOrder is the per-host sentence for each Certificate that
// did not issue, printed once and in the order the run decided the hosts.
func assertWarningsInHostOrder(t *testing.T, log string, results []CertificateResult) {
	t.Helper()
	at := -1
	for _, res := range results {
		want := fmt.Sprintf("certificate %s/%s is not Ready (Issuing: waiting for HTTP-01 challenge): %s is not served until it is; only the ACME challenge answers on port 80; box retries on the next run", GatewayNamespace, res.Name, res.Host)
		i := strings.Index(log, want)
		if i < 0 {
			t.Fatalf("missing warning for %s:\n%s", res.Host, log)
		}
		if i <= at {
			t.Fatalf("the warning for %s is out of host order:\n%s", res.Host, log)
		}
		if strings.Contains(log, "certificate "+GatewayNamespace+"/"+res.Name+" is Ready") {
			t.Fatalf("a Certificate that never issued was reported Ready:\n%s", log)
		}
		at = i
	}
}

// servesWritten counts the HTTPS listeners and redirects the run wrote: one of
// each is one host served.
func servesWritten(t *testing.T, cl *batchCertCluster) (https, redirects int) {
	t.Helper()
	for _, docs := range cl.writes {
		for _, doc := range docs {
			kind, _, _ := objectMeta(doc)
			switch kind {
			case "Gateway":
				for name := range listenersOf(t, doc) {
					if strings.HasPrefix(name, "https-") {
						https++
					}
				}
			case "HTTPRoute":
				redirects++
			}
		}
	}
	return https, redirects
}

// All Ready on the first read: each Certificate is read once, the run waits no
// further, and every host is served in host order.
func TestCertificatesReadyOnTheFirstReadWaitOnce(t *testing.T) {
	hosts := certWaitHosts()
	ready := map[string]bool{}
	for _, h := range hosts {
		ready[CertificateName(h.Host)] = true
	}
	cl := newBatchCertCluster(len(hosts), ready, "")
	var out strings.Builder
	set := &Certificates{
		Hosts:   hosts,
		Open:    func() (CertificateCluster, error) { return cl, nil },
		Say:     &say.Plain{W: &out, Err: &out},
		Timeout: time.Minute,
	}
	results, err := set.Apply(context.Background())
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if len(cl.awaits) != len(hosts) {
		t.Fatalf("reads = %d, want one per Certificate (%d): a Ready one answered after the first poll", len(cl.awaits), len(hosts))
	}
	log := out.String()
	if !strings.Contains(log, "4 of 4 Certificates are Ready") {
		t.Fatalf("the wait's Done line does not say all four came Ready:\n%s", log)
	}
	if strings.Contains(log, "is not Ready") {
		t.Fatalf("a Ready run warned about a Certificate:\n%s", log)
	}
	at := -1
	for i, res := range results {
		if !res.Ready || !res.HTTPS || !res.Redirect {
			t.Fatalf("result %d = %+v, want served", i, res)
		}
		line := fmt.Sprintf("certificate %s/%s is Ready", GatewayNamespace, res.Name)
		j := strings.Index(log, line)
		if j < 0 {
			t.Fatalf("missing success line for %s:\n%s", res.Host, log)
		}
		if j <= at {
			t.Fatalf("the success lines are out of host order:\n%s", log)
		}
		at = j
	}
}
