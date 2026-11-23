package bootstrap

// inspect.go is bootstrap's gate chain asked as a question.
//
// A migration that keeps a source's cluster installs onto a cluster box did not
// install, so the questions bootstrap would ask before it uses one are the
// questions to ask before a cutover does: does /version answer, can this identity
// manage it, is the node list readable, and is it a distribution with a node count
// box supports. `box migrate cutover` asks them with this, so there is one copy of
// the rules and a cluster box refuses to bootstrap onto is a cluster it refuses to
// cut over onto.
//
// Nothing here writes: it is not a second cluster decision, it records nothing and
// it probes no capabilities beyond the gates. The platform layer is deliberately
// not probed -- a source cluster is expected to have none, and installing it is
// what the keep path is for.

import "context"

// ExistingClusterRefusal runs bootstrap's gates over a cluster that is already
// there and returns the sentence bootstrap would stop with, or "" when box may
// install onto it. path names the kubeconfig in what it says, so the operator knows
// which file answered.
func ExistingClusterRefusal(ctx context.Context, path string, kubeconfig []byte) string {
	return existingClusterRefusal(ctx, path, kubeconfig, func(kc []byte) (Prober, error) {
		return NewHTTPProber(kc, false)
	})
}

// existingClusterRefusal is the gate chain with the prober as a parameter, so a
// test answers it without an API server. inspectCluster's own refusals are what it
// reports: the wording a `box bootstrap` on this cluster would stop with.
func existingClusterRefusal(ctx context.Context, path string, kubeconfig []byte, newProber func([]byte) (Prober, error)) string {
	d := &Decision{NewProber: newProber}
	_, res, ok := d.inspectCluster(ctx, kubeconfig, path, "the cluster this source runs on")
	if ok || res.Action != ActionStop {
		return ""
	}
	return res.Reason
}
