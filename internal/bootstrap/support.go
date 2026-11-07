package bootstrap

import "fmt"

// supportProblem reports why box refuses to drive this cluster, or "" when the
// cluster is one v1 supports. v1 runs one seedbox on one node: the data
// contract is hardlinks on one filesystem, the peers port and the Gateway share
// that node's one address, and box backs up, inspects and repairs one host. A
// managed control plane has no node box owns at all, and a cluster of several
// nodes breaks those guarantees, so both are refused by name rather than
// accepted and half-configured.
func supportProblem(dist Distribution, nodes int) string {
	switch dist {
	case DistEKS, DistGKE:
		return fmt.Sprintf("%s: managed clusters are not supported in v1 — there is no node box owns, so the hostPath data path with hardlinks, the peers port with real source addresses and the one host box backs up have nowhere to live; use the chart with helm install instead", dist)
	}
	switch {
	case nodes == 0:
		return "the cluster lists no nodes, so nothing can run there; check that the kubeconfig reaches the right cluster and its nodes have registered"
	case nodes > 1:
		return fmt.Sprintf("%d nodes: box runs one seedbox on one node — hardlinks need the data on one filesystem, the peers port shares that node's address, and box backs up one host; use a single-node cluster", nodes)
	}
	return ""
}
