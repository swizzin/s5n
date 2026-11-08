package bootstrap

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// kubeconfig is the subset of a kubeconfig file the real prober needs: enough
// to resolve the current context to a server and its TLS material. It is
// parsed from the machine-generated files k0s/k3s/kubeadm/kubectl write, not
// from arbitrary YAML, so exec credential plugins are out of scope here.
type kubeconfig struct {
	currentContext string
	clusters       map[string]*clusterEntry
	contexts       map[string]*contextEntry
	users          map[string]*userEntry
}

type clusterEntry struct {
	server          string
	caData          []byte
	caFile          string
	insecureSkipTLS bool
}

type contextEntry struct {
	cluster string
	user    string
}

type userEntry struct {
	clientCertData []byte
	clientKeyData  []byte
	clientCertFile string
	clientKeyFile  string
	token          string
}

// parseKubeconfig reads the fixed shape machine-generated kubeconfigs use:
//
//	clusters:
//	- cluster:
//	    certificate-authority-data: <b64>
//	    server: https://…
//	  name: foo
//	contexts:
//	- context:
//	    cluster: foo
//	    user: bar
//	  name: ctx
//	users:
//	- name: bar
//	  user:
//	    client-certificate-data: <b64>
//	    client-key-data: <b64>
func parseKubeconfig(data []byte) (*kubeconfig, error) {
	kc := &kubeconfig{
		clusters: map[string]*clusterEntry{},
		contexts: map[string]*contextEntry{},
		users:    map[string]*userEntry{},
	}
	section := ""
	var (
		item        string // "cluster", "context", "user"
		pendingName string
		cluster     *clusterEntry
		context     *contextEntry
		user        *userEntry
	)
	commit := func() {
		if pendingName == "" {
			return
		}
		switch item {
		case "cluster":
			if cluster != nil {
				kc.clusters[pendingName] = cluster
			}
		case "context":
			if context != nil {
				kc.contexts[pendingName] = context
			}
		case "user":
			if user != nil {
				kc.users[pendingName] = user
			}
		}
	}
	lines := strings.Split(string(data), "\n")
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			if strings.HasPrefix(trimmed, "- ") {
				commit()
				pendingName = ""
				cluster, context, user = nil, nil, nil
				body := strings.TrimSpace(trimmed[2:])
				key, val, _ := splitKV(body)
				switch key {
				case "cluster":
					item, cluster = "cluster", &clusterEntry{}
				case "context":
					item, context = "context", &contextEntry{}
				case "name":
					if section == "users" {
						item, user = "user", &userEntry{}
						pendingName = unquote(val)
					}
				}
				continue
			}
			commit()
			pendingName = ""
			cluster, context, user = nil, nil, nil
			key, val, ok := splitKV(trimmed)
			if !ok {
				continue
			}
			switch key {
			case "current-context":
				kc.currentContext = unquote(val)
			case "clusters", "contexts", "users", "preferences":
				section = key
			default:
				section = ""
			}
			continue
		}
		// indented: belongs to the current list item.
		key, val, ok := splitKV(trimmed)
		if !ok {
			continue
		}
		switch section {
		case "clusters":
			if item != "cluster" || cluster == nil {
				continue
			}
			switch key {
			case "name":
				pendingName = unquote(val)
			case "server":
				cluster.server = unquote(val)
			case "certificate-authority-data":
				cluster.caData = decodeB64(val)
			case "certificate-authority":
				cluster.caFile = unquote(val)
			case "insecure-skip-tls-verify":
				cluster.insecureSkipTLS = val == "true"
			}
		case "contexts":
			if item != "context" || context == nil {
				continue
			}
			switch key {
			case "name":
				pendingName = unquote(val)
			case "cluster":
				context.cluster = unquote(val)
			case "user":
				context.user = unquote(val)
			}
		case "users":
			if item != "user" || user == nil {
				continue
			}
			switch key {
			case "client-certificate-data":
				user.clientCertData = decodeB64(val)
			case "client-key-data":
				user.clientKeyData = decodeB64(val)
			case "client-certificate":
				user.clientCertFile = unquote(val)
			case "client-key":
				user.clientKeyFile = unquote(val)
			case "token":
				user.token = unquote(val)
			}
		}
	}
	commit()
	return kc, nil
}

func decodeB64(val string) []byte {
	val = unquote(strings.TrimSpace(val))
	b, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return nil
	}
	return b
}

// resolve returns the server URL and TLS/auth material for the current context.
func (k *kubeconfig) resolve() (*clusterEntry, *userEntry, error) {
	ctxName := k.currentContext
	if ctxName == "" {
		switch len(k.contexts) {
		case 0:
			return nil, nil, fmt.Errorf("kubeconfig: no contexts")
		case 1:
			for name := range k.contexts {
				ctxName = name
			}
		default:
			return nil, nil, fmt.Errorf("kubeconfig: no current-context and %d contexts; set one to select a cluster", len(k.contexts))
		}
	}
	ctx, ok := k.contexts[ctxName]
	if !ok {
		return nil, nil, fmt.Errorf("kubeconfig: context %q not found", ctxName)
	}
	cl, ok := k.clusters[ctx.cluster]
	if !ok {
		return nil, nil, fmt.Errorf("kubeconfig: cluster %q not found", ctx.cluster)
	}
	var us *userEntry
	if ctx.user != "" {
		u, ok := k.users[ctx.user]
		if !ok {
			return nil, nil, fmt.Errorf("kubeconfig: user %q not found", ctx.user)
		}
		us = u
	}
	return cl, us, nil
}

// HTTPProber is the real, read-only Prober: it talks to the API server over
// plain stdlib HTTP, the same way internal/wiring does in-cluster, so box adds
// no client-go dependency.
type HTTPProber struct {
	baseURL   string
	client    *http.Client
	localOnly bool
}

// NewHTTPProber parses a kubeconfig and builds a prober for its current
// context. When localOnly is set, every call first refuses to touch a server
// that is not loopback, returning ErrRemoteCluster — the --dry-run guard.
func NewHTTPProber(kubeconfig []byte, localOnly bool) (Prober, error) {
	kc, err := parseKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cl, us, err := kc.resolve()
	if err != nil {
		return nil, err
	}
	transport, err := buildTransport(cl, us)
	if err != nil {
		return nil, err
	}
	return &HTTPProber{
		baseURL:   strings.TrimRight(cl.server, "/"),
		client:    &http.Client{Timeout: 20 * time.Second, Transport: transport},
		localOnly: localOnly,
	}, nil
}

func buildTransport(cl *clusterEntry, us *userEntry) (*http.Transport, error) {
	tlsCfg := tlsConfig(cl)
	if us != nil && (len(us.clientCertData) > 0 || us.clientCertFile != "") {
		cert, err := tlsCert(us)
		if err != nil {
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Transport{TLSClientConfig: tlsCfg}, nil
}

// checkLocal returns ErrRemoteCluster when the prober is in local-only mode and
// the server is not loopback.
func (p *HTTPProber) checkLocal() error {
	if !p.localOnly {
		return nil
	}
	u := p.baseURL
	host := u
	if i := strings.Index(u, "://"); i >= 0 {
		host = u[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if isLoopback(host) {
		return nil
	}
	return fmt.Errorf("%w (server %s)", ErrRemoteCluster, host)
}

// isLoopback reports whether host names this machine unambiguously.
func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "localhost.localdomain" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func (p *HTTPProber) get(ctx context.Context, path string, out any) error {
	if err := p.checkLocal(); err != nil {
		return err
	}
	return p.do(ctx, http.MethodGet, path, nil, out)
}

func (p *HTTPProber) post(ctx context.Context, path string, body, out any) error {
	if err := p.checkLocal(); err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, path, body, out)
}

func (p *HTTPProber) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
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

// Version reads /version.
func (p *HTTPProber) Version(ctx context.Context) (Version, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := p.get(ctx, "/version", &v); err != nil {
		return Version{}, err
	}
	return Version{GitVersion: v.GitVersion}, nil
}

// CanAdmin checks, with one SelfSubjectAccessReview per privilege, that the
// identity can create namespaces, CRDs and cluster roles.
func (p *HTTPProber) CanAdmin(ctx context.Context) error {
	checks := []struct {
		name, resource, group string
	}{
		{"create namespaces", "namespaces", ""},
		{"create customresourcedefinitions", "customresourcedefinitions", "apiextensions.k8s.io"},
		{"create clusterroles", "clusterroles", "rbac.authorization.k8s.io"},
	}
	var denied []string
	for _, c := range checks {
		allowed, err := p.ssar(ctx, c.resource, c.group, "create")
		if err != nil {
			return fmt.Errorf("SelfSubjectAccessReview: %w", err)
		}
		if !allowed {
			denied = append(denied, c.name)
		}
	}
	if len(denied) > 0 {
		return fmt.Errorf("identity cannot %s", strings.Join(denied, ", "))
	}
	return nil
}

func (p *HTTPProber) ssar(ctx context.Context, resource, group, verb string) (bool, error) {
	req := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{
			"resourceAttributes": map[string]any{
				"namespace": "",
				"verb":      verb,
				"group":     group,
				"resource":  resource,
			},
		},
	}
	var out struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := p.post(ctx, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", req, &out); err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

// Describe reads the node count, the first node's labels, and whether a
// kubeadm-config ConfigMap exists.
func (p *HTTPProber) Describe(ctx context.Context) (Cluster, error) {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := p.get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return Cluster{}, fmt.Errorf("list nodes: %w", err)
	}
	cl := Cluster{Nodes: len(nodes.Items)}
	if len(nodes.Items) > 0 {
		cl.NodeLabels = nodes.Items[0].Metadata.Labels
	}
	_, err := p.getKubeadmConfig(ctx)
	if err != nil {
		if errors.Is(err, errNotFound) {
			cl.HasKubeadmConfig = false
		} else {
			return Cluster{}, fmt.Errorf("kubeadm-config: %w", err)
		}
	} else {
		cl.HasKubeadmConfig = true
	}
	return cl, nil
}

var errNotFound = errors.New("not found")

func (p *HTTPProber) getKubeadmConfig(ctx context.Context) (any, error) {
	var out any
	err := p.get(ctx, "/api/v1/namespaces/kube-system/configmaps/kubeadm-config", &out)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, err
	}
	return out, nil
}

// Platform runs the read-only subset of the capability probes. The
// LoadBalancer probe asks whether box's own Gateway got an address, and that
// Gateway is written after the confirm, so it is never run here and reports
// not-probed.
func (p *HTTPProber) Platform(ctx context.Context) (PlatformProbe, error) {
	var probe PlatformProbe

	gatewayAPI, err := p.hasCRD(ctx, "gatewayclasses.gateway.networking.k8s.io")
	if err != nil {
		return probe, err
	}
	httproutes, err := p.hasCRD(ctx, "httproutes.gateway.networking.k8s.io")
	if err != nil {
		return probe, err
	}
	if gatewayAPI && httproutes {
		probe.GatewayAPI = OutcomePresent
	} else {
		probe.GatewayAPI = OutcomeAbsent
	}

	accepted, err := p.gatewayClassAccepted(ctx)
	if err != nil {
		return probe, err
	}
	if accepted {
		probe.Gateway = OutcomePresent
	} else {
		probe.Gateway = OutcomeAbsent
	}

	certmanager, err := p.hasCRD(ctx, "certificates.cert-manager.io")
	if err != nil {
		return probe, err
	}
	if certmanager {
		probe.CertManager = OutcomePresent
	} else {
		probe.CertManager = OutcomeAbsent
	}

	probe.LoadBalancer = OutcomeNotProbed
	return probe, nil
}

func (p *HTTPProber) hasCRD(ctx context.Context, name string) (bool, error) {
	var out any
	err := p.get(ctx, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+name, &out)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *HTTPProber) gatewayClassAccepted(ctx context.Context) (bool, error) {
	var list struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := p.get(ctx, "/apis/gateway.networking.k8s.io/v1/gatewayclasses", &list); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, item := range list.Items {
		for _, c := range item.Status.Conditions {
			if c.Type == "Accepted" && c.Status == "True" {
				return true, nil
			}
		}
	}
	return false, nil
}
