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

// NamespaceNameLabel is the label Kubernetes puts on every Namespace, so a
// namespaceSelector can name one without the operator labelling anything.
const NamespaceNameLabel = "kubernetes.io/metadata.name"

// GatewayFrom derives the NetworkPolicy peers the seedbox's apps must admit: another
// implementation runs its proxy in its own namespace (NGINX Gateway Fabric v2 per
// Gateway), so the peer is read from the cluster, not assumed.
//
// The Gateway's address is the join: the Service whose status.loadBalancer holds it
// forwards to the proxy, and its namespace is where the proxy runs. A node address no
// Service holds is a host-networked proxy. Anything else is refused.
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

// peersForAddress is the decision over read objects, so its cases are testable without
// a cluster.
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
					// A host-networked proxy answers from the node's own IP.
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

// GatewayProxyIP is the ClusterIP in front of the Gateway's proxy pods: the address an
// app's pod uses to reach the box's own name, which resolves nowhere (docs/box.md). It is
// found by Envoy's labels, not the status address, which is empty until MetalLB picks
// one.
func GatewayProxyIP(ctx context.Context, kubeconfig string) (string, error) {
	k, err := NewCluster(kubeconfig)
	if err != nil {
		return "", err
	}
	return k.gatewayProxyIP(ctx)
}

func (k *Kubernetes) gatewayProxyIP(ctx context.Context) (string, error) {
	services, err := k.cs.CoreV1().Services(EnvoyGatewayNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=envoy,gateway.envoyproxy.io/owning-gateway-name=" + GatewayName +
			",gateway.envoyproxy.io/owning-gateway-namespace=" + GatewayNamespace,
	})
	if err != nil {
		return "", fmt.Errorf("find the Gateway's proxy Service in %s: %w", EnvoyGatewayNamespace, err)
	}
	switch len(services.Items) {
	case 1:
		ip := services.Items[0].Spec.ClusterIP
		if ip == "" || ip == "None" {
			return "", fmt.Errorf("the Gateway's proxy Service %s/%s has no ClusterIP (%q), so an app pod has no address for box's own name",
				EnvoyGatewayNamespace, services.Items[0].Name, ip)
		}
		return ip, nil
	case 0:
		// Not a refusal: an OIDC app renders without the address and the next root run
		// fills it in.
		return "", nil
	default:
		return "", fmt.Errorf("%d Services in %s carry the labels of Gateway %s/%s's proxy: box cannot tell which one answers for it",
			len(services.Items), EnvoyGatewayNamespace, GatewayNamespace, GatewayName)
	}
}

// gatewayAddress reads the Gateway box applied: the address every later read uses.
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
