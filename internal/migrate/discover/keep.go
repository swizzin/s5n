package discover

// keep.go is the keep path's pre-flight: what a source cluster has that stops box
// installing onto it.
//
// Keeping the source's cluster is the default cutover because it destroys
// nothing, so the one thing it must not do is half-take a cluster somebody else
// runs. Three things on a source stop it, and box never changes a cluster it did
// not install: it names each with the command to look at it (box's advice is
// always one to double-check) and the operator decides.
//
//   - a Service holding the node's address on port 80 or 443, or a container with
//     hostPort 80/443: that is an ingress controller (or whatever else serves the
//     source's front door), and box's Gateway needs those ports on the same node;
//   - an auto-updater watching the source's namespaces (keel, by its annotation or
//     the Deployment itself): it would rewrite the release box installs, which is
//     the one thing a kept cluster must not do to box's own apps;
//   - a cluster bootstrap's own gates refuse (a managed control plane, more than
//     one node, an API that does not answer): that is bootstrap's answer, asked
//     through bootstrap, not a second copy of the rules.
//
// The last one is asked by the caller, which holds the kubeconfig; this file reads
// objects.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Blocker is one thing about a source cluster that stops box installing onto it.
type Blocker struct {
	// What is one word for the kind of blocker: "ports", "auto-updater" or
	// "cluster".
	What string
	// Where is the object it is about, <namespace>/<name>, or the cluster itself.
	Where string
	// Why is one line saying what it would do to the install.
	Why string
	// Check is the command that shows it, for the operator to double-check:
	// box does not change a cluster it did not install.
	Check string
}

// String is one line, the way the run lists it.
func (b Blocker) String() string {
	return fmt.Sprintf("%s: %s (check: %s)", b.Where, b.Why, b.Check)
}

// Blockers is the keep path's pre-flight over a live source. Empty is a cluster
// box can install onto; anything else is a reason to stop after the final snapshot
// and to prefer `--replace-cluster`.
func (c *Cluster) Blockers(ctx context.Context) ([]Blocker, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.blockers, nil
}

// blockersOf is every blocker one read of a source holds, in one order so two runs
// over one cluster read the same way.
func blockersOf(objs readObjects) []Blocker {
	var out []Blocker
	out = append(out, portBlockers(objs)...)
	out = append(out, updaterBlockers(objs)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].What < out[j].What
	})
	return out
}

// theirFrontDoor reports whether a port is one box's Gateway needs: 80 for the
// redirect every host gets, 443 for the certificate.
func theirFrontDoor(port int32) bool { return port == 80 || port == 443 }

// portBlockers is every Service that already answers on the node's port 80 or 443,
// and every container that binds one on the host. A Service of type NodePort or
// LoadBalancer is the cluster's own front door whatever runs behind it, and a
// hostPort is the same thing written into a pod.
func portBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, svc := range objs.svcs {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer && svc.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		for _, p := range svc.Spec.Ports {
			if !theirFrontDoor(p.Port) {
				continue
			}
			out = append(out, Blocker{
				What:  "ports",
				Where: svc.Namespace + "/" + svc.Name,
				Why: fmt.Sprintf("this %s Service answers on %d, and box's Gateway needs the node's 80 and 443",
					svc.Spec.Type, p.Port),
				Check: fmt.Sprintf("kubectl -n %s get service %s -o wide", svc.Namespace, svc.Name),
			})
			break
		}
	}
	for _, pod := range objs.pods {
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if !theirFrontDoor(p.HostPort) {
					continue
				}
				out = append(out, Blocker{
					What:  "ports",
					Where: pod.Namespace + "/" + pod.Name,
					Why:   fmt.Sprintf("%s binds hostPort %d, and box's Gateway needs the node's 80 and 443", c.Name, p.HostPort),
					Check: fmt.Sprintf("kubectl -n %s get pod %s -o jsonpath='{.spec.containers[*].ports}'", pod.Namespace, pod.Name),
				})
			}
		}
	}
	return out
}

// keelAnnotation is what an auto-updater is configured by: keel watches every
// object carrying it and rewrites that object's image when the registry moves.
const keelAnnotation = "keel.sh/policy"

// updaterBlockers is every auto-updater watching the source's namespaces. Keel
// marks what it watches with an annotation and runs as a Deployment, and either is
// enough: an updater that rewrote the release box installs would be a box whose
// images nobody chose.
func updaterBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, wl := range updaterCandidates(objs) {
		policy := wl.annotations[keelAnnotation]
		if policy != "" {
			out = append(out, Blocker{
				What:  "auto-updater",
				Where: wl.namespace + "/" + wl.name,
				Why:   fmt.Sprintf("%s is %q, so keel would rewrite this workload's image under the release box installs", keelAnnotation, policy),
				Check: fmt.Sprintf("kubectl -n %s get %s %s -o yaml", wl.namespace, wl.kind, wl.name),
			})
		}
		for _, image := range wl.images {
			if !isKeel(image) {
				continue
			}
			out = append(out, Blocker{
				What:  "auto-updater",
				Where: wl.namespace + "/" + wl.name,
				Why:   "keel is running in this namespace, and it would rewrite the release box installs",
				Check: fmt.Sprintf("kubectl -n %s get %s %s -o wide", wl.namespace, wl.kind, wl.name),
			})
			break
		}
	}
	return out
}

// isKeel reports whether an image is keel. The name is the whole test: keel is
// published under more than one registry, and a repository whose last element is
// "keel" is the one programme.
func isKeel(image string) bool {
	repo := image
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	if i := strings.IndexAny(repo, "@:"); i >= 0 {
		repo = repo[:i]
	}
	return repo == "keel"
}

// updater is one object an auto-updater could be watching or running as.
type updater struct {
	namespace   string
	name        string
	kind        string
	annotations map[string]string
	images      []string
}

// updaterCandidates is every workload a read holds, with what an updater is
// configured by: the object's own annotations, and the images its pod runs. It is
// the objects, not the pods they made -- an updater annotates the workload, and a
// pod carries its owner's annotation second-hand.
func updaterCandidates(objs readObjects) []updater {
	var out []updater
	add := func(namespace, name, kind string, annotations map[string]string, spec corev1.PodSpec) {
		u := updater{namespace: namespace, name: name, kind: kind, annotations: annotations}
		for _, c := range spec.Containers {
			u.images = append(u.images, c.Image)
		}
		out = append(out, u)
	}
	for _, d := range objs.deploys {
		add(d.Namespace, d.Name, "deployment", d.Annotations, d.Spec.Template.Spec)
	}
	for _, s := range objs.stss {
		add(s.Namespace, s.Name, "statefulset", s.Annotations, s.Spec.Template.Spec)
	}
	for _, ds := range objs.daemonsets {
		add(ds.Namespace, ds.Name, "daemonset", ds.Annotations, ds.Spec.Template.Spec)
	}
	for _, rs := range objs.sets {
		add(rs.Namespace, rs.Name, "replicaset", rs.Annotations, rs.Spec.Template.Spec)
	}
	for _, pod := range objs.barePods {
		add(pod.Namespace, pod.Name, "pod", pod.Annotations, pod.Spec)
	}
	return out
}

// ClusterBlocker is the third blocker, the one only the caller can ask: bootstrap's
// own gates over this cluster, whose sentence it fills in here so a cluster blocker
// reads like the other two.
func ClusterBlocker(refusal string) Blocker {
	return Blocker{
		What:  "cluster",
		Where: "the cluster itself",
		Why:   refusal,
		Check: "kubectl get nodes -o wide, with the source's kubeconfig",
	}
}
