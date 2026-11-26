package bootstrap

// inspect.go is bootstrap's gate chain asked as a question.
//
// A migration that keeps a source's cluster installs onto a cluster box did not
// install, so the questions bootstrap asks before using one are a cutover's: does
// /version answer, can this identity manage it, is the node list readable, is it a
// supported distribution and node count. `box migrate cutover` asks them here, so one
// copy of the rules serves both.
//
// Nothing here writes or probes capabilities: a source cluster is expected to have no
// platform layer, and installing it is what the keep path is for.

import "context"

// ExistingClusterRefusal runs bootstrap's gates over a cluster already there and
// returns the sentence bootstrap would stop with, or "": path names the kubeconfig
// that answered.
func ExistingClusterRefusal(ctx context.Context, path string, kubeconfig []byte) string {
	return existingClusterRefusal(ctx, path, kubeconfig, func(kc []byte) (Prober, error) {
		return NewHTTPProber(kc, false)
	})
}

// existingClusterRefusal is the chain with the prober as a parameter, so a test needs
// no API server: inspectCluster's own refusals are what it reports.
func existingClusterRefusal(ctx context.Context, path string, kubeconfig []byte, newProber func([]byte) (Prober, error)) string {
	d := &Decision{NewProber: newProber}
	_, res, ok := d.inspectCluster(ctx, kubeconfig, path, "the cluster this source runs on")
	if ok || res.Action != ActionStop {
		return ""
	}
	return res.Reason
}
