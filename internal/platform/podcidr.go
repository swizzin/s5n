package platform

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodCIDRs is the cluster's pod network, one CIDR per node.
//
// It is what an app trusting a forwarded address must be told: the peer that connects
// is the Gateway's proxy *pod*, which changes on every restart, and the connection
// carries the person's real address in X-Forwarded-For, so the network it lives in is
// the only stable answer.
//
// The read is the Nodes' spec.podCIDR, where the cluster says which range it hands
// pods (kube-controller-manager, and k0s the same way). A node naming none is skipped,
// and a cluster where none does returns an empty list, so the caller leaves the app's
// setting alone rather than guessing.
//
// Not a namespace or a Service: a NetworkPolicy peer is a namespace (GatewayFrom) and
// this is an address range -- who may connect, against whose word is trusted.
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
