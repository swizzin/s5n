package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// kubeconfigFixture builds a kubeconfig in the exact shape k0s/k3s/kubeadm
// write: one cluster/context/user, inline base64 CA and client cert/key.
func kubeconfigFixture(t *testing.T, server string) []byte {
	t.Helper()
	ca := base64.StdEncoding.EncodeToString([]byte("ca-cert"))
	cert := base64.StdEncoding.EncodeToString([]byte("client-cert"))
	key := base64.StdEncoding.EncodeToString([]byte("client-key"))
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: %s
  name: my-cluster
contexts:
- context:
    cluster: my-cluster
    user: my-user
  name: my-context
current-context: my-context
users:
- name: my-user
  user:
    client-certificate-data: %s
    client-key-data: %s
`, ca, server, cert, key))
}

// kubeconfigFixtureToken is the same shape but with a bearer token instead of
// a client certificate, which the read-only HTTP prober also supports.
func kubeconfigFixtureToken(t *testing.T, server string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: my-cluster
contexts:
- context:
    cluster: my-cluster
    user: my-user
  name: my-context
current-context: my-context
users:
- name: my-user
  user:
    token: abc123
`, server))
}

func TestParseKubeconfig(t *testing.T) {
	kc, err := parseKubeconfig(kubeconfigFixture(t, "https://127.0.0.1:6443"))
	if err != nil {
		t.Fatalf("parseKubeconfig: %v", err)
	}
	if kc.currentContext != "my-context" {
		t.Errorf("current-context = %q", kc.currentContext)
	}
	cl, us, err := kc.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cl.server != "https://127.0.0.1:6443" {
		t.Errorf("server = %q", cl.server)
	}
	if string(cl.caData) != "ca-cert" {
		t.Errorf("caData = %q", cl.caData)
	}
	if string(us.clientCertData) != "client-cert" || string(us.clientKeyData) != "client-key" {
		t.Errorf("client cert/key decoded wrong: %q %q", us.clientCertData, us.clientKeyData)
	}
}

func TestParseKubeconfigSingleContextDefault(t *testing.T) {
	// No current-context, one context: resolve falls back to the only one.
	data := kubeconfigFixture(t, "https://127.0.0.1:6443")
	var out []byte
	for _, line := range splitLines(string(data)) {
		if hasKeyLine(line, "current-context") {
			continue
		}
		out = append(out, []byte(line+"\n")...)
	}
	kc, err := parseKubeconfig(out)
	if err != nil {
		t.Fatalf("parseKubeconfig: %v", err)
	}
	if _, _, err := kc.resolve(); err != nil {
		t.Fatalf("resolve without current-context: %v", err)
	}
}

func TestParseKubeconfigNoCurrentContextMultiple(t *testing.T) {
	// Two contexts and no current-context: resolve must refuse to guess.
	data := `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: a
- cluster:
    server: https://127.0.0.1:6444
  name: b
contexts:
- context:
    cluster: a
    user: u
  name: c1
- context:
    cluster: b
    user: u
  name: c2
users:
- name: u
  user:
    token: t
`
	kc, err := parseKubeconfig([]byte(data))
	if err != nil {
		t.Fatalf("parseKubeconfig: %v", err)
	}
	_, _, err = kc.resolve()
	if err == nil || !strings.Contains(err.Error(), "no current-context") {
		t.Fatalf("resolve err = %v, want a no-current-context refusal", err)
	}
}

func TestTLSConfigAndClientCert(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)

	cl := &clusterEntry{caData: []byte(certPEM)}
	us := &userEntry{clientCertData: certPEM, clientKeyData: keyPEM}

	cfg := tlsConfig(cl)
	if cfg.RootCAs == nil {
		t.Errorf("CA should be configured from caData")
	}
	if _, err := tlsCert(us); err != nil {
		t.Errorf("tlsCert with valid PEM: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"localhost.localdomain", true},
		{"[::1]", true},
		{"10.0.0.5", false},
		{"192.168.1.10", false},
		{"203.0.113.1", false},
		{"example.com", false},
		{"8.8.8.8", false},
	}
	for _, tc := range cases {
		if got := isLoopback(tc.host); got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestHTTPProberLocalOnlySkipsRemote(t *testing.T) {
	p, err := NewHTTPProber(kubeconfigFixtureToken(t, "https://203.0.113.1:6443"), true)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	_, err = p.Version(context.Background())
	if !errors.Is(err, ErrRemoteCluster) {
		t.Fatalf("Version err = %v, want ErrRemoteCluster", err)
	}
	if err := p.CanAdmin(context.Background()); !errors.Is(err, ErrRemoteCluster) {
		t.Fatalf("CanAdmin err = %v, want ErrRemoteCluster", err)
	}
}

func TestHTTPProberAgainstLoopbackServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.31.3+k0s.0"}`))
	})
	mux.HandleFunc("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":{"allowed":true}}`))
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"labels":{"kubernetes.io/hostname":"n1"}}},{"metadata":{"labels":{"kubernetes.io/hostname":"n2"}}}]}`))
	})
	mux.HandleFunc("/api/v1/namespaces/kube-system/configmaps/kubeadm-config", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := NewHTTPProber(kubeconfigFixtureToken(t, srv.URL), true)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	ctx := context.Background()

	v, err := p.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.GitVersion != "v1.31.3+k0s.0" {
		t.Errorf("gitVersion = %q", v.GitVersion)
	}
	if err := p.CanAdmin(ctx); err != nil {
		t.Errorf("CanAdmin: %v", err)
	}
	cl, err := p.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if cl.Nodes != 2 {
		t.Errorf("nodes = %d, want 2", cl.Nodes)
	}
	if cl.HasKubeadmConfig {
		t.Errorf("kubeadm-config should be absent")
	}
	if !reflect.DeepEqual(cl.NodeLabels, map[string]string{"kubernetes.io/hostname": "n1"}) {
		t.Errorf("node labels = %v", cl.NodeLabels)
	}
}

// selfSignedCert generates a throwaway ECDSA certificate/key for the TLS
// helpers.
func selfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

// splitLines is a tiny line splitter used only in these tests.
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func hasKeyLine(line, key string) bool {
	k, _, ok := splitKV(line)
	return ok && k == key
}
