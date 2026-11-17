package discover

import (
	"context"
	"fmt"
	"sort"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
)

// Cluster is a Source over a live source cluster. It only ever lists: no object
// it reads is written, no Secret is read at all, and the client is built the way
// every other host-side box client is (internal/kube), so a source kubeconfig
// with an exec credential or a token file works as it does under kubectl.
type Cluster struct {
	cs         kubernetes.Interface
	namespaces []string

	mu     sync.Mutex
	loaded bool
	wls    []Workload
	svcs   []Service
	ings   []Ingress
	pods   []Pod
	nodes  []string
	raw    map[string][]byte
	err    error
	// nodeErr is why the nodes could not be read, kept apart from err: a
	// namespace-scoped kubeconfig may not list a cluster-scoped object, and
	// that is the plan's host missing rather than the read failing.
	nodeErr error
}

// NewCluster builds the source client for one kubeconfig.
//
// namespaces limits every list to those namespaces, one call each; empty lists
// across all namespaces, which needs cluster-wide read access. A namespace-
// scoped kubeconfig -- a user's, where root's is not -- can only read the
// namespaces it names, so the same list is Options.Namespaces.
func NewCluster(kubeconfig string, namespaces []string) (*Cluster, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig: discovery reads a source cluster through one")
	}
	cfg, err := kube.Config(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	return NewClusterFor(cs, namespaces), nil
}

// NewClusterFor is NewCluster over an existing clientset, which is what the
// tests drive.
func NewClusterFor(cs kubernetes.Interface, namespaces []string) *Cluster {
	return &Cluster{cs: cs, namespaces: namespaces}
}

// Workloads is the source's pod templates nothing owns above them: its
// Deployments, StatefulSets and DaemonSets, the ReplicaSets no Deployment made
// and the Pods no workload made.
func (c *Cluster) Workloads(ctx context.Context) ([]Workload, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.wls, nil
}

// Services is the source's Services.
func (c *Cluster) Services(ctx context.Context) ([]Service, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.svcs, nil
}

// Ingresses is the source's Ingress rules.
func (c *Cluster) Ingresses(ctx context.Context) ([]Ingress, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.ings, nil
}

// Pods is every pod the source still holds.
func (c *Cluster) Pods(ctx context.Context) ([]Pod, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.pods, nil
}

// Nodes is the source cluster's node names, sorted. An error here is not the
// read failing: see Source.
func (c *Cluster) Nodes(ctx context.Context) ([]string, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.nodes, c.nodeErr
}

// Raw is every object read, as the plan directory keeps it.
func (c *Cluster) Raw(ctx context.Context) (map[string][]byte, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.raw, nil
}

// load reads the source once, whichever of the questions comes first.
func (c *Cluster) load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.err
	}
	c.loaded = true
	c.err = c.read(ctx)
	return c.err
}

func (c *Cluster) read(ctx context.Context) error {
	raw := map[string][]byte{}
	var (
		deploys    []*appsv1.Deployment
		stss       []*appsv1.StatefulSet
		daemonsets []*appsv1.DaemonSet
		// sets and barePods are the objects nothing owns: a ReplicaSet a
		// Deployment made, and a pod any workload made, are read through their
		// template and never as a workload of their own.
		sets     []*appsv1.ReplicaSet
		barePods []*corev1.Pod
		pods     []*corev1.Pod
		svcs     []*corev1.Service
		ings     []*networkingv1.Ingress
	)
	for _, ns := range c.scope() {
		list, err := c.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list deployments in %s: %w", where(ns), err)
		}
		for i := range list.Items {
			deploys = append(deploys, &list.Items[i])
		}
		setsList, err := c.cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list statefulsets in %s: %w", where(ns), err)
		}
		for i := range setsList.Items {
			stss = append(stss, &setsList.Items[i])
		}
		dsList, err := c.cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list daemonsets in %s: %w", where(ns), err)
		}
		for i := range dsList.Items {
			daemonsets = append(daemonsets, &dsList.Items[i])
		}
		rsList, err := c.cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list replicasets in %s: %w", where(ns), err)
		}
		for i := range rsList.Items {
			if ownerless(rsList.Items[i].OwnerReferences) {
				sets = append(sets, &rsList.Items[i])
			}
		}
		podList, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list pods in %s: %w", where(ns), err)
		}
		for i := range podList.Items {
			pod := &podList.Items[i]
			pods = append(pods, pod)
			if ownerless(pod.OwnerReferences) {
				barePods = append(barePods, pod)
			}
		}
		services, err := c.cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list services in %s: %w", where(ns), err)
		}
		for i := range services.Items {
			svcs = append(svcs, &services.Items[i])
		}
		ingresses, err := c.cs.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list ingresses in %s: %w", where(ns), err)
		}
		for i := range ingresses.Items {
			ings = append(ings, &ingresses.Items[i])
		}
	}
	pvcs, pvs, err := c.claims(ctx, deploys, stss, daemonsets, sets, barePods)
	if err != nil {
		return err
	}
	nodes, nodeErr := c.nodeList(ctx)
	for _, o := range []struct {
		apiVersion, kind string
		objs             []any
	}{
		{appsV1, "Deployment", anySlice(deploys)},
		{appsV1, "StatefulSet", anySlice(stss)},
		{appsV1, "DaemonSet", anySlice(daemonsets)},
		{appsV1, "ReplicaSet", anySlice(sets)},
		{"v1", "Pod", anySlice(barePods)},
		{"v1", "Service", anySlice(svcs)},
		{networkingV1, "Ingress", anySlice(ings)},
		{"v1", "PersistentVolumeClaim", anySlice(pvcs)},
		{"v1", "PersistentVolume", anySlice(pvs)},
		{"v1", "Node", anySlice(nodes)},
	} {
		if err := record(raw, o.apiVersion, o.kind, o.objs); err != nil {
			return err
		}
	}

	claims := indexClaims(pvcs, pvs)
	c.wls = workloadsOf(deploys, stss, daemonsets, sets, barePods, claims)
	c.svcs = servicesOf(svcs)
	c.ings = ingressesOf(ings)
	c.pods = podsOf(pods)
	if nodeErr == nil {
		c.nodes = nodeNames(nodes)
	}
	c.nodeErr = nodeErr
	c.raw = raw
	return nil
}

// nodeList is the cluster's nodes. A source read through a namespace-scoped
// kubeconfig cannot list them, which is a plan without a host and not a read
// that failed.
func (c *Cluster) nodeList(ctx context.Context) ([]*corev1.Node, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	nodes := make([]*corev1.Node, 0, len(list.Items))
	for i := range list.Items {
		nodes = append(nodes, &list.Items[i])
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// scope is what every list is asked for: the namespaces discovery was told
// about, or the empty string, which client-go reads as all of them.
func (c *Cluster) scope() []string {
	if len(c.namespaces) == 0 {
		return []string{""}
	}
	return c.namespaces
}

// where names a list's scope in a refusal.
func where(namespace string) string {
	if namespace == "" {
		return "every namespace"
	}
	return namespace
}

// claims reads the claims and volumes the source's mounts go through, and only
// when a mount goes through one: a source whose apps all use hostPaths does not
// need a permission it may not have.
func (c *Cluster) claims(ctx context.Context, deploys []*appsv1.Deployment, stss []*appsv1.StatefulSet, daemonsets []*appsv1.DaemonSet, sets []*appsv1.ReplicaSet, pods []*corev1.Pod) (pvcs []*corev1.PersistentVolumeClaim, pvs []*corev1.PersistentVolume, err error) {
	var specs []corev1.PodSpec
	for _, d := range deploys {
		specs = append(specs, d.Spec.Template.Spec)
	}
	for _, s := range stss {
		specs = append(specs, s.Spec.Template.Spec)
	}
	for _, ds := range daemonsets {
		specs = append(specs, ds.Spec.Template.Spec)
	}
	for _, rs := range sets {
		specs = append(specs, rs.Spec.Template.Spec)
	}
	for _, pod := range pods {
		specs = append(specs, pod.Spec)
	}
	wanted := false
	for _, spec := range specs {
		for _, v := range spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				wanted = true
			}
		}
	}
	if !wanted {
		return nil, nil, nil
	}
	for _, ns := range c.scope() {
		list, err := c.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list persistentvolumeclaims in %s: %w", where(ns), err)
		}
		for i := range list.Items {
			pvcs = append(pvcs, &list.Items[i])
		}
	}
	volumes, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list persistentvolumes: %w", err)
	}
	for i := range volumes.Items {
		pvs = append(pvs, &volumes.Items[i])
	}
	return pvcs, pvs, nil
}
