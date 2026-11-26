package platform

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodCIDRs is the cluster's own pod network, as one CIDR per node.
//
// It is what an app that trusts a forwarded address has to be told: the peer
// that connects to it is the Gateway's proxy *pod*, whose address is in this
// network, and the connection carries the person's real address in
// X-Forwarded-For. An app asked to trust "the proxy" therefore trusts a subnet
// it cannot name from its own namespace -- the proxy's pod changes on every
// restart -- so the network it lives in is the answer.
//
// The read is the Nodes' spec.podCIDR, which is where the cluster says which
// range it hands pods: kube-controller-manager allocates it per node (kubeadm
// and therefore kind), and k0s writes it the same way. A node that names none
// is skipped; a cluster where no node names one returns an empty list, and the
// caller then leaves the app's setting alone rather than guessing a network.
//
// This is deliberately not a namespace or a Service: a NetworkPolicy peer is a
// namespace (GatewayFrom), and this is an address range. One is who may
// connect; this is whose word about the client is trusted once it does.
func PodCIDRs(ctx context.Context, kubeconfig string) ([]string, error) {
	k, err := NewCluster(kubeconfig)
	if err != nil {
		return nil, err
	}
	return k.podCIDRs(ctx)
}

func (k *Kubernetes) podCIDRs(ctx context.Context) ([]string, error) {
	nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Nodes to read the pod network: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, node := range nodes.Items {
		cidr := node.Spec.PodCIDR
		if cidr == "" || seen[cidr] {
			continue
		}
		seen[cidr] = true
		out = append(out, cidr)
	}
	sort.Strings(out)
	return out, nil
}
