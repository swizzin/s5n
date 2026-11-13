package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NodePort is one nodePort a Service holds, and the Service, as
// namespace/name.
type NodePort struct {
	Port    int
	Service string
}

// NodePorts lists every nodePort the cluster's Services hold: one LIST of
// Services in every namespace, which only a cluster-wide identity (root's) may
// do. It is how box picks a peer port nothing else has.
func NodePorts(ctx context.Context, cfg *rest.Config) ([]NodePort, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	services, err := cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Services in every namespace: %w", err)
	}
	var held []NodePort
	for _, svc := range services.Items {
		for _, port := range svc.Spec.Ports {
			if port.NodePort == 0 {
				continue
			}
			held = append(held, NodePort{Port: int(port.NodePort), Service: svc.Namespace + "/" + svc.Name})
		}
	}
	return held, nil
}
