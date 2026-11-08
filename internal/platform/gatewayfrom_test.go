package platform

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func loadBalancerService(namespace, name string, ingress ...corev1.LoadBalancerIngress) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: ingress}},
	}
}

func nodeWithAddresses(name string, addresses ...corev1.NodeAddress) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addresses},
	}
}

// The Envoy shape: the Gateway's address is on the proxy Service in the
// controller's namespace, which is what the chart's default already says.
func TestPeersForAddressUsesTheServiceNamespace(t *testing.T) {
	peers, err := peersForAddress("192.0.2.10",
		[]corev1.Service{loadBalancerService("envoy-gateway-system", "envoy-s5n-gateway-s5n",
			corev1.LoadBalancerIngress{IP: "192.0.2.10"})},
		[]corev1.Node{nodeWithAddresses("node", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.0.2.1"})})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"namespaceSelector": map[string]any{
			"matchLabels": map[string]any{NamespaceNameLabel: "envoy-gateway-system"},
		},
	}
	if len(peers) != 1 {
		t.Fatalf("peers = %v, want one", peers)
	}
	got := peers[0].(map[string]any)
	if got["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)[NamespaceNameLabel] != "envoy-gateway-system" {
		t.Fatalf("peer = %v, want %v", got, want)
	}
}

// NGINX Gateway Fabric v2 runs a data-plane pod per Gateway in the Gateway's
// own namespace, so the Service that holds the address is the answer -- not
// the controller's namespace, and not the chart's default.
func TestPeersForAddressFollowsAnotherImplementation(t *testing.T) {
	peers, err := peersForAddress("10.0.0.50",
		[]corev1.Service{
			loadBalancerService("nginx-gateway", "ngf-nginx-gateway-fabric"), // controller, no address
			loadBalancerService("s5n-gateway", "s5n-nginx", corev1.LoadBalancerIngress{IP: "10.0.0.50"}),
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ns := peers[0].(map[string]any)["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)[NamespaceNameLabel]
	if ns != "s5n-gateway" {
		t.Fatalf("peer namespace = %v, want the Service holding the address", ns)
	}
}

// A host-networked proxy has no LoadBalancer Service of its own: the address
// is the node's, and the peer is that node address.
func TestPeersForAddressNodeAddressIsHostNetworked(t *testing.T) {
	peers, err := peersForAddress("198.51.100.7", nil,
		[]corev1.Node{nodeWithAddresses("node", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "198.51.100.7"})})
	if err != nil {
		t.Fatal(err)
	}
	cidr := peers[0].(map[string]any)["ipBlock"].(map[string]any)["cidr"]
	if cidr != "198.51.100.7/32" {
		t.Fatalf("peer = %v, want the node address as a /32", peers[0])
	}
}

// Underivable is a refusal, not a guess: a wrong peer either blocks the
// Gateway or admits a namespace that has nothing to do with the box.
func TestPeersForAddressRefusesWhatItCannotDerive(t *testing.T) {
	_, err := peersForAddress("203.0.113.9", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "203.0.113.9") || !strings.Contains(err.Error(), "will not guess") {
		t.Fatalf("want a refusal naming the address, got %v", err)
	}

	_, err = peersForAddress("203.0.113.9", []corev1.Service{
		loadBalancerService("a", "one", corev1.LoadBalancerIngress{IP: "203.0.113.9"}),
		loadBalancerService("b", "two", corev1.LoadBalancerIngress{IP: "203.0.113.9"}),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "several namespaces") {
		t.Fatalf("want an ambiguity refusal, got %v", err)
	}
}

// A Service that is not a LoadBalancer cannot hold a Gateway address, and a
// status entry for another address is not this Gateway's.
func TestPeersForAddressIgnoresOtherServices(t *testing.T) {
	clusterIP := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "clusterip"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}}},
	}
	_, err := peersForAddress("203.0.113.9", []corev1.Service{
		clusterIP,
		loadBalancerService("lb", "other", corev1.LoadBalancerIngress{IP: "203.0.113.10"}),
	}, nil)
	if err == nil {
		t.Fatal("box derived a peer from a Service that does not hold the address")
	}
}
