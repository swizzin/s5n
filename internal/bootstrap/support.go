package bootstrap

import "fmt"

// supportProblem reports why box refuses this cluster, or "" when it is one box
// supports. One seedbox on one node is the design: the data contract is hardlinks on
// one filesystem, the peers port and the Gateway share that node's address, and box
// backs up, inspects and repairs one host. A managed control plane has no node box
// owns, and several nodes break those guarantees, so both are refused by name.
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
