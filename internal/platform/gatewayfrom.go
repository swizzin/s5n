package platform

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NamespaceNameLabel is the label Kubernetes puts on every Namespace. A
// NetworkPolicy namespaceSelector on it names one namespace without requiring
// the operator to have labelled anything.
const NamespaceNameLabel = "kubernetes.io/metadata.name"

// GatewayFrom derives the NetworkPolicy peers the seedbox's apps must admit.
//
// The chart's default (a selector on envoy-gateway-system) is only right when
// the Gateway is served by box's Envoy Gateway. Any other implementation runs
// its proxy somewhere else -- NGINX Gateway Fabric v2, for example, runs a
// data-plane pod per Gateway inside the Gateway's own namespace -- so the peer
// has to be read from the cluster, not assumed.
//
// The Gateway's address is the join: the Service whose status.loadBalancer
// holds that address is the one forwarding to the proxy, and its namespace is
// where the proxy pods run. An address that no Service holds but that is a
// node address is a host-networked proxy, which is reached by node IP. Anything
// else is refused: guessing a peer would either block the Gateway or admit a
// namespace that has nothing to do with it.
func GatewayFrom(ctx context.Context, kubeconfig string) ([]any, error) {
	k, err := NewCluster(kubeconfig)
	if err != nil {
		return nil, err
	}
	address, err := k.gatewayAddress(ctx, GatewayNamespace, GatewayName)
	if err != nil {
		return nil, err
	}
	if address == "" {
		return nil, fmt.Errorf("Gateway %s/%s reports no address, so box cannot find where its proxy runs: run `box bootstrap` first",
			GatewayNamespace, GatewayName)
	}
	services, err := k.cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Services to find the Gateway's address: %w", err)
	}
	nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Nodes to tell a host-networked Gateway: %w", err)
	}
	return peersForAddress(address, services.Items, nodes.Items)
}

// peersForAddress is the decision over already-read objects, so the cases that
// matter -- one Service, several, a node address, nothing -- are testable
// without a cluster.
func peersForAddress(address string, services []corev1.Service, nodes []corev1.Node) ([]any, error) {
	namespaces := map[string]bool{}
	for _, svc := range services {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP == address || ingress.Hostname == address {
				namespaces[svc.Namespace] = true
			}
		}
	}
	switch len(namespaces) {
	case 1:
		for ns := range namespaces {
			return []any{namespacePeer(ns)}, nil
		}
	case 0:
		for _, node := range nodes {
			for _, a := range node.Status.Addresses {
				if a.Address == address {
					// A host-networked proxy answers from the node's own IP;
					// the values' extraHttpFrom documents the same peer.
					return []any{map[string]any{
						"ipBlock": map[string]any{"cidr": address + "/32"},
					}}, nil
				}
			}
		}
		return nil, fmt.Errorf("the Gateway's address %s is not held by any LoadBalancer Service and is not a node address: box cannot tell where the Gateway's proxy runs, and will not guess the NetworkPolicy peer. Set networkPolicy.gatewayFrom by hand to the namespace the proxy runs in", address)
	default:
		list := make([]string, 0, len(namespaces))
		for ns := range namespaces {
			list = append(list, ns)
		}
		sort.Strings(list)
		return nil, fmt.Errorf("the Gateway's address %s is held by Services in several namespaces (%s): box cannot tell which one proxies for it, so it will not guess the NetworkPolicy peer",
			address, strings.Join(list, ", "))
	}
	return nil, nil
}

func namespacePeer(namespace string) map[string]any {
	return map[string]any{
		"namespaceSelector": map[string]any{
			"matchLabels": map[string]any{NamespaceNameLabel: namespace},
		},
	}
}

// gatewayAddress reads the Gateway box applied; it is the address every later
// read (and the operator's browser) uses.
func (k *Kubernetes) gatewayAddress(ctx context.Context, namespace, name string) (string, error) {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	if err != nil {
		return "", fmt.Errorf("Gateway %s/%s: %w", namespace, name, err)
	}
	obj, err := k.dyn.Resource(gvr.Resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("Gateway %s/%s does not exist: run `box bootstrap` first, because box needs the Gateway to know where its proxy runs", namespace, name)
	}
	if err != nil {
		return "", fmt.Errorf("read Gateway %s/%s: %w", namespace, name, err)
	}
	return firstAddress(obj), nil
}
