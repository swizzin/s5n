package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// tlsConfig builds the API server TLS config from a kubeconfig cluster entry:
// the CA (inline data or a file), or an explicit insecure opt-in.
func tlsConfig(cl *clusterEntry) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cl == nil {
		return cfg
	}
	if cl.insecureSkipTLS {
		// The user set insecure-skip-tls-verify in their kubeconfig; honour it.
		cfg.InsecureSkipVerify = true
		return cfg
	}
	var ca []byte
	switch {
	case len(cl.caData) > 0:
		ca = cl.caData
	case cl.caFile != "":
		if b, err := os.ReadFile(cl.caFile); err == nil {
			ca = b
		}
	}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		cfg.RootCAs = pool
	}
	return cfg
}

// tlsCert builds the client certificate from a kubeconfig user entry (inline
// data or files).
func tlsCert(us *userEntry) (tls.Certificate, error) {
	var certPEM, keyPEM []byte
	switch {
	case len(us.clientCertData) > 0:
		certPEM = us.clientCertData
	case us.clientCertFile != "":
		b, err := os.ReadFile(us.clientCertFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("client certificate: %w", err)
		}
		certPEM = b
	}
	switch {
	case len(us.clientKeyData) > 0:
		keyPEM = us.clientKeyData
	case us.clientKeyFile != "":
		b, err := os.ReadFile(us.clientKeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("client key: %w", err)
		}
		keyPEM = b
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}
