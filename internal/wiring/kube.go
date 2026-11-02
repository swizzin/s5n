package wiring

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Publisher writes the receipt into the release's wiring ConfigMap. It talks to
// the API server with the Pod's own service account: the Job's RBAC is a single
// namespaced Role on that one object, so a compromised Job cannot reach anything
// else in the cluster.
type Publisher struct {
	BaseURL   string
	Namespace string
	Token     string
	HTTP      *http.Client
}

// InCluster builds a Publisher from the mounted service account.
func InCluster() (*Publisher, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT are unset")
	}
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster CA %s is not a certificate", serviceAccountDir+"/ca.crt")
	}
	ns, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("read namespace: %w", err)
	}
	return &Publisher{
		BaseURL:   "https://" + host + ":" + port,
		Namespace: strings.TrimSpace(string(ns)),
		Token:     strings.TrimSpace(string(token)),
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

type configMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
}

// Upsert sets keys on the ConfigMap, creating it when absent. Existing keys the
// caller did not mention (the chart's own wiring.json) are preserved: the Job
// shares the object with the chart and must not blank it.
func (p *Publisher) Upsert(ctx context.Context, name string, data map[string]string) error {
	path := fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps/%s", p.BaseURL, p.Namespace, name)
	var cur configMap
	err := p.do(ctx, http.MethodGet, path, nil, &cur)
	switch {
	case err == nil:
		if cur.Data == nil {
			cur.Data = map[string]string{}
		}
		for k, v := range data {
			cur.Data[k] = v
		}
		cur.Metadata["name"] = name
		cur.Metadata["namespace"] = p.Namespace
		return p.do(ctx, http.MethodPut, path, cur, nil)
	case isStatus(err, http.StatusNotFound):
		obj := configMap{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Metadata:   map[string]any{"name": name, "namespace": p.Namespace},
			Data:       data,
		}
		listPath := fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps", p.BaseURL, p.Namespace)
		return p.do(ctx, http.MethodPost, listPath, obj, nil)
	default:
		return err
	}
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	body := strings.Join(strings.Fields(e.Body), " ")
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("kubernetes API: %d: %s", e.Status, body)
}

func isStatus(err error, status int) bool {
	ae, ok := err.(*apiError)
	return ok && ae.Status == status
}

func (p *Publisher) do(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
