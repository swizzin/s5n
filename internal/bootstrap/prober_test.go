package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// kubeconfigFixtureToken is a token-based kubeconfig for the read-only HTTP
// prober. Served over TLS in the auth tests: clientcmd deliberately sends no
// credentials to a plain http server, as kubectl does.
func kubeconfigFixtureToken(t *testing.T, server string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
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

// kubeconfigWithUser writes the same single-cluster/context shape, with the
// user block spelled exactly as a test needs it. The cluster skips TLS
// verification so an httptest TLS server needs no CA fixture.
func kubeconfigWithUser(server, userBody string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
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
%s
`, server, userBody))
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

func TestHTTPProberSendsBearerToken(t *testing.T) {
	const token = "abc123"
	want := "Bearer " + token
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			if got != "" {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"gitVersion":"v1.37.2+k0s.1"}`))
	})
	mux.HandleFunc("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			if got != "" {
				t.Errorf("SSAR Authorization = %q, want %q", got, want)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":{"allowed":true}}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	// The same server answers 401 when the header is absent; that is the
	// failure main exhibits, where the prober never set it.
	raw := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := raw.Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request without the header = %d, want 401", resp.StatusCode)
	}

	p, err := NewHTTPProber(kubeconfigFixtureToken(t, srv.URL), false)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	ctx := context.Background()
	if _, err := p.Version(ctx); err != nil {
		t.Fatalf("Version with a token kubeconfig: %v", err)
	}
	if err := p.CanAdmin(ctx); err != nil {
		t.Fatalf("CanAdmin with a token kubeconfig: %v", err)
	}
}

func TestHTTPProberSendsClientCertAndToken(t *testing.T) {
	const token = "abc123"
	certPEM, keyPEM := selfSignedCert(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+token)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("the request carried no client certificate")
			http.Error(w, "no client certificate", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"gitVersion":"v1.37.2+k0s.1"}`))
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	body := fmt.Sprintf("    client-certificate-data: %s\n    client-key-data: %s\n    token: %s\n",
		base64.StdEncoding.EncodeToString(certPEM),
		base64.StdEncoding.EncodeToString(keyPEM),
		token)
	p, err := NewHTTPProber(kubeconfigWithUser(srv.URL, body), false)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	if _, err := p.Version(context.Background()); err != nil {
		t.Fatalf("Version with a client certificate and a token: %v", err)
	}
}

func TestHTTPProberReadsTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer file-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer file-token")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"gitVersion":"v1.37.2+k0s.1"}`))
	}))
	defer srv.Close()

	p, err := NewHTTPProber(kubeconfigWithUser(srv.URL, "    tokenFile: "+path+"\n"), false)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	if _, err := p.Version(context.Background()); err != nil {
		t.Fatalf("Version with a tokenFile kubeconfig: %v", err)
	}
}

// writeExecCredScript writes a stand-in exec credential plugin: it records
// that it ran, then prints one v1beta1 ExecCredential carrying a fixed token.
// Nothing in box may run it except client-go, on an actual request.
func writeExecCredScript(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cred.sh")
	script := fmt.Sprintf(`#!/bin/sh
: > %q
printf '%%s\n' '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"exec-token"}}'
`, marker)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write exec credential script: %v", err)
	}
	return path
}

func execKubeconfigWithUser(server, script string) []byte {
	return kubeconfigWithUser(server, "    exec:\n      apiVersion: client.authentication.k8s.io/v1beta1\n      command: "+script+"\n      interactiveMode: Never\n")
}

func TestHTTPProberRunsExecCredentialPluginForLocalServer(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeExecCredScript(t, marker)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer exec-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer exec-token")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"gitVersion":"v1.37.2+k0s.1"}`))
	}))
	defer srv.Close()

	p, err := NewHTTPProber(execKubeconfigWithUser(srv.URL, script), true)
	if err != nil {
		t.Fatalf("NewHTTPProber with an exec credential plugin: %v", err)
	}
	if _, err := p.Version(context.Background()); err != nil {
		t.Fatalf("Version with an exec credential plugin: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the exec credential plugin did not run: %v", err)
	}
}

func TestHTTPProberLocalOnlyNeverRunsExecPluginForRemote(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeExecCredScript(t, marker)
	p, err := NewHTTPProber(execKubeconfigWithUser("https://10.0.0.1:6443", script), true)
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	if _, err := p.Version(context.Background()); !errors.Is(err, ErrRemoteCluster) {
		t.Fatalf("Version = %v, want ErrRemoteCluster", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the exec credential plugin ran for a remote host (marker stat: %v)", err)
	}
}

func TestHTTPProberCheckLocal(t *testing.T) {
	local := []string{
		"https://[::1]:6443",
		"https://[::1]",
		"https://127.0.0.1:6443",
		"https://localhost:6443",
	}
	for _, server := range local {
		p := &HTTPProber{baseURL: server, localOnly: true}
		if err := p.checkLocal(); err != nil {
			t.Errorf("checkLocal(%q) = %v, want local", server, err)
		}
	}
	remote := []string{
		"https://10.0.0.1:6443",
		"https://example.com:6443",
		"https://[2001:db8::1]:6443",
	}
	for _, server := range remote {
		p := &HTTPProber{baseURL: server, localOnly: true}
		if err := p.checkLocal(); !errors.Is(err, ErrRemoteCluster) {
			t.Errorf("checkLocal(%q) = %v, want ErrRemoteCluster", server, err)
		}
	}
}

func TestHTTPProberAgainstLoopbackServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.37.2+k0s.1"}`))
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
	if v.GitVersion != "v1.37.2+k0s.1" {
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

// selfSignedCert generates a throwaway ECDSA certificate/key for the client
// certificate test.
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
