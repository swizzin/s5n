package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// kubeClient builds the HTTP client and base URL for a kubeconfig's current
// context through client-go's clientcmd, the loader kubectl itself uses. That
// keeps the prober on the same auth box's later client-go writes use: bearer
// tokens, tokenFile, client certificates, and exec credential plugins, which
// client-go runs lazily inside the transport on the first request.
//
// This is the one place bootstrap turns kubeconfig bytes into a client;
// HTTPProber and APIReadiness (k0s.go) both call it.
func kubeClient(kubeconfig []byte, timeout time.Duration) (*http.Client, string, error) {
	// clientcmd reports a kubeconfig with no current-context as "no
	// configuration has been provided", which reads as an empty file. Say what
	// is actually missing. box does not pick a context itself: the writers
	// load the same file through clientcmd and would refuse it too.
	if raw, err := clientcmd.Load(kubeconfig); err == nil && raw.CurrentContext == "" {
		return nil, "", fmt.Errorf("kubeconfig sets no current-context: select one with `kubectl config use-context`")
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	// client-go prints scheme and deprecation warnings through klog; box's
	// output stays its own.
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	client.Timeout = timeout
	return client, strings.TrimRight(cfg.Host, "/"), nil
}

// HTTPProber is the real, read-only Prober: it talks to the API server with
// the client client-go builds from the kubeconfig, so every credential
// clientcmd understands works the same way it does for box's later writes.
type HTTPProber struct {
	baseURL   string
	client    *http.Client
	localOnly bool
	// localAddrs is this machine's own interface addresses. It is a field so
	// a test can stand in a fake host instead of depending on the machine it
	// runs on; nil means net.InterfaceAddrs.
	localAddrs func() ([]net.IP, error)
}

// NewHTTPProber builds a prober for a kubeconfig's current context. When
// localOnly is set, every call first refuses to touch a server that is not
// this machine, returning ErrRemoteCluster — the --dry-run guard. Local means
// loopback or an IP literal bound to one of this host's interfaces: a local
// kubeadm cluster's admin.conf names the node's own address, not 127.0.0.1.
// The check runs before any request, so an exec credential plugin has not run
// yet either.
func NewHTTPProber(kubeconfig []byte, localOnly bool) (Prober, error) {
	client, baseURL, err := kubeClient(kubeconfig, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return &HTTPProber{baseURL: baseURL, client: client, localOnly: localOnly}, nil
}

// checkLocal returns ErrRemoteCluster when the prober is in local-only mode and
// the server is not this machine.
func (p *HTTPProber) checkLocal() error {
	if !p.localOnly {
		return nil
	}
	u, err := url.Parse(p.baseURL)
	if err != nil || u.Hostname() == "" {
		// An address box cannot parse is not one it can prove local.
		return fmt.Errorf("%w (server %q)", ErrRemoteCluster, p.baseURL)
	}
	host := u.Hostname()
	if isLocal(host, p.localAddrs) {
		return nil
	}
	return fmt.Errorf("%w (server %s)", ErrRemoteCluster, host)
}

// isLocal reports whether host is this machine: loopback, or an IP literal
// bound to one of its interfaces. A name other than localhost is never local.
// Proving a name would need a DNS lookup, and this guard must make no network
// call before it has decided.
func isLocal(host string, addrs func() ([]net.IP, error)) bool {
	if isLoopback(host) {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if addrs == nil {
		addrs = interfaceAddrs
	}
	local, err := addrs()
	if err != nil {
		// The machine's addresses cannot be read, so its own address cannot
		// be proved: refuse rather than guess.
		return false
	}
	for _, a := range local {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// interfaceAddrs is the real lookup behind isLocal: every address bound to an
// interface on this host, whatever the family.
func interfaceAddrs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			out = append(out, v.IP)
		case *net.IPAddr:
			out = append(out, v.IP)
		}
	}
	return out, nil
}

// isLoopback reports whether host names the loopback interface.
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

	classes, err := p.gatewayClasses(ctx)
	if err != nil {
		return probe, err
	}
	probe.Classes = classes
	for _, c := range classes {
		if c.Accepted {
			probe.Gateway = OutcomePresent
			break
		}
	}
	if probe.Gateway != OutcomePresent {
		probe.Gateway = OutcomeAbsent
	}

	certmanager, err := p.hasCRD(ctx, "certificates.cert-manager.io")
	if err != nil {
		return probe, err
	}
	state, err := p.certManagerState(ctx, certmanager)
	if err != nil {
		return probe, err
	}
	probe.CertManager, probe.CertManagerReason = classifyCertManager(state)

	probe.LoadBalancer = OutcomeNotProbed
	return probe, nil
}

// deploymentList is the subset of apps/v1 Deployment box reads to tell whether
// an installed cert-manager can do what s5n needs. Only the controller's
// command line, its mounted controller configuration and the webhook's
// availability matter.
type deploymentList struct {
	Items []deploymentItem `json:"items"`
}

type deploymentItem struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Volumes    []deploymentVolume    `json:"volumes"`
				Containers []deploymentContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		AvailableReplicas int                   `json:"availableReplicas"`
		Conditions        []deploymentCondition `json:"conditions"`
	} `json:"status"`
}

type deploymentVolume struct {
	Name      string `json:"name"`
	ConfigMap *struct {
		Name string `json:"name"`
	} `json:"configMap"`
}

type deploymentContainer struct {
	Name         string           `json:"name"`
	Image        string           `json:"image"`
	Args         []string         `json:"args"`
	VolumeMounts []volumeMountRef `json:"volumeMounts"`
}

type volumeMountRef struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type deploymentCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// certManagerState reads an existing cert-manager: the controller's version,
// whether its webhook is available, and whether the controller runs with
// Gateway API support. It reads flags and the controller's own configuration
// file; it never changes either.
func (p *HTTPProber) certManagerState(ctx context.Context, crd bool) (CertManagerState, error) {
	var s CertManagerState
	if !crd {
		return s, nil
	}
	s.CRD = true
	controller, err := p.deployments(ctx, "app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller")
	if err != nil {
		return s, err
	}
	if len(controller.Items) == 0 {
		return s, nil
	}
	dep := controller.Items[0]
	s.Controller = true
	s.Version = controllerVersion(dep)
	enabled, err := gatewayAPIEnabled(ctx, p, dep)
	if err != nil {
		return s, err
	}
	s.GatewayAPI = enabled

	webhook, err := p.webhookAvailable(ctx, dep.Metadata.Namespace, dep.Metadata.Name)
	if err != nil {
		return s, err
	}
	s.Webhook = webhook
	return s, nil
}

// deployments lists Deployments cluster-wide with a label selector: an
// install box did not make may live in any namespace.
func (p *HTTPProber) deployments(ctx context.Context, selector string) (deploymentList, error) {
	var out deploymentList
	path := "/apis/apps/v1/deployments?labelSelector=" + url.QueryEscape(selector)
	if err := p.get(ctx, path, &out); err != nil {
		return out, fmt.Errorf("list deployments: %w", err)
	}
	return out, nil
}

// webhookAvailable finds the webhook Deployment beside the controller and
// answers whether it is Available. The upstream chart names it
// "<release>-webhook"; a differently named one in the same namespace with the
// webhook component label is accepted too.
func (p *HTTPProber) webhookAvailable(ctx context.Context, namespace, controller string) (bool, error) {
	var list deploymentList
	path := "/apis/apps/v1/namespaces/" + namespace + "/deployments"
	if err := p.get(ctx, path, &list); err != nil {
		return false, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}
	var chosen *deploymentItem
	for i := range list.Items {
		item := &list.Items[i]
		// The chart labels the webhook app.kubernetes.io/name=webhook, not
		// cert-manager, so the name is the reliable match; a differently
		// named sibling with the webhook component label is the fallback.
		if item.Metadata.Name == controller+"-webhook" {
			chosen = item
			break
		}
		if chosen == nil && item.Metadata.Labels["app.kubernetes.io/component"] == "webhook" {
			chosen = item
		}
	}
	if chosen == nil {
		return false, nil
	}
	for _, c := range chosen.Status.Conditions {
		if c.Type == "Available" {
			return c.Status == "True", nil
		}
	}
	return chosen.Status.AvailableReplicas > 0, nil
}

// controllerVersion is the version the controller runs, from its image tag.
// The chart also labels the Deployment with app.kubernetes.io/version; that is
// the fallback for a digest-pinned image.
func controllerVersion(dep deploymentItem) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if !strings.Contains(c.Image, "cert-manager-controller") && len(dep.Spec.Template.Spec.Containers) > 1 {
			continue
		}
		if tag := imageTag(c.Image); tag != "" && tag != "latest" {
			return tag
		}
	}
	return dep.Metadata.Labels["app.kubernetes.io/version"]
}

// imageTag is the tag in an image reference, empty for a digest-only one.
func imageTag(image string) string {
	ref := image
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < slash {
		return ""
	}
	return ref[colon+1:]
}

// gatewayAPIEnabled reads whether the controller has Gateway API support on:
// the legacy boolean flag, or gatewayAPI.enabled in the controller
// configuration file the Deployment mounts. The file is the chart's
// config.gatewayAPI.enabled setting; box reads it and never rewrites it.
func gatewayAPIEnabled(ctx context.Context, p *HTTPProber, dep deploymentItem) (bool, error) {
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, arg := range c.Args {
			if arg == "--enable-gateway-api" || arg == "--enable-gateway-api=true" {
				return true, nil
			}
			if strings.HasPrefix(arg, "--enable-gateway-api=") {
				return strings.TrimPrefix(arg, "--enable-gateway-api=") == "true", nil
			}
			path, ok := strings.CutPrefix(arg, "--config=")
			if !ok || path == "" {
				continue
			}
			return pollConfigMap(ctx, p, dep, c, path)
		}
	}
	return false, nil
}

// pollConfigMap resolves --config=<path> through the container's volume mounts
// to a ConfigMap and reads the one setting. A configuration mounted some other
// way is not something box can read, so it answers false and the refusal names
// the setting.
func pollConfigMap(ctx context.Context, p *HTTPProber, dep deploymentItem, c deploymentContainer, path string) (bool, error) {
	volume := volumeForPath(c.VolumeMounts, path)
	if volume == "" {
		return false, nil
	}
	name := configMapForVolume(dep.Spec.Template.Spec.Volumes, volume)
	if name == "" {
		return false, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	apiPath := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", dep.Metadata.Namespace, name)
	if err := p.get(ctx, apiPath, &cm); err != nil {
		return false, fmt.Errorf("read controller config %s/%s: %w", dep.Metadata.Namespace, name, err)
	}
	text, ok := cm.Data[pathBase(path)]
	if !ok {
		for _, v := range cm.Data {
			text = v
			break
		}
	}
	return configEnablesGatewayAPI(text), nil
}

// volumeForPath is the volume mounted at path or at a directory above it.
func volumeForPath(mounts []volumeMountRef, path string) string {
	best := ""
	bestLen := -1
	for _, m := range mounts {
		if path == m.MountPath || strings.HasPrefix(path, strings.TrimRight(m.MountPath, "/")+"/") {
			if len(m.MountPath) > bestLen {
				best, bestLen = m.Name, len(m.MountPath)
			}
		}
	}
	return best
}

func configMapForVolume(volumes []deploymentVolume, name string) string {
	for _, v := range volumes {
		if v.Name == name && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

func pathBase(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
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

// gatewayClasses reads every GatewayClass with what box needs to choose one:
// its controller, whether it is Accepted (and why not), whether box created it,
// and the features its controller publishes. It is a read; box acts on it in
// gatewayChoice, not here.
func (p *HTTPProber) gatewayClasses(ctx context.Context) ([]GatewayClass, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				ControllerName string `json:"controllerName"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
				SupportedFeatures []struct {
					Name string `json:"name"`
				} `json:"supportedFeatures"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := p.get(ctx, "/apis/gateway.networking.k8s.io/v1/gatewayclasses", &list); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]GatewayClass, 0, len(list.Items))
	for _, item := range list.Items {
		class := GatewayClass{
			Name:           item.Metadata.Name,
			ControllerName: item.Spec.ControllerName,
			ManagedByBox:   item.Metadata.Labels["app.kubernetes.io/managed-by"] == "box",
		}
		for _, c := range item.Status.Conditions {
			if c.Type != "Accepted" {
				continue
			}
			class.Accepted = c.Status == "True"
			if !class.Accepted {
				class.AcceptedReason = c.Message
				if class.AcceptedReason == "" {
					class.AcceptedReason = c.Reason
				}
			}
		}
		for _, f := range item.Status.SupportedFeatures {
			class.Features = append(class.Features, f.Name)
		}
		out = append(out, class)
	}
	return out, nil
}
