package discover

// stops.go turns what a cutover found still running into the fewest commands that
// stop it. A person runs these by hand -- box never stops a source -- so they are
// written for the cluster box found (the `kubectl` its own tooling spells) and
// grouped: one command per namespace and kind, every name in one call, because a
// list of twelve one-object commands is a list somebody gets wrong (docs/output.md,
// rule 3).

import (
	"sort"
	"strings"
)

// Stop is one command that stops several workloads: a scale to zero, or a delete
// for the kinds that have no count to scale.
type Stop struct {
	Namespace string
	Kind      string
	Names     []string
	// Delete says this kind is deleted rather than scaled: a DaemonSet runs one
	// pod per node and a bare Pod is the pod, so neither has a replica count.
	Delete bool
}

// Stops groups what is still running into one command per namespace and kind,
// in a fixed order so two runs read the same way.
func Stops(running []Running) []Stop {
	index := map[string]*Stop{}
	var order []string
	for _, r := range running {
		key := r.Namespace + "\x00" + r.Kind
		s, ok := index[key]
		if !ok {
			s = &Stop{Namespace: r.Namespace, Kind: r.Kind}
			switch r.Kind {
			case kindDaemonSet, kindPod:
				s.Delete = true
			}
			index[key] = s
			order = append(order, key)
		}
		s.Names = append(s.Names, r.Name)
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := index[order[i]], index[order[j]]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Kind < b.Kind
	})
	out := make([]Stop, 0, len(order))
	for _, key := range order {
		s := index[key]
		sort.Strings(s.Names)
		out = append(out, *s)
	}
	return out
}

// Command is the command that runs this stop, with kubectl spelled the way this
// cluster's own tooling spells it ("kubectl" when box cannot tell).
func (s Stop) Command(kubectl string) string {
	where := ""
	if s.Namespace != "" {
		where = "-n " + s.Namespace + " "
	}
	verb := "scale " + strings.ToLower(s.Kind)
	if s.Delete {
		verb = "delete " + strings.ToLower(s.Kind)
	}
	cmd := kubectl + " " + where + verb
	if !s.Delete {
		cmd += " --replicas=0"
	}
	return cmd + " " + strings.Join(s.Names, " ")
}

// Kubectl is how this cluster's kubectl is spelled: the distribution's own
// wrapper where the source's nodes say which one installed it, and plain
// kubectl where they do not. It is printed for a person on the host to run, so
// it carries the sudo a distribution's wrapper needs.
func Kubectl(nodes []Node) string {
	switch Distribution(nodes) {
	case K0s:
		return "sudo k0s kubectl"
	case K3s:
		return "sudo k3s kubectl"
	}
	return "kubectl"
}
