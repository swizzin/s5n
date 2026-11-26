package discover

// keep.go is the keep path's pre-flight. Keeping the source's cluster destroys
// nothing, so it must never half-take a cluster someone else runs: box names each
// blocker with a command to check it and never changes what it did not install.
// Four block: an ingress or anything holding node ports 80/443, an auto-updater
// (keel) that would rewrite box's release, a GitOps controller that would put the
// source back, and bootstrap's own gates (a managed control plane, several nodes,
// a dead API), which the caller asks through bootstrap.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Blockers is the pre-flight; empty means box can install. Node ports and an
// updater's pod belong to the whole cluster, so they are listed across every
// namespace; only annotations come from the scoped read.
func (c *Cluster) Blockers(ctx context.Context) ([]Blocker, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	wide, err := c.readWide(ctx)
	if err != nil {
		return nil, err
	}
	out := append([]Blocker(nil), c.watched...)
	out = append(out, portBlockers(wide)...)
	out = append(out, keelBlockers(wide)...)
	out = append(out, gitopsBlockers(wide)...)
	sortBlockers(out)
	return out, nil
}

// readWide lists what the cluster-wide questions need, across every namespace. A
// kubeconfig that cannot list them is a refusal: a pre-flight that saw half the
// cluster would be a guess.
func (c *Cluster) readWide(ctx context.Context) (readObjects, error) {
	var objs readObjects
	svcs, err := c.cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return objs, fmt.Errorf("list services in every namespace, for what holds the node's 80 and 443: %w", err)
	}
	for i := range svcs.Items {
		objs.svcs = append(objs.svcs, &svcs.Items[i])
	}
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return objs, fmt.Errorf("list pods in every namespace, for what holds the node's 80 and 443: %w", err)
	}
	for i := range pods.Items {
		objs.pods = append(objs.pods, &pods.Items[i])
	}
	deploys, err := c.cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return objs, fmt.Errorf("list deployments in every namespace, for an auto-updater: %w", err)
	}
	for i := range deploys.Items {
		objs.deploys = append(objs.deploys, &deploys.Items[i])
	}
	stss, err := c.cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return objs, fmt.Errorf("list statefulsets in every namespace, for an auto-updater: %w", err)
	}
	for i := range stss.Items {
		objs.stss = append(objs.stss, &stss.Items[i])
	}
	daemonsets, err := c.cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return objs, fmt.Errorf("list daemonsets in every namespace, for an auto-updater: %w", err)
	}
	for i := range daemonsets.Items {
		objs.daemonsets = append(objs.daemonsets, &daemonsets.Items[i])
	}
	return objs, nil
}

// sortBlockers puts blockers in one order, so two runs over one cluster read the
func sortBlockers(out []Blocker) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].What < out[j].What
	})
}

// theirFrontDoor reports whether a port is one box's Gateway needs: 80 for the
func theirFrontDoor(port int32) bool { return port == 80 || port == 443 }

// portBlockers is everything that already holds the node's port 80 or 443, one
// NodePort Service's in-cluster port is not the node's: its node port is.
func portBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, svc := range objs.svcs {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer && svc.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		for _, p := range svc.Spec.Ports {
			var why string
			switch {
			case svc.Spec.Type == corev1.ServiceTypeLoadBalancer && theirFrontDoor(p.Port):
				why = fmt.Sprintf("this LoadBalancer Service answers on %d", p.Port)
			case theirFrontDoor(p.NodePort):
				why = fmt.Sprintf("this %s Service holds the node's port %d", svc.Spec.Type, p.NodePort)
			default:
				continue
			}
			out = append(out, Blocker{
				What:  "ports",
				Where: svc.Namespace + "/" + svc.Name,
				Why:   why + ", and box's Gateway needs the node's 80 and 443",
				Check: fmt.Sprintf("kubectl -n %s get service %s -o wide", svc.Namespace, svc.Name),
			})
			break
		}
	}
	for _, pod := range objs.pods {
		if b, ok := podPortBlocker(pod); ok {
			out = append(out, b)
		}
	}
	return out
}

// podPortBlocker is the one line a pod gets when one of its containers holds the
func podPortBlocker(pod *corev1.Pod) (Blocker, bool) {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			var why string
			switch {
			case theirFrontDoor(p.HostPort):
				why = fmt.Sprintf("%s binds hostPort %d", c.Name, p.HostPort)
			case pod.Spec.HostNetwork && theirFrontDoor(p.ContainerPort):
				why = fmt.Sprintf("%s listens on %d on the node's own network", c.Name, p.ContainerPort)
			default:
				continue
			}
			return Blocker{
				What:  "ports",
				Where: pod.Namespace + "/" + pod.Name,
				Why:   why + ", and box's Gateway needs the node's 80 and 443",
				Check: fmt.Sprintf("kubectl -n %s get pod %s -o wide", pod.Namespace, pod.Name),
			}, true
		}
	}
	return Blocker{}, false
}

// keelAnnotation is what an auto-updater is configured by: keel watches every
// object carrying it and rewrites that object's image when the registry moves.
const keelAnnotation = "keel.sh/policy"

// watchedBlockers is every workload in the source's namespaces that keel is told
// to watch, or that a GitOps controller manages. Read from the scoped lists: a workload outside the source is not this
func watchedBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, wl := range updaterCandidates(objs) {
		if b, ok := gitopsMark(wl); ok {
			out = append(out, b)
		}
		policy := wl.annotations[keelAnnotation]
		if policy == "" {
			continue
		}
		out = append(out, Blocker{
			What:  "auto-updater",
			Where: wl.namespace + "/" + wl.name,
			Why:   fmt.Sprintf("%s is %q, so keel would rewrite this workload's image under the release box installs", keelAnnotation, policy),
			Check: fmt.Sprintf("kubectl -n %s get %s %s -o yaml", wl.namespace, wl.kind, wl.name),
		})
	}
	return out
}

// keelBlockers is keel itself, wherever it runs: it usually has a namespace of its
// own, and an updater that rewrote the release box installs would be a box whose
func keelBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, wl := range updaterCandidates(objs) {
		for _, image := range wl.images {
			if !isKeel(image) {
				continue
			}
			out = append(out, Blocker{
				What:  "auto-updater",
				Where: wl.namespace + "/" + wl.name,
				Why:   "keel is running on this cluster, and it would rewrite the release box installs",
				Check: fmt.Sprintf("kubectl -n %s get %s %s -o wide", wl.namespace, wl.kind, wl.name),
			})
			break
		}
	}
	return out
}

// gitopsWhy is what a GitOps controller does to a kept cluster: it puts back what
// it manages, and a source stopped by hand is the first thing it puts back.
const gitopsWhy = "it would scale a stopped source back up beside box"

// The GitOps controllers by the name the operator knows them by.
const (
	flux   = "Flux"
	argoCD = "Argo CD"
)

// gitopsMarks are the marks Flux and Argo CD leave on what they manage: Flux
// labels every object a Kustomization or HelmRelease applies, and Argo CD tracks
// by its own label or annotation. Helm's app.kubernetes.io/instance is not one:
// Argo CD can track by it, but so does every plain Helm release, and a source
// installed with helm is not managed by anything that will put it back.
var gitopsMarks = []struct{ key, who string }{
	{"kustomize.toolkit.fluxcd.io/name", flux},
	{"helm.toolkit.fluxcd.io/name", flux},
	{"argocd.argoproj.io/instance", argoCD},
	{"argocd.argoproj.io/tracking-id", argoCD},
}

// gitopsMark is the blocker a source workload gets when it carries a GitOps
func gitopsMark(wl updater) (Blocker, bool) {
	for _, m := range gitopsMarks {
		v, ok := wl.labels[m.key]
		if !ok {
			v, ok = wl.annotations[m.key]
		}
		if !ok {
			continue
		}
		return Blocker{
			What:  "gitops",
			Where: wl.namespace + "/" + wl.name,
			Why:   fmt.Sprintf("%s manages this workload (%s=%s), and %s", m.who, m.key, v, gitopsWhy),
			Check: fmt.Sprintf("kubectl -n %s get %s %s -o yaml", wl.namespace, wl.kind, wl.name),
		}, true
	}
	return Blocker{}, false
}

// fluxControllers are the workloads that do the putting back, by the repository
// their image comes from (Flux publishes one image per controller) or, for Argo
// CD, whose one image runs every component, by the component's own name.
var fluxControllers = map[string]bool{"kustomize-controller": true, "helm-controller": true}

// gitopsController names the GitOps controller a workload is, if it is one.
func gitopsController(wl updater) string {
	if wl.labels["app.kubernetes.io/name"] == "argocd-application-controller" || wl.name == "argocd-application-controller" {
		return argoCD
	}
	for _, image := range wl.images {
		if strings.Contains(image, "fluxcd/") && fluxControllers[imageName(image)] {
			return flux
		}
	}
	return ""
}

// gitopsBlockers is every GitOps controller on the cluster, wherever it runs: one
func gitopsBlockers(objs readObjects) []Blocker {
	var out []Blocker
	for _, wl := range updaterCandidates(objs) {
		who := gitopsController(wl)
		if who == "" {
			continue
		}
		out = append(out, Blocker{
			What:  "gitops",
			Where: wl.namespace + "/" + wl.name,
			Why:   fmt.Sprintf("%s runs on this cluster, and %s", who, gitopsWhy),
			Check: fmt.Sprintf("kubectl -n %s get %s %s -o wide", wl.namespace, wl.kind, wl.name),
		})
	}
	return out
}

// imageName is an image's last path element without its tag or digest.
func imageName(image string) string {
	repo := image
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	if i := strings.IndexAny(repo, "@:"); i >= 0 {
		repo = repo[:i]
	}
	return repo
}

// isKeel reports whether an image is keel. The name is the whole test: keel is
func isKeel(image string) bool {
	return imageName(image) == "keel"
}

// updater is one object an auto-updater could be watching or running as.
type updater struct {
	namespace   string
	name        string
	kind        string
	labels      map[string]string
	annotations map[string]string
	images      []string
}

// updaterCandidates is every workload a read holds, with what an updater is
// runs. It is the objects, not the pods they made -- an updater marks the
func updaterCandidates(objs readObjects) []updater {
	var out []updater
	add := func(meta metav1.ObjectMeta, kind string, spec corev1.PodSpec) {
		u := updater{namespace: meta.Namespace, name: meta.Name, kind: kind, labels: meta.Labels, annotations: meta.Annotations}
		for _, c := range spec.Containers {
			u.images = append(u.images, c.Image)
		}
		out = append(out, u)
	}
	for _, d := range objs.deploys {
		add(d.ObjectMeta, "deployment", d.Spec.Template.Spec)
	}
	for _, s := range objs.stss {
		add(s.ObjectMeta, "statefulset", s.Spec.Template.Spec)
	}
	for _, ds := range objs.daemonsets {
		add(ds.ObjectMeta, "daemonset", ds.Spec.Template.Spec)
	}
	for _, rs := range objs.sets {
		add(rs.ObjectMeta, "replicaset", rs.Spec.Template.Spec)
	}
	for _, pod := range objs.barePods {
		add(pod.ObjectMeta, "pod", pod.Spec)
	}
	return out
}

// ClusterBlocker is the third blocker, the one only the caller can ask: bootstrap's
func ClusterBlocker(refusal string) Blocker {
	return Blocker{
		What:  "cluster",
		Where: "the cluster itself",
		Why:   refusal,
		Check: "kubectl get nodes -o wide, with the source's kubeconfig",
	}
}
