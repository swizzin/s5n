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

	"k8s.io/client-go/tools/clientcmd"

	"github.com/swizzin/s5n/internal/kube"
)

// kubeClient builds the HTTP client and base URL for a kubeconfig's current
// context through clientcmd, the loader kubectl uses, so the prober runs on the same
// auth as box's writes (tokens, tokenFile, certificates, exec plugins run lazily on
// the first request). HTTPProber and APIReadiness (k0s.go) both call it.
func kubeClient(kubeconfig []byte, timeout time.Duration) (*http.Client, string, error) {
	// clientcmd calls a kubeconfig with no current-context "no configuration has
	// been provided", which reads as an empty file. Say what is missing; box never
	// picks a context itself.
	if raw, err := clientcmd.Load(kubeconfig); err == nil && raw.CurrentContext == "" {
		return nil, "", fmt.Errorf("kubeconfig sets no current-context: select one with `kubectl config use-context`")
	}
	// The same config, with warnings silenced, that every writer builds.
	return kube.ProbeClient(kubeconfig, timeout)
}

// HTTPProber is the read-only Prober: the API server through client-go, so every
// credential clientcmd understands works as it does for box's writes.
type HTTPProber struct {
	baseURL   string
	client    *http.Client
	localOnly bool
	// localAddrs is this machine's addresses, a field so a test stands in a fake
	// host; nil means net.InterfaceAddrs.
	localAddrs func() ([]net.IP, error)
}

// NewHTTPProber builds a prober for a kubeconfig's current context. With localOnly,
// every call first refuses a server that is not this machine, returning
// ErrRemoteCluster -- the --dry-run guard. Local is loopback or an IP literal on one
// of this host's interfaces (a kubeadm admin.conf names the node's own address), and
// the check runs before any request, so no exec plugin has run either.
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

// isLocal reports whether host is this machine: loopback, or an IP literal on one of
// its interfaces. Any other name is never local -- proving one needs a DNS lookup,
// and this guard makes no network call before it decides.
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
		// Its own address cannot be proved: refuse rather than guess.
		return false
	}
	for _, a := range local {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// interfaceAddrs is the real lookup: every address on this host, whatever the family.
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

// adminAccess is what installing needs: the cluster-scoped writes the platform layer
// makes through Helm. Each is an apply, because a re-run patches what the first run
// created, and a create-only identity would fail mid-install.
func adminAccess() []kube.Access {
	return []kube.Access{
		{Verb: kube.VerbApply, Resource: "namespaces", What: "namespaces"},
		{Verb: kube.VerbApply, Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", What: "custom resource definitions"},
		{Verb: kube.VerbApply, Group: "rbac.authorization.k8s.io", Resource: "clusterroles", What: "cluster roles"},
	}
}

// CanAdmin checks, one SelfSubjectAccessReview per privilege, that the identity can
// manage the cluster. The questions and verbs are kube's; the sentence is
// bootstrap's, because this goes into a skip reason and not a failed write.
func (p *HTTPProber) CanAdmin(ctx context.Context) error {
	denied, err := kube.Denied(ctx, p, adminAccess())
	if err != nil {
		return fmt.Errorf("SelfSubjectAccessReview: %w", err)
	}
	if len(denied) > 0 {
		return fmt.Errorf("identity cannot %s", strings.Join(denied, ", "))
	}
	return nil
}

// Allowed answers one SelfSubjectAccessReview (kube.Reviewer). The prober posts it
// itself rather than building a typed client: a kubeconfig this read-only client can
// use must not be refused by a client it cannot build.
func (p *HTTPProber) Allowed(ctx context.Context, a kube.Access) (bool, error) {
	return p.ssar(ctx, a)
}

func (p *HTTPProber) ssar(ctx context.Context, a kube.Access) (bool, error) {
	req := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{
			"resourceAttributes": map[string]any{
				"namespace": a.Namespace,
				"verb":      a.Verb,
				"group":     a.Group,
				"resource":  a.Resource,
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

// Describe reads the node count, InternalIPs, the first node's labels, and whether a
// kubeadm-config ConfigMap exists.
func (p *HTTPProber) Describe(ctx context.Context) (Cluster, error) {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := p.get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return Cluster{}, fmt.Errorf("list nodes: %w", err)
	}
	cl := Cluster{Nodes: len(nodes.Items)}
	for _, item := range nodes.Items {
		for _, addr := range item.Status.Addresses {
			if addr.Type == "InternalIP" {
				cl.NodeIPs = append(cl.NodeIPs, addr.Address)
			}
		}
	}
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

// Platform runs the read-only capability probes. LoadBalancer asks whether box's own
// Gateway got an address, and that Gateway is written after the confirm, so it
// reports not-probed.
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

// deploymentList is the part of an apps/v1 Deployment box reads: the controller's
// command line, its mounted configuration and the webhook's availability.
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

// certManagerState reads an existing cert-manager: the controller's version, the
// webhook's availability and whether Gateway API support is on, from flags and the
// controller's own configuration. It changes nothing.
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

// deployments lists Deployments cluster-wide by label: an install box did not make
// may live anywhere.
func (p *HTTPProber) deployments(ctx context.Context, selector string) (deploymentList, error) {
	var out deploymentList
	path := "/apis/apps/v1/deployments?labelSelector=" + url.QueryEscape(selector)
	if err := p.get(ctx, path, &out); err != nil {
		return out, fmt.Errorf("list deployments: %w", err)
	}
	return out, nil
}

// webhookAvailable finds the webhook Deployment beside the controller and answers
// whether it is Available: "<release>-webhook", or a sibling with the webhook
// component label.
func (p *HTTPProber) webhookAvailable(ctx context.Context, namespace, controller string) (bool, error) {
	var list deploymentList
	path := "/apis/apps/v1/namespaces/" + namespace + "/deployments"
	if err := p.get(ctx, path, &list); err != nil {
		return false, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}
	var chosen *deploymentItem
	for i := range list.Items {
		item := &list.Items[i]
		// The chart labels the webhook name=webhook, not cert-manager, so the name is
		// the reliable match; the component label is the fallback.
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
			return c.Status == conditionTrue, nil
		}
	}
	return chosen.Status.AvailableReplicas > 0, nil
}

// controllerVersion is the version the controller runs, from its image tag, with
// the Deployment's app.kubernetes.io/version label for a digest-pinned image.
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

// gatewayAPIEnabled reads whether the controller has Gateway API support: the legacy
// boolean flag, or gatewayAPI.enabled in the mounted configuration (the chart's
// config.gatewayAPI.enabled). box reads it and never rewrites it.
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

// pollConfigMap resolves --config=<path> through the volume mounts to a ConfigMap and
// reads the one setting. A configuration mounted another way cannot be read, so it
// answers false and the refusal names the setting.
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

// gatewayClasses reads every GatewayClass with what choosing one needs: controller,
// Accepted (and why not), whether box created it, its published features. Box acts on
// it in gatewayChoice.
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
			ManagedByBox:   item.Metadata.Labels[kube.ManagedByLabel] == kube.ManagedBy,
		}
		for _, c := range item.Status.Conditions {
			if c.Type != "Accepted" {
				continue
			}
			class.Accepted = c.Status == conditionTrue
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
